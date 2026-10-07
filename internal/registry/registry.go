// Package registry 实现 /api/agent/* 节点注册与心跳端点。
//
// 安全口径（DESIGN §7-2/§7-4/§7-9）：
//   - 除 /healthz 外全端点认证；认证失败一律 401，不泄露原因细节；
//   - register 校验一次性注册 token（短期、可绑定预期节点，DESIGN §4.1-A），
//     其余端点校验节点 token（SHA-256 哈希比对，DB 等值查询外再经应用层
//     constant-time 复核）；
//   - 节点 token 与节点绑定，心跳 body 的 node 必须与 token 匹配，跨节点 403；
//   - 请求体上限 1MB、并发上限有界，超限拒绝；
//   - 客户端可控字符串（collect_errors）入库与入日志前截断、去控制字符。
package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

const (
	maxBodyBytes     = 1 << 20 // DESIGN §7-9：请求体 ≤1MB
	maxInFlight      = 64      // DESIGN §7-4/§7-9：并发上限，超限拒绝
	maxCollectErrors = 16      // collect_errors 条目数上限
	maxCollectErrKv  = 256     // collect_errors 单条 key/value 长度上限
)

// Handler 为 agent API 的 HTTP 处理器集合。
type Handler struct {
	st  *store.Store
	log *slog.Logger
	// regTokens 为配置注入的一次性注册 token（哈希 + 到期 + 预期节点绑定）。
	regTokens []regTokenEntry
}

type regTokenEntry struct {
	hash []byte // SHA-256 hex
	// expiresAt 零值表示长期有效；到期后认证一律拒绝。
	expiresAt time.Time
	// expectedNode 非空表示仅该节点名可用此 token 注册（DESIGN §4.1-A）。
	expectedNode string
}

func New(st *store.Store, log *slog.Logger, registrationTokens []config.RegistrationToken) *Handler {
	h := &Handler{st: st, log: log}
	for _, t := range registrationTokens {
		sum := sha256.Sum256([]byte(t.Token))
		h.regTokens = append(h.regTokens, regTokenEntry{
			hash:         []byte(hex.EncodeToString(sum[:])),
			expiresAt:    t.ExpiresAt.Time,
			expectedNode: t.ExpectedNode,
		})
	}
	return h
}

// ---- 认证中间件 ----

type ctxKey int

const (
	ctxKeyNode   ctxKey = iota
	ctxKeyRegTok ctxKey = iota
)

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func tokenHashHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// 不携带任何失败原因细节。
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}

// requireRegToken 验证一次性注册 token：constant-time 全量比对（无提前退出）、
// 到期检查通过后把 token 约束项放入请求上下文。
func (h *Handler) requireRegToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" || len(h.regTokens) == 0 {
			unauthorized(w)
			return
		}
		sum := []byte(tokenHashHex(tok))
		// 单轮全量累加（R1-#7/R3-#7）：不因命中即退出，比对次数与条目序号无关，
		// 命中条目随累加一并记取，不再有第二轮查找。
		ok := 0
		var entry regTokenEntry
		for _, e := range h.regTokens {
			eq := subtle.ConstantTimeCompare(sum, e.hash)
			ok |= eq
			if eq == 1 {
				entry = e
			}
		}
		if ok != 1 {
			unauthorized(w)
			return
		}
		if !entry.expiresAt.IsZero() && !time.Now().Before(entry.expiresAt) {
			// token 已到期：与认证失败同等对待，统一 401。
			h.log.Warn("register rejected: registration token expired")
			unauthorized(w)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyRegTok, entry)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireNodeToken 验证节点 token 并把对应节点放入请求上下文。
// DB 按 token_hash 等值查询定位候选行，命中后再做应用层 constant-time 复核
// （索引查找的耗时不保证常量，复核保证接受路径常量时间，审查 R1-#7）。
func (h *Handler) requireNodeToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			unauthorized(w)
			return
		}
		sum := []byte(tokenHashHex(tok))
		node, err := h.st.AuthenticateNode(r.Context(), string(sum))
		if err != nil {
			h.log.Error("authenticate node", "err", err)
			unauthorized(w)
			return
		}
		if node == nil || subtle.ConstantTimeCompare([]byte(node.TokenHash), sum) != 1 {
			unauthorized(w)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyNode, node)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// limitBody 对请求体施加 1MB 上限。
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// limitConcurrent 用共享信号量限制 /api/agent/* 在处理的请求数（注册+心跳合计
// 不超配额，审查 R7），超限立即 503（DESIGN §7-4/§7-9）。
func limitConcurrent(sem chan struct{}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			httpError(w, http.StatusServiceUnavailable, "busy")
		}
	})
}

// decodeJSONStrict 解码 JSON 请求体：拒绝语法错误、超限（413）与尾部多余数据
// （body 未读到 EOF 视为协议违规，审查 R1-#8）。
func decodeJSONStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errTooLarge
		}
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if errors.As(err, new(*http.MaxBytesError)) {
			return errTooLarge
		}
		return errTrailingData
	}
	return nil
}

var (
	errTooLarge     = errors.New("request body too large")
	errTrailingData = errors.New("trailing data after JSON object")
)

// RegisterRoutes 将 agent API 挂到 mux（/healthz 由调用方单独注册，唯一免认证端点）。
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// 注册与心跳共享同一并发配额：两路由在处理请求合计不超 maxInFlight（审查 R7）。
	sem := make(chan struct{}, maxInFlight)
	mux.Handle("POST /api/agent/register",
		limitConcurrent(sem, limitBody(h.requireRegToken(http.HandlerFunc(h.handleRegister)))))
	mux.Handle("POST /api/agent/heartbeat",
		limitConcurrent(sem, limitBody(h.requireNodeToken(http.HandlerFunc(h.handleHeartbeat)))))
}

// ---- register ----

type registerReq struct {
	Name string `json:"name"`
	Role string `json:"role"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type registerResp struct {
	Status   string `json:"status"`
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	// NodeToken 明文只在注册响应出现一次，服务端只存 SHA-256。
	NodeToken string `json:"node_token"`
}

func validLabel(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

func (h *Handler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := decodeJSONStrict(r, &req); err != nil {
		h.badRequest(w, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !validLabel(req.Name, 64) {
		httpError(w, http.StatusBadRequest, "bad_request")
		return
	}
	req.Role, req.OS, req.Arch = strings.TrimSpace(req.Role), strings.TrimSpace(req.OS), strings.TrimSpace(req.Arch)
	for _, s := range []string{req.Role, req.OS, req.Arch} {
		if len(s) > 64 {
			httpError(w, http.StatusBadRequest, "bad_request")
			return
		}
	}

	// 预期节点绑定（DESIGN §4.1-A）：token 与节点身份绑定，其他节点名一律 401，
	// 不泄露「token 本身有效」这一事实。
	entry, _ := r.Context().Value(ctxKeyRegTok).(regTokenEntry)
	if entry.expectedNode != "" && req.Name != entry.expectedNode {
		h.log.Warn("register rejected: node name not bound to this registration token",
			"expected", entry.expectedNode, "got", req.Name)
		unauthorized(w)
		return
	}

	nodeToken, err := newNodeToken()
	if err != nil {
		h.log.Error("generate node token", "err", err)
		httpError(w, http.StatusInternalServerError, "internal")
		return
	}
	hash := tokenHashHex(nodeToken)
	// 一次性注册 token 的哈希（中间件已验证并放入上下文）作为消耗键；
	// 到期时刻一并带入，供注册事务内消费前二次校验（R3：消除读 body 期间的跨到期窗口）。
	regHashHex := string(entry.hash)
	var regExpires int64
	if !entry.expiresAt.IsZero() {
		regExpires = entry.expiresAt.Unix()
	}

	node, err := h.st.RegisterNode(r.Context(), req.Name, req.Role, req.OS, req.Arch, hash, regHashHex, regExpires)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrDuplicateNode):
		// 重复注册防护：名字已占用，直接拒绝，不签发、不覆盖。
		h.log.Warn("register rejected: duplicate node name", "name", req.Name)
		httpError(w, http.StatusConflict, "conflict")
		return
	case errors.Is(err, store.ErrTokenUsed), errors.Is(err, store.ErrTokenExpired):
		// 一次性 token 已用过/已到期（先于重名检查，不可借 409 探测节点名，审查 R1-#3）：
		// 与认证失败同等对待，统一 401。
		unauthorized(w)
		return
	default:
		h.log.Error("register node", "err", err)
		httpError(w, http.StatusInternalServerError, "internal")
		return
	}

	h.log.Info("node registered", "node_id", node.ID, "name", node.Name, "role", node.Role,
		"os", node.OS, "arch", node.Arch)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(registerResp{
		Status: "ok", NodeID: node.ID, NodeName: node.Name, NodeToken: nodeToken,
	})
}

// newNodeToken 生成 32 字节随机 hex（节点 token 明文）。
func newNodeToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ---- heartbeat ----

type heartbeatReq struct {
	// Node 须与 token 对应的节点名一致（跨节点上报拒绝）。
	Node string `json:"node"`
	// AgentVersion 为 agent 构建版本。
	AgentVersion string    `json:"agent_version"`
	Metrics      metricsIn `json:"metrics"`
	// CollectErrors 字段名 -> 采集失败原因；非空时 last_success 不刷新。
	CollectErrors map[string]string `json:"collect_errors"`
}

// metricsIn 与 agent 上报结构对应；指针承载 null 语义——采集失败的字段为 nil，禁止填 0。
type metricsIn struct {
	CPUPct    *float64 `json:"cpu_pct"`
	MemUsed   *int64   `json:"mem_used"`
	MemTotal  *int64   `json:"mem_total"`
	DiskUsed  *int64   `json:"disk_used"`
	DiskTotal *int64   `json:"disk_total"`
	NetRx     *int64   `json:"net_rx"`
	NetTx     *int64   `json:"net_tx"`
	UptimeS   *int64   `json:"uptime_s"`
	Load1     *float64 `json:"load1"`
}

type heartbeatResp struct {
	Status     string `json:"status"`
	ServerTime int64  `json:"server_time"`
}

// metricsAllEmpty 九个指标字段全为 null——采集层一个值都没给出（R3-空指标）。
func metricsAllEmpty(m *metricsIn) bool {
	return m.CPUPct == nil && m.MemUsed == nil && m.MemTotal == nil &&
		m.DiskUsed == nil && m.DiskTotal == nil &&
		m.NetRx == nil && m.NetTx == nil && m.UptimeS == nil && m.Load1 == nil
}

// validateMetrics 校验上报值域（审查 R1-#9）：cpu_pct ∈ [0,100]、load1 ≥ 0、
// 字节/秒数等整型非负；越界属协议违规，整条拒绝。
func validateMetrics(m *metricsIn) error {
	if m.CPUPct != nil {
		v := *m.CPUPct
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			return fmt.Errorf("cpu_pct out of range")
		}
	}
	if m.Load1 != nil {
		v := *m.Load1
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("load1 out of range")
		}
	}
	for name, v := range map[string]*int64{
		"mem_used": m.MemUsed, "mem_total": m.MemTotal,
		"disk_used": m.DiskUsed, "disk_total": m.DiskTotal,
		"net_rx": m.NetRx, "net_tx": m.NetTx, "uptime_s": m.UptimeS,
	} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%s negative", name)
		}
	}
	return nil
}

// sanitizeErrStr 去除控制字符并截断到 max 字节（rune 边界安全）。
func sanitizeErrStr(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sanitizeCollectErrors 净化客户端可控的 collect_errors：限制条目数与单条长度、
// 去控制字符；结构非法（条目超限/空 key/空 value）返回 ok=false 由调用方拒绝。
func sanitizeCollectErrors(in map[string]string) (map[string]string, bool) {
	if len(in) == 0 {
		return nil, true
	}
	if len(in) > maxCollectErrors {
		return nil, false
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = sanitizeErrStr(k, maxCollectErrKv)
		v = sanitizeErrStr(v, maxCollectErrKv)
		if k == "" || v == "" {
			return nil, false
		}
		out[k] = v
	}
	return out, true
}

func (h *Handler) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	node, _ := r.Context().Value(ctxKeyNode).(*store.Node)
	if node == nil {
		unauthorized(w)
		return
	}
	var req heartbeatReq
	if err := decodeJSONStrict(r, &req); err != nil {
		h.badRequest(w, err)
		return
	}
	// 节点 token 只能上报本节点数据。
	if strings.TrimSpace(req.Node) != node.Name {
		h.log.Warn("heartbeat node mismatch", "token_node", node.Name, "body_node", req.Node)
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := validateMetrics(&req.Metrics); err != nil {
		h.log.Warn("heartbeat metrics out of range", "node", node.Name, "reason", err.Error())
		httpError(w, http.StatusBadRequest, "bad_request")
		return
	}
	// 全空指标且无任何采集错误说明 → 400（R3）：正常 agent 心跳必带指标或
	// 错误说明，两者皆空的心跳只会在库里长出无法解读的空行。
	if metricsAllEmpty(&req.Metrics) && len(req.CollectErrors) == 0 {
		h.log.Warn("heartbeat with no metrics and no collect_errors", "node", node.Name)
		httpError(w, http.StatusBadRequest, "bad_request")
		return
	}

	collectErrors, ok := sanitizeCollectErrors(req.CollectErrors)
	if !ok {
		h.log.Warn("heartbeat collect_errors malformed", "node", node.Name)
		httpError(w, http.StatusBadRequest, "bad_request")
		return
	}
	errJSON := ""
	if len(collectErrors) > 0 {
		if b, err := json.Marshal(collectErrors); err == nil {
			errJSON = string(b)
		}
	}
	row := &store.MetricsRow{
		NodeID:        node.ID,
		TS:            time.Now().Unix(),
		CPUPct:        req.Metrics.CPUPct,
		MemUsed:       req.Metrics.MemUsed,
		MemTotal:      req.Metrics.MemTotal,
		DiskUsed:      req.Metrics.DiskUsed,
		DiskTotal:     req.Metrics.DiskTotal,
		NetRx:         req.Metrics.NetRx,
		NetTx:         req.Metrics.NetTx,
		UptimeS:       req.Metrics.UptimeS,
		Load1:         req.Metrics.Load1,
		CollectErrors: errJSON,
	}
	stOK, err := h.st.Heartbeat(r.Context(), row, sanitizeErrStr(req.AgentVersion, 64))
	if err != nil {
		h.log.Error("store heartbeat", "node", node.Name, "err", err)
		httpError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !stOK {
		// token 认证通过但节点行已消失（被删除）：视为凭据失效。
		unauthorized(w)
		return
	}
	if len(collectErrors) > 0 {
		// 只记 key 列表与条数，不记 value 原文（R3-附：错误说明不进日志，
		// 入库副本仍可在 metrics.collect_errors 里按需排查）。
		keys := make([]string, 0, len(collectErrors))
		for k := range collectErrors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		h.log.Warn("heartbeat with collect errors", "node", node.Name,
			"count", len(collectErrors), "keys", keys)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(heartbeatResp{Status: "ok", ServerTime: time.Now().Unix()})
}

// badRequest 统一处理请求体类错误：超限 413，其余 400；响应不带细节。
func (h *Handler) badRequest(w http.ResponseWriter, err error) {
	if errors.Is(err, errTooLarge) {
		httpError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	h.log.Warn("bad request body", "reason", err.Error())
	httpError(w, http.StatusBadRequest, "bad_request")
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
