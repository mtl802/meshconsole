package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

const regToken = "test-reg-token-0123456789abcdef"

func newTestHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	return newTestHandlerWithTokens(t, config.RegistrationToken{Token: regToken})
}

func newTestHandlerWithTokens(t *testing.T, tokens ...config.RegistrationToken) (*Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), tokens, nil, nil), st
}

func doReq(t *testing.T, h *Handler, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	// 模拟回环来源：生产 agent 恒来自 tailnet/私网（默认允许网段）；
	// httptest 默认 192.0.2.1 属 TEST-NET，会被来源收敛正确拒绝。
	req.RemoteAddr = "127.0.0.1:40000"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// doRawReq 以原始字符串为 body 发请求（构造尾部多余数据等协议违规）。
func doRawReq(t *testing.T, h *Handler, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:40000"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return m
}

func registerBody(name string) map[string]string {
	return map[string]string{"name": name, "role": "workstation", "os": "darwin", "arch": "arm64"}
}

// mustRegister 走正常流程注册并返回节点 token 与节点 ID。
func mustRegister(t *testing.T, h *Handler, name string) (string, int64) {
	t.Helper()
	return mustRegisterWithToken(t, h, name, regToken)
}

// mustRegisterWithToken 以指定注册 token 注册。注册 token 一次性（消耗即失效）
// ——多节点测试各节点须用独立 token（newTestHandlerWithTokens 注入）。
func mustRegisterWithToken(t *testing.T, h *Handler, name, token string) (string, int64) {
	t.Helper()
	rec := doReq(t, h, "/api/agent/register", token, registerBody(name))
	if rec.Code != http.StatusOK {
		t.Fatalf("register %s = %d body=%s", name, rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	tok, _ := resp["node_token"].(string)
	id := int64(resp["node_id"].(float64))
	return tok, id
}

func TestRegisterAuth(t *testing.T) {
	h, st := newTestHandler(t)

	// 无 token / 错 token：统一 401，无原因细节。
	for _, bearer := range []string{"", "wrong-token"} {
		rec := doReq(t, h, "/api/agent/register", bearer, registerBody("n1"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bearer=%q code = %d, want 401", bearer, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "token") || strings.Contains(rec.Body.String(), "reason") {
			t.Fatalf("401 leaks detail: %s", rec.Body.String())
		}
	}

	// 正确 token：200 并签发节点 token。
	rec := doReq(t, h, "/api/agent/register", regToken, registerBody("mac-mini"))
	if rec.Code != http.StatusOK {
		t.Fatalf("register code = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	tok, _ := resp["node_token"].(string)
	if len(tok) != 64 { // 32B hex
		t.Fatalf("node_token length = %d, want 64", len(tok))
	}
	node, err := st.GetNodeByName(t.Context(), "mac-mini")
	if err != nil || node == nil {
		t.Fatalf("node row missing: %v", err)
	}
	if node.Status != "online" || node.OS != "darwin" || node.Arch != "arm64" {
		t.Fatalf("unexpected node row: %+v", node)
	}
	// 库里只存 SHA-256 哈希，不存明文。
	if node.TokenHash == tok || len(node.TokenHash) != 64 {
		t.Fatalf("token_hash wrong: %q", node.TokenHash)
	}
}

func TestRegisterTokenSingleUse(t *testing.T) {
	h, _ := newTestHandler(t)

	if rec := doReq(t, h, "/api/agent/register", regToken, registerBody("a")); rec.Code != http.StatusOK {
		t.Fatalf("first register = %d", rec.Code)
	}
	// 同 token 第二次（即使换名）：401（用后即废）。
	// 消费校验先于重名检查：即使 b 未被占用也必须 401，不可借 409 探测节点名。
	rec := doReq(t, h, "/api/agent/register", regToken, registerBody("b"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused token = %d, want 401", rec.Code)
	}
	// 已消费 token + 已占用节点名：仍 401（不得因重名先行返回 409 泄露名字存在性）。
	rec = doReq(t, h, "/api/agent/register", regToken, registerBody("a"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("consumed token + dup name = %d, want 401", rec.Code)
	}
}

func TestRegisterDuplicateName(t *testing.T) {
	h, _ := newTestHandler(t)
	mustRegister(t, h, "dup")

	// 追加第二个可用 token（未消费），重名注册 → 409。
	h.regTokens = append(h.regTokens, regTokenEntry{hash: []byte(tokenHashHex("second-token-0123456789ab"))})
	rec := doReq(t, h, "/api/agent/register", "second-token-0123456789ab", registerBody("dup"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name = %d, want 409", rec.Code)
	}
	// 409 路径不得烧掉第二个 token：换名后仍可正常注册。
	rec = doReq(t, h, "/api/agent/register", "second-token-0123456789ab", registerBody("fresh"))
	if rec.Code != http.StatusOK {
		t.Fatalf("register after 409 = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegisterTokenExpired(t *testing.T) {
	expired := config.RegistrationToken{
		Token:     "expired-token-0123456789ab",
		ExpiresAt: config.FlexTime{Time: time.Now().Add(-time.Hour)},
	}
	h, _ := newTestHandlerWithTokens(t, expired)
	rec := doReq(t, h, "/api/agent/register", expired.Token, registerBody("n1"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d, want 401", rec.Code)
	}
}

func TestRegisterTokenBinding(t *testing.T) {
	bound := config.RegistrationToken{Token: regToken, ExpectedNode: "mac-mini"}
	h, _ := newTestHandlerWithTokens(t, bound)

	// 绑定预期节点：其他节点名一律 401（不泄露 token 有效性）。
	rec := doReq(t, h, "/api/agent/register", regToken, registerBody("other-box"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unbound name = %d, want 401", rec.Code)
	}
	// 绑定名单内的节点名：正常注册。
	rec = doReq(t, h, "/api/agent/register", regToken, registerBody("mac-mini"))
	if rec.Code != http.StatusOK {
		t.Fatalf("bound name = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegisterBadBody(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/agent/register", strings.NewReader("{not json"))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("Authorization", "Bearer "+regToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400", rec.Code)
	}

	// 空名 / 超长名 / 带空白的名。
	for _, name := range []string{"", strings.Repeat("x", 65), "bad name"} {
		rec := doReq(t, h, "/api/agent/register", regToken, registerBody(name))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name=%q code = %d, want 400", name, rec.Code)
		}
	}

	// 尾部多余数据（未到 EOF）：400（审查 R1-#8）。
	raw := `{"name":"n1","role":"","os":"","arch":""} {"trailing":1}`
	if rec := doRawReq(t, h, "/api/agent/register", regToken, raw); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing data = %d, want 400", rec.Code)
	}
	if rec := doRawReq(t, h, "/api/agent/register", regToken, `{"name":"n1","role":"","os":"","arch":}junk`); rec.Code != http.StatusBadRequest {
		t.Fatalf("junk suffix = %d, want 400", rec.Code)
	}
}

func heartbeatBody(node string, withErrors bool) map[string]any {
	mem := int64(2048)
	body := map[string]any{
		"node":          node,
		"agent_version": "v0.1.0",
		"metrics": map[string]any{
			"cpu_pct": 33.3, "mem_used": mem, "mem_total": mem,
			"disk_used": nil, "disk_total": nil, "net_rx": nil, "net_tx": nil,
			"uptime_s": 100, "load1": nil, // 磁盘/网络/load 采集失败 → null
		},
	}
	if withErrors {
		body["collect_errors"] = map[string]string{
			"load1": "not implemented on windows",
			"disk":  "permission denied",
		}
	}
	return body
}

func TestHeartbeatFlow(t *testing.T) {
	h, st := newTestHandler(t)
	tokA, nodeID := mustRegister(t, h, "node-a")

	// 无 token / 坏 token → 401。
	for _, bearer := range []string{"", "bad-token"} {
		rec := doReq(t, h, "/api/agent/heartbeat", bearer, heartbeatBody("node-a", false))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bearer=%q heartbeat = %d, want 401", bearer, rec.Code)
		}
	}

	// 跨节点：token 是 node-a 的，body 却报 node-b → 403。
	rec := doReq(t, h, "/api/agent/heartbeat", tokA, heartbeatBody("node-b", false))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-node heartbeat = %d, want 403", rec.Code)
	}

	// 正常心跳 → 200，落库。
	rec = doReq(t, h, "/api/agent/heartbeat", tokA, heartbeatBody("node-a", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d body=%s", rec.Code, rec.Body.String())
	}
	if n, err := st.CountMetrics(t.Context(), nodeID); err != nil || n != 1 {
		t.Fatalf("metrics count = %d err=%v, want 1", n, err)
	}
	node, _ := st.GetNodeByName(t.Context(), "node-a")
	if !node.LastSuccess.Valid {
		t.Fatal("last_success should be set on clean heartbeat")
	}
	if node.AgentVersion != "v0.1.0" || node.Status != "online" {
		t.Fatalf("unexpected node state: %+v", node)
	}

	// 带采集错误的心跳：接受并落库，但 last_success 不刷新（双时间戳语义）。
	prevSuccess := node.LastSuccess.Int64
	time.Sleep(1100 * time.Millisecond)
	rec = doReq(t, h, "/api/agent/heartbeat", tokA, heartbeatBody("node-a", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat(errors) = %d", rec.Code)
	}
	node, _ = st.GetNodeByName(t.Context(), "node-a")
	if node.LastSuccess.Int64 != prevSuccess {
		t.Fatal("last_success must not advance when collect_errors present")
	}
	if node.LastSeen.Int64 <= prevSuccess {
		t.Fatal("last_seen should refresh even with collect errors")
	}
	if n, _ := st.CountMetrics(t.Context(), nodeID); n != 2 {
		t.Fatalf("metrics count = %d, want 2", n)
	}

	// null 字段入库保持 NULL，collect_errors 已记录。
	recRow, err := st.LatestMetrics(t.Context(), nodeID)
	if err != nil || recRow == nil {
		t.Fatalf("latest metrics: %v", err)
	}
	if recRow.DiskUsed.Valid || recRow.Load1.Valid || recRow.NetRx.Valid {
		t.Fatalf("null fields became values: %+v", recRow)
	}
	if !strings.Contains(recRow.CollectErrors, "load1") {
		t.Fatalf("collect_errors not stored: %q", recRow.CollectErrors)
	}
}

// TestHeartbeatValueRange 值域校验（审查 R1-#9）：越界值整条拒绝，不入库。
func TestHeartbeatValueRange(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	cases := []map[string]any{
		{"cpu_pct": 150.0},  // cpu 上界
		{"cpu_pct": -0.5},   // cpu 下界
		{"mem_used": -1},    // 负字节
		{"net_tx": -100},    // 负累计字节
		{"uptime_s": -5},    // 负 uptime
		{"load1": -1.0},     // 负 load
		{"disk_total": -10}, // 负磁盘总量
	}
	for i, over := range cases {
		body := heartbeatBody("n1", false)
		metrics := body["metrics"].(map[string]any)
		for k, v := range over {
			metrics[k] = v
		}
		rec := doReq(t, h, "/api/agent/heartbeat", tok, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("case %d %v = %d, want 400", i, over, rec.Code)
		}
	}
	if n, _ := st.CountMetrics(t.Context(), nodeID); n != 0 {
		t.Fatalf("rejected heartbeats must not store rows, got %d", n)
	}

	// 合法边界值：0 值可接受（0 是合法测量值，仅禁止冒充采集失败）。
	body := heartbeatBody("n1", false)
	metrics := body["metrics"].(map[string]any)
	metrics["cpu_pct"] = 0.0
	metrics["net_rx"] = 0
	rec := doReq(t, h, "/api/agent/heartbeat", tok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("boundary zero values = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHeartbeatEmptyMetricsRejected 全空指标且无采集错误说明 → 400（R3）：
// 正常 agent 心跳必带指标或错误说明；全空但带说明是合法心跳（全体采集失败）。
func TestHeartbeatEmptyMetricsRejected(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	allNull := map[string]any{
		"cpu_pct": nil, "mem_used": nil, "mem_total": nil, "disk_used": nil,
		"disk_total": nil, "net_rx": nil, "net_tx": nil, "uptime_s": nil, "load1": nil,
	}
	// 全空 + 无说明：400，不落库。
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, map[string]any{
		"node": "n1", "agent_version": "v0.1.0", "metrics": allNull,
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty metrics without errors = %d, want 400", rec.Code)
	}
	// metrics 字段整个缺省同样算全空：400。
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, map[string]any{
		"node": "n1", "agent_version": "v0.1.0",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing metrics = %d, want 400", rec.Code)
	}
	if n, _ := st.CountMetrics(t.Context(), nodeID); n != 0 {
		t.Fatalf("rejected heartbeats must not store rows, got %d", n)
	}

	// 全空但带错误说明：200（全体采集失败是合法心跳），且 last_success 不刷新。
	rec := doReq(t, h, "/api/agent/heartbeat", tok, map[string]any{
		"node": "n1", "agent_version": "v0.1.0", "metrics": allNull,
		"collect_errors": map[string]string{"cpu_pct": "collector crashed"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty metrics with errors = %d body=%s", rec.Code, rec.Body.String())
	}
	node, _ := st.GetNodeByName(t.Context(), "n1")
	if node.LastSuccess.Valid {
		t.Fatal("last_success must stay unset for all-failed heartbeat")
	}
	if n, _ := st.CountMetrics(t.Context(), nodeID); n != 1 {
		t.Fatalf("metrics count = %d, want 1", n)
	}
}

// TestHeartbeatTrailingData 尾部数据拒绝（审查 R1-#8）。
func TestHeartbeatTrailingData(t *testing.T) {
	h, _ := newTestHandler(t)
	tok, _ := mustRegister(t, h, "n1")
	raw := `{"node":"n1","agent_version":"v","metrics":{}} {"dup":true}`
	if rec := doRawReq(t, h, "/api/agent/heartbeat", tok, raw); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing data = %d, want 400", rec.Code)
	}
}

// TestCollectErrorsSanitized 客户端可控 collect_errors 的净化（审查 R1-附）。
func TestCollectErrorsSanitized(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	// 超长消息 + 控制字符：接受但截断、去控制字符。
	long := strings.Repeat("x", 10000) + "\x00\x1b[31m"
	body := heartbeatBody("n1", false)
	body["collect_errors"] = map[string]string{"cpu_pct": long}
	rec := doReq(t, h, "/api/agent/heartbeat", tok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("oversize collect_errors = %d body=%s", rec.Code, rec.Body.String())
	}
	row, _ := st.LatestMetrics(t.Context(), nodeID)
	if len(row.CollectErrors) > 512 {
		t.Fatalf("collect_errors not truncated: %d bytes", len(row.CollectErrors))
	}
	if strings.ContainsAny(row.CollectErrors, "\x00\x1b") {
		t.Fatalf("control chars not stripped: %q", row.CollectErrors)
	}

	// 条目数超限：400。
	many := map[string]string{}
	for i := range 20 {
		many[fmt.Sprintf("e%02d", i)] = "boom"
	}
	body = heartbeatBody("n1", false)
	body["collect_errors"] = many
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("too many collect_errors entries = %d, want 400", rec.Code)
	}

	// 空 key/value：400。
	for _, bad := range []map[string]string{{"": "x"}, {"k": ""}} {
		body = heartbeatBody("n1", false)
		body["collect_errors"] = bad
		if rec := doReq(t, h, "/api/agent/heartbeat", tok, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("empty key/value collect_errors = %d, want 400", rec.Code)
		}
	}
}

func TestOfflineThenHeartbeatRevives(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	// 节点掉线：last_seen 拨到 120s 前并执行巡检。
	if err := st.SetNodeLastSeen(t.Context(), nodeID, time.Now().Unix()-120); err != nil {
		t.Fatalf("age node: %v", err)
	}
	if n, err := st.SweepOffline(t.Context(), time.Now().Add(-60*time.Second).Unix()); err != nil || n != 1 {
		t.Fatalf("sweep = %d err=%v, want 1", n, err)
	}
	offline, _ := st.GetNodeByName(t.Context(), "n1")
	if offline.Status != "offline" {
		t.Fatalf("status = %q, want offline", offline.Status)
	}

	// 同一个老 token 心跳 → 复活为 online，且不产生重复节点行。
	rec := doReq(t, h, "/api/agent/heartbeat", tok, heartbeatBody("n1", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("revive heartbeat = %d body=%s", rec.Code, rec.Body.String())
	}
	revived, _ := st.GetNodeByName(t.Context(), "n1")
	if revived.Status != "online" {
		t.Fatalf("status = %q, want online", revived.Status)
	}
	if revived.ID != nodeID {
		t.Fatalf("revive created new row: id %d -> %d", nodeID, revived.ID)
	}
}

func TestHeartbeatAfterNodeDeleted(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	if rec := doReq(t, h, "/api/agent/heartbeat", tok, heartbeatBody("n1", false)); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat before delete = %d", rec.Code)
	}

	// 节点行被删除（管理操作）后，旧 token 心跳 → 401（凭据失效，不复活幽灵节点）。
	if n, err := st.DeleteNode(t.Context(), nodeID); err != nil || n != 1 {
		t.Fatalf("delete node = %d err=%v", n, err)
	}
	rec := doReq(t, h, "/api/agent/heartbeat", tok, heartbeatBody("n1", false))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("heartbeat after delete = %d, want 401", rec.Code)
	}
}

// ---- M1b-a：services/agents 上报扩展（SPEC §2/§3/§4）----

// heartbeatServicesBody 构造带 services/agents 的心跳体。
func heartbeatServicesBody(node string, services []map[string]any, agents []map[string]any) map[string]any {
	body := heartbeatBody(node, false)
	if services != nil {
		body["services"] = services
	}
	if agents != nil {
		body["agents"] = agents
	}
	return body
}

func svcMap(name, typ, target, status string) map[string]any {
	return map[string]any{"name": name, "type": typ, "target": target, "status": status, "detail": ""}
}

func agentMap(name, typ, version, path, status string) map[string]any {
	return map[string]any{"name": name, "type": typ, "version": version, "path": path, "status": status}
}

func TestHeartbeatServicesAndAgents(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	// 带 services+agents 的心跳 → 200 且落库。
	body := heartbeatServicesBody("n1",
		[]map[string]any{svcMap("rustdesk", "process", "rustdesk", "active"), svcMap("derp", "docker", "derp", "inactive")},
		[]map[string]any{agentMap("zcode", "cli", "1.2.3", "/usr/local/bin/zcode", "active")})
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, body); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat(svc+agents) = %d body=%s", rec.Code, rec.Body.String())
	}
	svcs, _ := st.ListServices(t.Context(), nodeID)
	if len(svcs) != 2 || svcs[0].Name != "derp" || svcs[1].Name != "rustdesk" {
		t.Fatalf("services rows wrong: %+v", svcs)
	}
	if svcs[1].Type != "process" || svcs[1].Target != "rustdesk" || svcs[1].Status != "active" {
		t.Fatalf("rustdesk row wrong: %+v", svcs[1])
	}
	agents, _ := st.ListAgents(t.Context(), nodeID)
	if len(agents) != 1 || agents[0].Name != "zcode" || agents[0].Version != "1.2.3" ||
		agents[0].Path != "/usr/local/bin/zcode" || agents[0].Status != "active" {
		t.Fatalf("agents rows wrong: %+v", agents)
	}
	if agents[0].Invokable {
		t.Fatal("invokable must be false")
	}

	// 字段缺席 → 无变化不覆盖（上轮数据保持）。
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, heartbeatBody("n1", false)); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat(no svc) = %d", rec.Code)
	}
	svcs, _ = st.ListServices(t.Context(), nodeID)
	if len(svcs) != 2 || svcs[1].Status != "active" {
		t.Fatalf("absent services must not overwrite, got %+v", svcs)
	}
	agents, _ = st.ListAgents(t.Context(), nodeID)
	if len(agents) != 1 || agents[0].Status != "active" {
		t.Fatalf("absent agents must not overwrite, got %+v", agents)
	}

	// 显式 [] → 全量替换为空：既有行转 stale。
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, heartbeatServicesBody("n1", []map[string]any{}, []map[string]any{})); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat(empty arrays) = %d", rec.Code)
	}
	svcs, _ = st.ListServices(t.Context(), nodeID)
	for _, s := range svcs {
		if s.Status != "stale" {
			t.Fatalf("service %s should be stale after empty report: %+v", s.Name, s)
		}
	}
	agents, _ = st.ListAgents(t.Context(), nodeID)
	if len(agents) != 1 || agents[0].Status != "stale" {
		t.Fatalf("agent should be stale after empty report: %+v", agents)
	}

	// 同名覆盖（声明优先语义在服务端即 UPSERT）。
	body = heartbeatServicesBody("n1", []map[string]any{svcMap("rustdesk", "process", "rustdesk", "failed")}, nil)
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, body); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat(upsert) = %d", rec.Code)
	}
	svcs, _ = st.ListServices(t.Context(), nodeID)
	if len(svcs) != 2 {
		t.Fatalf("upsert must not create duplicate rows, rows=%d", len(svcs))
	}
	for _, s := range svcs {
		if s.Name == "rustdesk" && s.Status != "failed" {
			t.Fatalf("rustdesk should be failed after upsert: %+v", s)
		}
	}
}

// TestHeartbeatServicesValidation services/agents 值域与结构校验，全部 400 且不落库。
func TestHeartbeatServicesValidation(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad type", heartbeatServicesBody("n1", []map[string]any{svcMap("s", "k8s", "s", "active")}, nil)},
		{"bad status", heartbeatServicesBody("n1", []map[string]any{svcMap("s", "process", "s", "running")}, nil)},
		{"empty name", heartbeatServicesBody("n1", []map[string]any{svcMap("", "process", "s", "active")}, nil)},
		{"dup name", heartbeatServicesBody("n1", []map[string]any{svcMap("s", "process", "a", "active"), svcMap("s", "process", "b", "active")}, nil)},
		{"bad target charset", heartbeatServicesBody("n1", []map[string]any{svcMap("s", "systemd", "a;b", "active")}, nil)},
		{"target leading dash", heartbeatServicesBody("n1", []map[string]any{svcMap("s", "docker", "-c", "active")}, nil)},
		{"agent bad type", heartbeatServicesBody("n1", nil, []map[string]any{agentMap("a", "daemon", "1", "/x", "active")})},
		{"agent bad status", heartbeatServicesBody("n1", nil, []map[string]any{agentMap("a", "cli", "1", "/x", "ok")})},
		{"agent empty name", heartbeatServicesBody("n1", nil, []map[string]any{agentMap("", "cli", "1", "", "active")})},
	}
	for _, c := range cases {
		if rec := doReq(t, h, "/api/agent/heartbeat", tok, c.body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d body=%s, want 400", c.name, rec.Code, rec.Body.String())
		}
	}

	// 条目超限 → 400。
	many := []map[string]any{}
	for i := range 65 {
		many = append(many, svcMap(fmt.Sprintf("s%02d", i), "process", "x", "active"))
	}
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, heartbeatServicesBody("n1", many, nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("too many services = %d, want 400", rec.Code)
	}

	// 全部被拒的请求不应留下任何行。
	if n, err := st.CountMetrics(t.Context(), nodeID); err != nil || n != 0 {
		t.Fatalf("rejected heartbeats must not store metrics, got %d err=%v", n, err)
	}
	rows, _ := st.ListServices(t.Context(), nodeID)
	if len(rows) != 0 {
		t.Fatalf("rejected services must not store rows, got %+v", rows)
	}
}

// TestHeartbeatAgentsDetailSanitized detail/version/path 净化：控制字符去除、超长截断。
func TestHeartbeatAgentsDetailSanitized(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	long := strings.Repeat("v", 500) + "\x00\x1b[31m"
	body := heartbeatServicesBody("n1", nil, []map[string]any{
		{"name": "zcode", "type": "cli", "version": long, "path": long, "status": "unavailable", "detail": long},
	})
	if rec := doReq(t, h, "/api/agent/heartbeat", tok, body); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d body=%s", rec.Code, rec.Body.String())
	}
	agents, _ := st.ListAgents(t.Context(), nodeID)
	a := agents[0]
	for field, val := range map[string]string{"version": a.Version, "path": a.Path, "detail": a.Detail} {
		if strings.ContainsAny(val, "\x00\x1b") {
			t.Fatalf("%s contains control chars: %q", field, val)
		}
		if len(val) > 512 {
			t.Fatalf("%s not truncated: %d bytes", field, len(val))
		}
	}
}

// TestHeartbeatNullFieldsRejected 可选数组字段三态回归（R11-A）：显式 null 是
// 协议违规（400，不落库），与「字段缺席 = 无变化」严格区分。
func TestHeartbeatNullFieldsRejected(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	base := `{"node":"n1","metrics":{"cpu_pct":1.5},"agent_version":"v1"`
	cases := []struct {
		name, body string
	}{
		{"services null", base + `,"services":null}`},
		{"agents null", base + `,"agents":null}`},
		{"both null", base + `,"services":null,"agents":null}`},
		{"null with whitespace", base + `,"services": null }`},
	}
	for _, c := range cases {
		if rec := doRawReq(t, h, "/api/agent/heartbeat", tok, c.body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d body=%s, want 400", c.name, rec.Code, rec.Body.String())
		}
	}
	// 被拒的心跳不落任何行。
	if n, err := st.CountMetrics(t.Context(), nodeID); err != nil || n != 0 {
		t.Fatalf("null-rejected heartbeats must not store metrics, got %d err=%v", n, err)
	}

	// 对照：字段缺席 → 200 且正常落库（缺席语义保持）。
	if rec := doRawReq(t, h, "/api/agent/heartbeat", tok, base+`}`); rec.Code != http.StatusOK {
		t.Fatalf("absent fields must stay 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if n, err := st.CountMetrics(t.Context(), nodeID); err != nil || n != 1 {
		t.Fatalf("absent-field heartbeat must store metrics, got %d err=%v", n, err)
	}
}
