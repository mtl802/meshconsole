// http.go 为 MCP 公网 HTTP 传输（SPEC-M1b-c2 §2）：
//
//	POST /mcp   Streamable HTTP（官方 go-sdk，锁定 go.mod v1.8.0）
//
// 实现选型说明：官方 go-sdk 自 v1.8.0 起 StreamableHTTPOptions 提供
// `Stateless`（仅 POST，GET/DELETE 405，无会话状态）与 `JSONResponse`
// （响应为单个 application/json 而非 SSE 流）——正是 SPEC 指定的「JSON 同步
// 响应模式」：tools/call 均为短查询，无长连 SSE，与 http.Server WriteTimeout
// 30s 无冲突（若 SDK 不可用才回退完整手写，本期不需要）。兼容客户端声明：
// 实现 MCP Streamable HTTP 传输规范（2025-06-18 及之后版本）的任意客户端，
// 例如 HanaAgent MCP connector、Claude/其他标准 MCP 客户端。
//
// 安全口径：Bearer api_token 哈希查表比对（auth.Manager.CheckAPIToken：应用层
// constant-time 复核 + 用户启用态 + 到期校验），失败 401、响应体中文化、不泄露
// 工具列表；MCP 独立并发闸 semaphore 4，超出 429——与 overview 的 8 并行，
// 防挤占 agent 心跳；全链 no-store/nosniff；请求体上限 1MB（SDK 侧
// MaxRequestBodyBytes 强制，DESIGN §7-9 口径）。
package mcpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mtl802/meshconsole/internal/auth"
	"github.com/mtl802/meshconsole/internal/store"
)

// mcpConcurrent 为 MCP 独立并发闸上限（SPEC-M1b-c2 §2/§7：semaphore 4）。
const mcpConcurrent = 4

// maxMCPBody 为 MCP 请求体上限（与 agent API 同口径 1MB，DESIGN §7-9）。
const maxMCPBody = 1 << 20

// writeJSONError 输出统一形态的中文化 JSON 错误（协议层错误仍由 JSON-RPC 帧
// 承载，本函数只用于 HTTP 层：认证/限流/方法）。
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// NewHTTPHandler 构造挂 /mcp 的公网 handler 链：
//
//	安全头 → Bearer 认证（401 中文化）→ 并发闸（429）→ SDK Streamable HTTP
func NewHTTPHandler(st *store.Store, version string, am *auth.Manager) http.Handler {
	srv := New(st, version, nil)
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			// Stateless：无会话状态、仅 POST（GET/DELETE 405）——每次请求独立
			// 临时会话，天然规避会话表与闲置连接管理。
			Stateless: true,
			// JSONResponse：响应为单个 application/json（JSON 同步响应模式，
			// SPEC §2 指定），不开 SSE 流——与 WriteTimeout 30s 无冲突。
			JSONResponse:        true,
			MaxRequestBodyBytes: maxMCPBody,
		})
	// MCP 独立并发闸：超出 4 个并发即 429、不排队（公网面排队只会堆积连接；
	// agent 心跳的 64 配额与本闸完全独立，互不挤占——SPEC §2）。
	sem := make(chan struct{}, mcpConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 全链安全头（SPEC §7：no-store/nosniff 不因拒绝路径豁免）。
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		// 认证先行：不消耗并发闸位（认证失败不值得占用配额）。
		if _, err := am.CheckAPIToken(r.Context(), auth.BearerToken(r)); err != nil {
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			// zhErrorWriter 在 noStoreWriter 之外：SDK 的英文错误体（unknown tool、
			// method not found 等，R33-#3）在落笔前被映射为中文，安全头覆盖不变。
			// JSON 模式下 SDK 依懒 net/http 的隐式收尾（不显式 Flush），缓冲内容
			// 必须在 ServeHTTP 返回后显式落定；finalize 幂等，Flush 路径不受影响。
			zw := &zhErrorWriter{ResponseWriter: &noStoreWriter{ResponseWriter: w}}
			inner.ServeHTTP(zw, r)
			zw.finalize()
		default:
			writeJSONError(w, http.StatusTooManyRequests, "MCP 请求并发已达上限，请稍后再试")
		}
	})
}

// zhBufferCap 为错误翻译的缓冲上限：JSON 模式响应在缓冲内完整落定后统一判定，
// 超限即放弃翻译原样透出（视图数据规模远低于此，兜底内存有界）。
const zhBufferCap = 1 << 20

// zhErrorWriter 拦截 SDK handler 的响应体（R33-#3）：SDK 的 JSON-RPC 错误与
// 少量 text/plain 错误为固定英文文案（unknown tool / method not found 等），
// SPEC §2/§3 要求中文化且不泄露内部细节与工具清单。SDK 未提供错误定制钩子，
// 故在 HTTP 层缓冲响应、按「错误类型」映射 message 后落笔——JSON-RPC 的
// code/id/data 原样保留（协议层兼容客户端），仅 message 替换；成功响应与
// 超限/流式形态原样透出。生产配置为 JSONResponse 模式（单帧 JSON 响应），
// 缓冲不影响流式语义；Flush 到达时按「已缓冲内容先翻译落笔」处理。
type zhErrorWriter struct {
	http.ResponseWriter
	buf      bytes.Buffer
	status   int
	haveSt   bool
	flushed  bool // 已向底层落笔（此后直通，不再翻译）
	fallback bool // 超出缓冲上限，放弃翻译
}

func (w *zhErrorWriter) WriteHeader(code int) {
	if w.flushed || w.fallback {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.status, w.haveSt = code, true
}

func (w *zhErrorWriter) Write(p []byte) (int, error) {
	if w.flushed || w.fallback {
		return w.ResponseWriter.Write(p)
	}
	if w.buf.Len()+len(p) > zhBufferCap {
		w.finalize() // 原样落笔已缓冲内容，后续直通
		w.fallback = true
		return w.ResponseWriter.Write(p)
	}
	w.buf.Write(p) // bytes.Buffer 写内存恒成功
	return len(p), nil
}

func (w *zhErrorWriter) Flush() {
	if w.flushed || w.fallback {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.finalize()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finalize 把缓冲内容（按需翻译后）一次性落笔。
func (w *zhErrorWriter) finalize() {
	if w.flushed {
		return
	}
	w.flushed = true
	body := translateSDKBody(w.Header().Get("Content-Type"), w.buf.Bytes())
	if !w.haveSt {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}

// jsonrpcErrorResp 为 JSON-RPC 错误响应的最小解码形态（id/data 以 RawMessage
// 原样透传，重编码不改变协议层语义）。
type jsonrpcErrorResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

// translateSDKBody 按 Content-Type 把 SDK 固定形态的英文错误体映射为中文
// （R33-#3、R35-#2 收口）。只做类型级映射：application/json 解 JSON-RPC 信封
// 后错误 message 全量过 translateSDKErrorMessage（含默认分支）；text/plain
// 为 SDK http.Error 的固定文案闭合集，未收录形态统一为通用中文——没有任何
// 「看不懂就透出」的路径；成功响应（含 result）不受影响。
func translateSDKBody(contentType string, body []byte) []byte {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if strings.HasPrefix(ct, "application/json") {
		var resp jsonrpcErrorResp
		if err := json.Unmarshal(body, &resp); err != nil || resp.Error == nil {
			return body // 成功响应或非 JSON-RPC 形态：原样
		}
		resp.Error.Message = translateSDKErrorMessage(resp.Error.Code, resp.Error.Message)
		out, err := json.Marshal(&resp)
		if err != nil {
			return body
		}
		return out
	}
	if strings.HasPrefix(ct, "text/plain") {
		// SDK 经 http.Error 写出的 HTTP 层错误文本（固定文案，闭合集匹配；
		// R35-#2：未收录形态不再原样透出，统一映射为通用中文——SDK 的
		// text/plain 响应仅出自 http.Error 错误路径，无成功体经此发生）。
		s := strings.TrimRight(string(body), "\n")
		switch {
		case s == "Method Not Allowed" || s == "unsupported method":
			return []byte("不支持的请求方法\n")
		case s == "Content-Type must be 'application/json'":
			return []byte("Content-Type 须为 application/json\n")
		case s == "Accept must contain both 'application/json' and 'text/event-stream'":
			return []byte("Accept 头须同时包含 application/json 与 text/event-stream\n")
		case strings.HasPrefix(s, "JSON-RPC batching is not supported"):
			return []byte("JSON-RPC 批量请求不受支持\n")
		case strings.HasPrefix(s, "JSON RPC not handled") || strings.HasPrefix(s, "method not found"):
			return []byte("不支持的方法\n")
		case strings.Contains(s, "Unsupported protocol version"):
			return []byte("协议版本不受支持\n")
		case strings.HasPrefix(s, "malformed payload"):
			return []byte("请求体解析失败：非法 JSON-RPC 帧\n")
		case strings.Contains(s, "request body exceeds"):
			return []byte("请求体超过大小上限\n")
		case strings.HasPrefix(s, "Forbidden: invalid Host header"):
			return []byte("Host 不被允许\n")
		case s == "no server available":
			return []byte("服务暂不可用\n")
		// R35-#2 复审点名的两条残余路径：空 POST 与带 Last-Event-ID 的 POST。
		case s == "POST requires a non-empty body":
			return []byte("请求体不能为空\n")
		case s == "can't send Last-Event-ID for POST request":
			return []byte("POST 请求不支持 Last-Event-ID（该头仅用于 GET 事件流续传）\n")
		default:
			// 兜底：未知错误文案不回显原文（不泄露内部细节），与 JSON 路径的
			// translateSDKErrorMessage 默认分支同口径；状态码由 zhErrorWriter
			// 原样保留。
			return []byte("请求处理失败\n")
		}
	}
	return body
}

// translateSDKErrorMessage 按错误类型映射中文 message（不透出工具清单与内部
// 细节；「unknown tool」不回显请求的工具名）。JSON-RPC code 由调用方原样保留。
func translateSDKErrorMessage(code int, msg string) string {
	switch {
	case strings.HasPrefix(msg, "unknown tool"):
		return "未知工具：请求的工具不存在（可用 tools/list 获取清单）"
	case strings.Contains(msg, "unsupported protocol version"):
		return "协议版本不受支持"
	case strings.Contains(msg, "is invalid during session initialization"):
		return "会话未初始化：请先发送 initialize"
	case strings.Contains(msg, "duplicate in-flight request ID"):
		return "请求冲突：同一请求 ID 仍在处理中，请勿重复提交"
	case code == jsonrpc.CodeParseError:
		return "请求解析失败：非法 JSON-RPC 帧"
	case code == jsonrpc.CodeInvalidRequest:
		return "请求无效"
	case code == jsonrpc.CodeMethodNotFound:
		return "不支持的方法"
	case code == jsonrpc.CodeInvalidParams:
		return "请求参数无效"
	default:
		return "请求处理失败"
	}
}

// noStoreWriter 在 SDK 落笔响应头前强制 Cache-Control: no-store——SDK 自己会
// 写 Cache-Control: no-cache, no-transform（语义相近但非 SPEC §7「全链
// no-store」字面口径），在 WriteHeader 拦截点覆盖为 no-store。
type noStoreWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *noStoreWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush 透传（SDK 的流式写路径需要；JSON 模式下通常不触发）。
func (w *noStoreWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
