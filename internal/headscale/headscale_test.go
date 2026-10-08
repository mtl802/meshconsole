package headscale

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func tailnetRows(t *testing.T, st *store.Store) []store.TailnetNodeRecord {
	t.Helper()
	rows, err := st.ListTailnetNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// headscaleJSON v0.23+ 风格响应：id 为字符串、带未知字段（解析必须忽略）；
// 第二节点 id 为数字形态（旧版本风格）、无 online 字段（nil → offline 存储）。
const headscaleJSON = `{
  "apiVersion": "v1",
  "kind": "ListNodesResponse",
  "nodes": [
    {
      "id": "1",
      "machineKey": "mkey:aaa",
      "nodeKey": "nodekey:bbb",
      "user": {"id": "1", "name": "ops"},
      "name": "cloud-1",
      "addresses": ["100.64.0.1", "fd7a:115c:a1e0::1"],
      "online": true,
      "lastSeen": "2026-10-08T02:00:00.123456789Z",
      "forcedTags": ["tag:server"],
      "invalidTags": [],
      "createdAt": "2026-01-01T00:00:00Z"
    },
    {
      "id": 2,
      "name": "mac-mini",
      "addresses": ["100.64.0.2"],
      "lastSeen": "2026-10-08T01:30:00Z"
    }
  ]
}`

// TestFetchNodesTolerantParsing 只取需要的字段；id 数字/字符串两形态兼容；
// 未知字段忽略；无 online 字段按 offline 存但不报错。
func TestFetchNodesTolerantParsing(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/node" {
			t.Errorf("path = %q, want /api/v1/node", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, headscaleJSON)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key-1")
	nodes, err := c.FetchNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer test-key-1" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	n1 := nodes[0]
	if n1.ID != 1 || n1.Name != "cloud-1" || !n1.Online || len(n1.IPs) != 2 || n1.IPs[0] != "100.64.0.1" {
		t.Fatalf("node1 = %+v", n1)
	}
	if n1.LastSeen == nil || n1.LastSeen.Unix() != time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("node1 lastSeen = %v", n1.LastSeen)
	}
	n2 := nodes[1]
	if n2.ID != 2 || n2.Name != "mac-mini" || n2.Online {
		t.Fatalf("node2 = %+v (id 数字形态须兼容；无 online 不得编造 true)", n2)
	}
	if n2.LastSeen == nil {
		t.Fatalf("node2 lastSeen missing")
	}
}

// TestFetchNodesHTTPError 非 200 → 错误（含状态码），不部分解析。
func TestFetchNodesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "bad-key")
	if _, err := c.FetchNodes(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401 error", err)
	}
}

// TestFetchNodesIllegalBody HTTP 200 但 body 非法（R19-#1，R21-#1 收紧为严格
// EOF 口径）：顶层 null / {} / 缺 nodes 键 / nodes:null / nodes 非数组 / 尾部
// 多余数据（含 `]`/`}` 起始的垃圾与拼接的第二个 JSON 值——dec.More() 放不过
// 的形态）→ 一律报错（调用方保留旧数据），不得被当成空列表清库；只有显式
// nodes 数组（含空数组）才是合法成功响应。
func TestFetchNodesIllegalBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"top-level null", `null`},
		{"empty object", `{}`},
		{"missing nodes key", `{"apiVersion":"v1","kind":"ListNodesResponse"}`},
		{"nodes null", `{"nodes":null}`},
		{"nodes not array", `{"nodes":{}}`},
		{"trailing garbage", `{"nodes":[]} trailing-junk`},
		{"trailing bracket", `{"nodes":[]}]`},
		{"trailing brace", `{"nodes":[]}}`},
		{"two json values", `{"nodes":[]}{"nodes":[]}`},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, tc.body)
		}))
		_, err := NewClient(srv.URL, "k").FetchNodes(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("%s: illegal 200 body must be a fetch error, got nil", tc.name)
		}
	}
	// 合法空列表：200 + {"nodes":[]} → 0 节点不报错（合法全量替换语义）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"nodes":[]}`)
	}))
	defer srv.Close()
	nodes, err := NewClient(srv.URL, "k").FetchNodes(context.Background())
	if err != nil || len(nodes) != 0 {
		t.Fatalf("explicit empty nodes array is a legal success: nodes=%d err=%v", len(nodes), err)
	}
}

// TestFetchNodesTruncatedOversized R21-#1 的截断/超限面：4MiB 上限内完整值+
// 巨量尾随空白（LimitReader 截断点落在完整值之后，二次 Decode 险些伪装 EOF）
// 与中途截断（连接提前断开，ErrUnexpectedEOF）都必须显式报 malformed 拒绝，
// 绝不能按正常空列表清库。
func TestFetchNodesTruncatedOversized(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"complete value padded past 4MiB", `{"nodes":[]}` + strings.Repeat(" ", 4<<20)},
		{"truncated mid-value", `{"nodes":[{"id":"1","name":"cloud-1"`},
		{"truncated mid-string", `{"nodes":"` + strings.Repeat("a", 4096)},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, tc.body)
		}))
		_, err := NewClient(srv.URL, "k").FetchNodes(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("%s: truncated/oversized body must be rejected, got nil", tc.name)
		}
		if !strings.Contains(err.Error(), "malformed") && !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("%s: err = %v, want explicit malformed/truncation report", tc.name, err)
		}
	}
}

// TestSyncIllegalResponseKeepsOld Sync 层验证 R19-#1：走真 client 的非法 200
// 响应按拉取失败处理——Sync 报错、既有 tailnet 数据原样保留（不清库）。
func TestSyncIllegalResponseKeepsOld(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	st := newTestStore(t)
	f := NewFetcher(NewClient(srv.URL, "k"), st)
	// 先放一份旧数据。
	f.fetch = func(context.Context) ([]Node, error) {
		return []Node{{ID: 1, Name: "cloud-1", Online: true}}, nil
	}
	if _, err := f.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 换非法 200 响应（fetch 注入置 nil 走真 client HTTP 路径）。
	f.fetch = nil
	if _, err := f.Sync(context.Background()); err == nil {
		t.Fatal("illegal 200 body must fail the sync")
	}
	rows := tailnetRows(t, st)
	if len(rows) != 1 || rows[0].MachineName != "cloud-1" || !rows[0].Online {
		t.Fatalf("illegal 200 must keep old data intact, got %+v", rows)
	}
}

// capturingHandler 捕获日志记录（节流状态机断言用）。
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }
func (h *capturingHandler) count(level slog.Level, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// TestFailureCounterStreakReset R19-#9：失败计数在连续成功 3 次后才归零——
// 闪断（失败→单次成功→失败）不重置计数也不重置节流窗口（WARN 至多一条），
// 连续 3 次成功才 INFO recovered 并归零。
func TestFailureCounterStreakReset(t *testing.T) {
	h := &capturingHandler{}
	log := slog.New(h)
	ctx := context.Background()
	boom := errors.New("boom")
	warnMsg := "headscale sync failed (keeping stale tailnet data)"
	infoMsg := "headscale sync recovered"

	// 序列：fail → ok → fail → ok → fail → ok → ok → ok。
	f := NewFetcher(nil, nil)
	seq := []bool{false, true, false, true, false, true, true, true}
	for i, ok := range seq {
		if ok {
			f.handleResult(ctx, 1, nil, log)
		} else {
			f.handleResult(ctx, 0, boom, log)
		}
		if n := i + 1; n == 3 && f.fails != 2 {
			// 第二次失败时：单次成功未归零，计数应累计到 2。
			t.Fatalf("after fail/ok/fail counter = %d, want 2 (single success must not reset)", f.fails)
		}
	}
	if got := h.count(slog.LevelWarn, warnMsg); got != 1 {
		t.Fatalf("WARN count = %d, want 1 (flapping must stay throttled)", got)
	}
	if got := h.count(slog.LevelInfo, infoMsg); got != 1 {
		t.Fatalf("recovered INFO count = %d, want 1 (only after %d consecutive successes)", got, successResetStreak)
	}
	if f.fails != 0 || f.streak != 0 {
		t.Fatalf("after 3 consecutive successes counter must reset, fails=%d streak=%d", f.fails, f.streak)
	}

	// 对照：单次成功夹在失败之间，计数必须保持累计（不归零）。
	f2 := NewFetcher(nil, nil)
	f2.handleResult(ctx, 0, boom, log)
	f2.handleResult(ctx, 1, nil, log)
	f2.handleResult(ctx, 0, boom, log)
	if f2.fails != 2 {
		t.Fatalf("counter = %d, want 2 (single success amid failures must not reset)", f2.fails)
	}
}

// TestWarnThrottleIndependentOfReset R21-#2：WARN 节流窗口独立于失败计数/
// streak——「失败→连续成功恢复归零→再失败」后的新失败仍受上次 WARN 的 1h
// 窗口约束（降级 Debug），不得借计数归零立即再 WARN；恢复 INFO 不受限。
func TestWarnThrottleIndependentOfReset(t *testing.T) {
	h := &capturingHandler{}
	log := slog.New(h)
	ctx := context.Background()
	boom := errors.New("boom")
	warnMsg := "headscale sync failed (keeping stale tailnet data)"
	infoMsg := "headscale sync recovered"
	debugMsg := "headscale sync failed (warn throttled)"

	f := NewFetcher(nil, nil)
	f.handleResult(ctx, 0, boom, log) // 首个失败（lastWarn 零值）→ 立即 WARN
	f.handleResult(ctx, 1, nil, log)  // 连续 3 次成功 → recovered INFO，计数归零
	f.handleResult(ctx, 1, nil, log)
	f.handleResult(ctx, 1, nil, log)
	if f.fails != 0 {
		t.Fatalf("fails = %d, want 0 after recovery", f.fails)
	}
	f.handleResult(ctx, 0, boom, log) // 恢复后再失败：紧邻上次 WARN 不足 1h
	f.handleResult(ctx, 0, boom, log) // 再失败：同样在窗口内

	if got := h.count(slog.LevelWarn, warnMsg); got != 1 {
		t.Fatalf("WARN count = %d, want 1 (window survives counter reset)", got)
	}
	if got := h.count(slog.LevelInfo, infoMsg); got != 1 {
		t.Fatalf("recovered INFO count = %d, want 1 (never throttled)", got)
	}
	if got := h.count(slog.LevelDebug, debugMsg); got != 2 {
		t.Fatalf("throttled Debug count = %d, want 2 (windowed failures downgrade to Debug)", got)
	}
	if f.fails != 2 {
		t.Fatalf("fails = %d, want 2 (counting keeps accruing across resets)", f.fails)
	}
}

// TestSyncFullReplace 全量替换（SPEC-M1b-b §3）：A 集合 → B 集合两次拉取后，
// 表内容与 B 完全一致（多退少补，无幽灵行）；行内字段（含 lastSeen 落秒）核对。
func TestSyncFullReplace(t *testing.T) {
	st := newTestStore(t)
	f := NewFetcher(nil, st)
	setA := []Node{{ID: 1, Name: "cloud-1", IPs: []string{"100.64.0.1"}, Online: true}}
	f.fetch = func(context.Context) ([]Node, error) { return setA, nil }
	if n, err := f.Sync(context.Background()); err != nil || n != 1 {
		t.Fatalf("sync A: n=%d err=%v", n, err)
	}
	lastSeen := time.Unix(1760000000, 0).UTC()
	setB := []Node{
		{ID: 2, Name: "mac-mini", IPs: []string{"100.64.0.2"}, Online: true, LastSeen: &lastSeen},
		{ID: 3, Name: "new-node"},
	}
	f.fetch = func(context.Context) ([]Node, error) { return setB, nil }
	if n, err := f.Sync(context.Background()); err != nil || n != 2 {
		t.Fatalf("sync B: n=%d err=%v", n, err)
	}
	rows := tailnetRows(t, st)
	if len(rows) != 2 || rows[0].ID != 2 || rows[1].ID != 3 {
		t.Fatalf("rows = %+v, want exactly B set (old rows replaced)", rows)
	}
	if rows[0].MachineName != "mac-mini" || rows[0].IPs != "100.64.0.2" || !rows[0].Online {
		t.Fatalf("row0 = %+v", rows[0])
	}
	if !rows[0].LastSeen.Valid || rows[0].LastSeen.Int64 != lastSeen.Unix() {
		t.Fatalf("row0 lastSeen = %+v", rows[0].LastSeen)
	}
	if rows[1].LastSeen.Valid {
		t.Fatalf("row1 lastSeen should be NULL, got %+v", rows[1].LastSeen)
	}
}

// TestSyncFailureKeepsOld 拉取失败 → 不触库，旧数据原样保留（DESIGN §4.1-F）。
func TestSyncFailureKeepsOld(t *testing.T) {
	st := newTestStore(t)
	f := NewFetcher(nil, st)
	f.fetch = func(context.Context) ([]Node, error) {
		return []Node{{ID: 1, Name: "cloud-1", Online: true}}, nil
	}
	if _, err := f.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("headscale unreachable")
	f.fetch = func(context.Context) ([]Node, error) { return nil, boom }
	if _, err := f.Sync(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	rows := tailnetRows(t, st)
	if len(rows) != 1 || rows[0].MachineName != "cloud-1" || !rows[0].Online {
		t.Fatalf("stale rows must be kept intact, got %+v", rows)
	}
}

// TestRunRecoveryAndThrottle Run 循环：失败计数 + 恢复 INFO + 数据恢复替换。
// 用可注入 fetch 与短 interval；日志只断言不打断流程。
func TestRunRecoveryAndThrottle(t *testing.T) {
	st := newTestStore(t)
	f := NewFetcher(nil, st)
	calls := 0
	f.fetch = func(context.Context) ([]Node, error) {
		calls++
		if calls <= 2 {
			return nil, errors.New("transient failure")
		}
		return []Node{{ID: 9, Name: "recovered", Online: true}}, nil
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	f.Run(ctx, 40*time.Millisecond, log)

	rows := tailnetRows(t, st)
	if len(rows) != 1 || rows[0].MachineName != "recovered" {
		t.Fatalf("rows after recovery = %+v", rows)
	}
	if f.fails != 0 {
		t.Fatalf("fails counter must reset on success, got %d", f.fails)
	}
}
