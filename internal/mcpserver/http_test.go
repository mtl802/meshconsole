// http_test.go 为 MCP 公网 HTTP 测试（SPEC-M1b-c2 §2）：Bearer 认证（401
// 中文化、不泄露工具列表）、Stateless JSON 模式协商、工具清单/调用真实可通、
// description 中文、安全头。
package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mtl802/meshconsole/internal/auth"
	"github.com/mtl802/meshconsole/internal/store"
)

const testTokenPlain = "mcp_test0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

func newTestHTTP(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	am := auth.New(st, nopLog{})
	// 建用户 + token 明文哈希落库（与 CLI user add / token create 同口径）。
	hash, err := auth.HashPassword("unused-password-99")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(t.Context(), "lunge", hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(t.Context(), auth.HashToken(testTokenPlain), u.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	return NewHTTPHandler(st, "test", am), st
}

type nopLog struct{}

func (nopLog) Warn(string, ...any)  {}
func (nopLog) Error(string, ...any) {}
func (nopLog) Info(string, ...any)  {}

// postMCP 按 MCP streamable HTTP 客户端形态发 JSON-RPC POST。
func postMCP(h http.Handler, body string, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestMCPAuthGate 无/错 token 一律 401：响应体中文化、不泄露任何工具元数据。
func TestMCPAuthGate(t *testing.T) {
	h, _ := newTestHTTP(t)
	for _, bearer := range []string{"", "wrong-token", testTokenPlain + "x"} {
		rec := postMCP(h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, bearer)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bearer %q: code = %d, want 401", bearer, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "认证失败") {
			t.Fatalf("bearer %q: body = %s, want Chinese error", bearer, body)
		}
		if strings.Contains(body, "list_nodes") || strings.Contains(body, "tools") {
			t.Fatalf("bearer %q: leaked tool metadata: %s", bearer, body)
		}
	}
}

// TestMCPJSONModeAndTools 有 token：JSON 同步响应（非 SSE）协商 initialize /
// tools/list / tools/call 全通；description 全中文；GET 405（Stateless 仅 POST）；
// 安全头全链携带。
func TestMCPJSONModeAndTools(t *testing.T) {
	h, st := newTestHTTP(t)
	// 造一点真实数据供 tools/call。
	ctx := t.Context()
	if _, err := st.RegisterNode(ctx, "cloud-1", "server", "linux", "amd64", "h1", "r1", 0); err != nil {
		t.Fatal(err)
	}

	// initialize。
	rec := postMCP(h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, testTokenPlain)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize: code = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("initialize content-type = %q, want application/json (JSON mode)", ct)
	}
	var initResp struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil || initResp.Result.ServerInfo.Name != "meshconsole" {
		t.Fatalf("initialize resp = %s err=%v", rec.Body.String(), err)
	}

	// tools/list：六工具、description 非空且含中文。
	rec = postMCP(h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, testTokenPlain)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list: code = %d", rec.Code)
	}
	var listResp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("tools/list decode: %v\n%s", err, rec.Body.String())
	}
	tools := listResp.Result.Tools
	if len(tools) != 6 {
		t.Fatalf("tools = %d, want 6", len(tools))
	}
	for _, tl := range tools {
		if tl.Description == "" || !strings.ContainsAny(tl.Description, "节点工具状态服务任务网") {
			t.Fatalf("tool %q description not Chinese: %q", tl.Name, tl.Description)
		}
	}

	// tools/call list_nodes：真实数据。
	rec = postMCP(h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_nodes","arguments":{}}}`, testTokenPlain)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call: code = %d", rec.Code)
	}
	var callResp struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &callResp); err != nil {
		t.Fatalf("tools/call decode: %v\n%s", err, rec.Body.String())
	}
	nodes, _ := callResp.Result.StructuredContent["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %v", callResp.Result.StructuredContent)
	}

	// GET /mcp：Stateless 模式下 SDK 405（认证先过）。
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testTokenPlain)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405", rec.Code)
	}

	// 安全头（no-store/nosniff）在成功路径也要有。
	if rec = postMCP(h, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, testTokenPlain); rec.Code == http.StatusOK {
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("security headers missing on success path")
		}
	}
}

// TestMCPSDKErrorChinese SDK 层英文错误中文化（R33-#3）：未知工具 / 未知方法
// / 非法帧 / GET 405 一律中文错误体；不泄露工具清单，JSON-RPC code 保留
// （协议层兼容）。
func TestMCPSDKErrorChinese(t *testing.T) {
	h, _ := newTestHTTP(t)
	// 未知工具：message 中文化，不回显 SDK 原文、不泄露工具清单。
	rec := postMCP(h, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`, testTokenPlain)
	var errResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil || errResp.Error == nil {
		t.Fatalf("unknown tool: body = %s err=%v", rec.Body.String(), err)
	}
	if !strings.Contains(errResp.Error.Message, "未知工具") {
		t.Fatalf("unknown tool message = %q, want Chinese", errResp.Error.Message)
	}
	if strings.Contains(rec.Body.String(), "unknown tool") {
		t.Fatalf("raw SDK error leaked: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "list_nodes") || strings.Contains(rec.Body.String(), "get_mesh_status") {
		t.Fatalf("tool inventory leaked: %s", rec.Body.String())
	}
	// JSON-RPC code 原样保留（客户端按 code 分支不受中文化影响）。
	if errResp.Error.Code != -32602 {
		t.Fatalf("unknown tool code = %d, want -32602", errResp.Error.Code)
	}

	// 未知方法（旧协议版本走纯文本 400 路径）：中文化，不回显 SDK 原文。
	rec = postMCP(h, `{"jsonrpc":"2.0","id":8,"method":"bogus/method"}`, testTokenPlain)
	body := rec.Body.String()
	if !strings.Contains(body, "不支持的方法") || strings.Contains(body, "unsupported") {
		t.Fatalf("unknown method body = %s", body)
	}

	// 非法 JSON 帧。
	rec = postMCP(h, `{not-json`, testTokenPlain)
	if body = rec.Body.String(); !strings.Contains(body, "解析失败") {
		t.Fatalf("parse error body = %s", body)
	}

	// GET 405：状态保持，错误文本中文化。
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testTokenPlain)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "不支持的请求方法") {
		t.Fatalf("405 body = %q, want Chinese", rec.Body.String())
	}
}

// TestMCPHTTPErrorPathsChinese 复审点名残余路径的中文化（R35-#2）：空 POST、
// 带 Last-Event-ID 的 POST 两条 SDK 英文路径与未收录文案的兜底，状态码保持、
// SDK 原文不在场、不泄露内部细节。
func TestMCPHTTPErrorPathsChinese(t *testing.T) {
	h, _ := newTestHTTP(t)
	// 空 POST：400 + 「请求体不能为空」，SDK 原文不在场。
	rec := postMCP(h, "", testTokenPlain)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty POST = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "请求体不能为空") {
		t.Fatalf("empty POST body = %q, want Chinese", body)
	} else if strings.Contains(body, "non-empty body") {
		t.Fatalf("empty POST leaked SDK raw text: %q", body)
	}
	// 带 Last-Event-ID 的 POST：400 + 中文，SDK 原文不在场。
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testTokenPlain)
	req.Header.Set("Last-Event-ID", "stream-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Last-Event-ID POST = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Last-Event-ID") || !strings.Contains(body, "不支持") {
		t.Fatalf("Last-Event-ID POST body = %q, want Chinese", body)
	} else if strings.Contains(body, "can't send") {
		t.Fatalf("Last-Event-ID POST leaked SDK raw text: %q", body)
	}
	// 未收录的 text/plain 错误文案：兜底通用中文，不再原样透出（状态码由
	// zhErrorWriter 保留，此处直测翻译函数）。
	if got := string(translateSDKBody("text/plain; charset=utf-8", []byte("some future sdk error\n"))); got != "请求处理失败\n" {
		t.Fatalf("unmatched text/plain = %q, want generic Chinese", got)
	}
	// 兜底不得改变成功响应与非错误 JSON 形态。
	same := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	if got := translateSDKBody("application/json", same); string(got) != string(same) {
		t.Fatalf("success response altered: %s", got)
	}
}
