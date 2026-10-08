// commands_m1d_test.go 为 MCP 命令工具的 scope 授权与下发链路测试（SPEC-M1d
// §4/§7 验收：readonly token 调 submit_command → 403；operator 正常下发）。
package mcpserver

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mtl802/meshconsole/internal/auth"
	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

const opTokenPlain = "mcp_operator0123456789abcdef0123456789abcdef0123456789abcdef0123"

// newM1dHTTP 构造带 readonly + operator 双 token 与一个 l2 节点的 handler。
func newM1dHTTP(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mcp-m1d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	am := auth.New(st, nopLog{})
	hash, err := auth.HashPassword("unused-password-99")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(t.Context(), "lunge", hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(t.Context(), auth.HashToken(testTokenPlain), u.ID, "", nil, store.ScopeReadonly); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(t.Context(), auth.HashToken(opTokenPlain), u.ID, "", nil, store.ScopeOperator); err != nil {
		t.Fatal(err)
	}
	// 节点：在线 + l2 白名单 + mac caps（经导出 API 构造，与生产同步路径一致）。
	if _, err := st.RegisterNode(t.Context(), "mac-mini", "server", "darwin", "arm64", "node-hash-1", "reg-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncL2Allowed(t.Context(), []string{"mac-mini"}); err != nil {
		t.Fatal(err)
	}
	caps := `["mac-launchd"]`
	if ok, err := st.HeartbeatFull(t.Context(), &store.MetricsRow{NodeID: 1, TS: 1, CPUPct: fltp(1)},
		"m1d", nil, nil, nil, false, nil, &caps); err != nil || !ok {
		t.Fatalf("seed node: %v", err)
	}
	return NewHTTPHandler(st, &config.Console{}, "test", am), st
}

func fltp(v float64) *float64 { return &v }

func submitCallBody(id int) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"submit_command","arguments":{"node":"mac-mini","kind":"ps_snapshot"}}}`
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// TestMCPSubmitScope403 SPEC-M1d §7 验收：readonly token 调 submit → HTTP 403
// （SDK 工具错误是 JSON-RPC 200 帧，scope 拦截必须在 HTTP 层给出真实 403）。
func TestMCPSubmitScope403(t *testing.T) {
	h, _ := newM1dHTTP(t)
	rec := postMCP(h, submitCallBody(1), testTokenPlain)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("readonly submit = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "operator") {
		t.Fatalf("403 body must explain scope: %s", rec.Body.String())
	}
	// 缺失/错误 token 仍 401（认证先于授权）。
	rec = postMCP(h, submitCallBody(2), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
}

// TestMCPOperatorSubmit operator token 下发成功（JSON-RPC 200 + 结构化结果），
// 命令落库 pending；非法 kind 400 语义经 JSON-RPC 错误帧回传。
func TestMCPOperatorSubmit(t *testing.T) {
	h, st := newM1dHTTP(t)
	rec := postMCP(h, submitCallBody(1), opTokenPlain)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator submit = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var rpc struct {
		Result struct {
			StructuredContent struct {
				CommandID string `json:"command_id"`
				Status    string `json:"status"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rpc); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if rpc.Result.StructuredContent.CommandID == "" || rpc.Result.StructuredContent.Status != "pending" {
		t.Fatalf("submit result: %s", rec.Body.String())
	}
	rows, err := st.ListCommands(t.Context(), "mac-mini", "pending", 10)
	if err != nil || len(rows) != 1 || rows[0].CommandID != rpc.Result.StructuredContent.CommandID {
		t.Fatalf("pending row: %v err=%v", rows, err)
	}
	if rows[0].CreatedBy != "lunge" || rows[0].Scope != "operator" {
		t.Fatalf("audit attribution: %+v", rows[0])
	}
	// get_command（readonly 也可查）。
	getBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_command","arguments":{"command_id":"` +
		rpc.Result.StructuredContent.CommandID + `"}}}`
	rec = postMCP(h, getBody, testTokenPlain)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), rpc.Result.StructuredContent.CommandID) {
		t.Fatalf("readonly get_command: %d %s", rec.Code, rec.Body.String())
	}
	// 非白名单 kind：JSON-RPC 错误帧（HTTP 200）。
	badBody := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"submit_command","arguments":{"node":"mac-mini","kind":"shutdown_now"}}}`
	rec = postMCP(h, badBody, opTokenPlain)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "白名单") {
		t.Fatalf("bad kind: %d %s", rec.Code, rec.Body.String())
	}
}
