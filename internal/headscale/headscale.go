// Package headscale 实现控制台对 Headscale 的只读集成（DESIGN §4.1-F、
// SPEC-M1b-b §3）：定期拉取 REST /api/v1/node，把 tailnet 节点状态全量替换
// 进本地 tailnet_nodes 表，并入 mesh 视图（MCP get_mesh_status 与 Web 面板）。
//
// 边界：只读，绝不代操作；HTTP client 超时 ≤10s；响应解析只取需要的字段
// （id/name/addresses/online/lastSeen），未知字段一律忽略——headscale 版本
// 演进（字段增删、id 数字/字符串形态差异）不破坏解析。拉取失败（含 HTTP 200
// 但 body 非法，R19-#1/R21-#1）保留旧数据，连续失败计数进日志（WARN，节流
// 窗口独立于计数 1/h，R21-#2；连续成功 3 次才确认恢复并归零计数，R19-#9）。
package headscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/store"
)

// fetchTimeout 单次拉取的 HTTP 超时上限（SPEC-M1b-b §3：≤10s）。
const fetchTimeout = 10 * time.Second

// maxRespBytes 成功响应 body 的字节上限（4MiB）：headscale 节点列表远小于此。
// 超限或读满上限仍未见结尾一律按 malformed 拒绝（R21-#1）——LimitReader 静默
// 截断会把「截断」伪装成「正常结束」，进而误当空列表清库。
const maxRespBytes = 4 << 20

// warnThrottle 连续失败 WARN 日志的节流窗口（1/h，防刷屏）。
const warnThrottle = time.Hour

// successResetStreak 失败计数归零所需的连续成功次数（R19-#9）：flapping 场景
// （失败→单次成功→失败…）下若单次成功即归零，每次新失败都会满足「首次失败
// 立即 WARN」再次突破节流；恢复必须连续成功确认后才归零失败计数。WARN 节流
// 窗口独立维护、不随恢复重置（R21-#2/R22-#2）——任意时刻相邻两条 WARN 间隔
// 仍 ≥ warnThrottle。
const successResetStreak = 3

// Client 为 headscale REST 只读客户端。
type Client struct {
	base   string // 如 http://127.0.0.1:8080，不带末尾斜杠
	apiKey string
	hc     *http.Client
}

// NewClient 构造；base 为 API 基地址（自动去末尾斜杠）。
func NewClient(base, apiKey string) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		apiKey: apiKey,
		// 超时在 client 层兜底：连接/读/写全路径受 10s 约束（调用方 ctx 之外
		// 的最后防线）。
		hc: &http.Client{Timeout: fetchTimeout},
	}
}

// Node 为解析后的 tailnet 节点最小字段集（只存表格需要的列）。
type Node struct {
	ID       int64
	Name     string
	IPs      []string
	Online   bool
	LastSeen *time.Time // headscale 未给出/为 null 时 nil
}

// wireResp/wireNode 只声明需要的字段；JSON 未知字段忽略。Nodes 用指针承载
// 「键存在性」：顶层 null / {} / 缺 nodes 键 / nodes:null 在 Decode 后均为
// nil——非法成功响应按拉取失败处理（调用方保留旧数据），只有显式 nodes 数组
// （含空数组）才全量替换（R19-#1：空列表清库是合法语义，非法响应清库是数据丢失）。
type wireResp struct {
	Nodes *[]wireNode `json:"nodes"`
}

type wireNode struct {
	ID   json.RawMessage `json:"id"` // 数字或字符串（版本差异），见 parseNodeID
	Name string          `json:"name"`
	// 节点 tailscale 地址字段名随 headscale 版本演进改名（SPEC-M1b-c §4 顺手修
	// 的 ips=null 根因）：新版为 `addresses`，旧版 proto 字段为 `ip_addresses`
	// （JSON `ipAddresses`）。两处都声明、取非空者——都是节点地址列表；
	// `availableRoutes` 等路由字段是子网路由（非 100.x 节点地址），刻意不采。
	Addresses   []string   `json:"addresses"`
	IpAddresses []string   `json:"ipAddresses"`
	Online      *bool      `json:"online"` // 旧版无此字段 → nil，按 offline 存但不误判为拉取失败
	LastSeen    *time.Time `json:"lastSeen"`
}

// parseNodeID 兼容 headscale 各版本的 id 形态（数字 / 整数字符串）。
func parseNodeID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, errors.New("node id missing")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, fmt.Errorf("node id: %w", err)
		}
		return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("node id: %w", err)
	}
	return n, nil
}

// FetchNodes 拉取并解析节点列表。非 200 / 解析失败一律返回错误（调用方保留
// 旧数据，不部分写入）。
func (c *Client) FetchNodes(ctx context.Context) ([]Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/node", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 错误说明截断 200 字节：够定位（token 无效/权限/版本路径变化），不吞响应体。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("headscale api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// body 先整读（≤4MiB，多读 1 字节探测截断）：读满上限仍未见结尾 = body
	// 超限/被截断，显式报 malformed——若直接在 LimitReader 上解码，截断点恰在
	// 完整值之后时二次 Decode 会返回 io.EOF，截断被伪装成正常结束（R21-#1）。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read headscale response: %w", err)
	}
	if len(body) > maxRespBytes {
		return nil, fmt.Errorf("headscale response exceeds %d bytes (malformed body)", maxRespBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var wire wireResp
	if err := dec.Decode(&wire); err != nil {
		// 中途截断（连接提前断开）显式报 malformed，不用裸 unexpected EOF 混同语法错。
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("headscale response truncated mid-value (malformed body): %w", err)
		}
		return nil, fmt.Errorf("decode headscale nodes: %w", err)
	}
	// 合法成功响应必须是 {"nodes":[...]} 对象（R19-#1）：顶层 null / {} / 缺
	// nodes 键均视为非法响应——返回错误走「保留旧数据」路径，绝不能被当成
	// 空列表清库。
	if wire.Nodes == nil {
		return nil, errors.New("headscale response missing \"nodes\" array (malformed body)")
	}
	// 严格 EOF 校验（registry decodeJSONStrict 同口径，R21-#1）：二次 Decode
	// 必须返回 io.EOF。dec.More() 对 `]`/`}` 起始的尾随垃圾返回 false，
	// `{"nodes":[]}]` 之类会被静默放过，故不用 More()。
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("headscale response has trailing data after JSON value (malformed body)")
	}
	nodes := *wire.Nodes
	out := make([]Node, 0, len(nodes))
	for i := range nodes {
		wn := &nodes[i]
		id, err := parseNodeID(wn.ID)
		if err != nil {
			return nil, fmt.Errorf("node[%d]: %w", i, err)
		}
		if wn.Name == "" {
			return nil, fmt.Errorf("node[%d]: empty machine name", i)
		}
		// 地址取两代字段名中非空者（都为空 = 该版本确实未给地址，如实存空串）。
		ips := wn.Addresses
		if len(ips) == 0 {
			ips = wn.IpAddresses
		}
		out = append(out, Node{
			ID:       id,
			Name:     wn.Name,
			IPs:      ips,
			Online:   wn.Online != nil && *wn.Online,
			LastSeen: wn.LastSeen,
		})
	}
	return out, nil
}

// rowsToStore 把解析结果映射为表行（ips 逗号连接，保序）。
func rowsToStore(nodes []Node, now int64) []store.TailnetNodeRow {
	rows := make([]store.TailnetNodeRow, 0, len(nodes))
	for _, n := range nodes {
		row := store.TailnetNodeRow{
			ID:          n.ID,
			MachineName: n.Name,
			IPs:         strings.Join(n.IPs, ","),
			Online:      n.Online,
			UpdatedAt:   now,
		}
		if n.LastSeen != nil {
			row.LastSeen.Valid = true
			row.LastSeen.Int64 = n.LastSeen.Unix()
		}
		rows = append(rows, row)
	}
	return rows
}

// Fetcher 把「拉取 → 全量替换」封装为可单测的单元（fetch 可注入）。
type Fetcher struct {
	client *Client
	st     *store.Store
	// fetch 可注入（测试用 canned 响应/错误）；nil 取 client.FetchNodes。
	fetch func(ctx context.Context) ([]Node, error)
	// fails/streak/lastWarn 为 Run 循环的节流状态：fails 连续失败计数、
	// streak 归零前的连续成功计数（R19-#9）、lastWarn 上次 WARN 时刻。
	fails    int
	streak   int
	lastWarn time.Time
}

// NewFetcher 构造。
func NewFetcher(client *Client, st *store.Store) *Fetcher {
	return &Fetcher{client: client, st: st}
}

// Sync 执行一次拉取与全量替换。返回本次写入行数；任何失败不触库（旧数据原样）。
func (f *Fetcher) Sync(ctx context.Context) (int, error) {
	fetch := f.fetch
	if fetch == nil {
		fetch = f.client.FetchNodes
	}
	nodes, err := fetch(ctx)
	if err != nil {
		return 0, err
	}
	rows := rowsToStore(nodes, time.Now().Unix())
	if err := f.st.ReplaceTailnetNodes(ctx, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// handleResult 处理一次同步结果（从 Run 抽出以便单测直测节流状态机）：
// 失败计数递增；WARN 节流窗口独立于失败计数/streak（R21-#2）——只看上次
// WARN 时刻，任意相邻两条 WARN 间隔 ≥1h，「失败→恢复→失败」循环不再借
// 计数归零后的 fails==1 立即告警突破上限，窗口内的失败降级 Debug。失败
// 计数在连续成功 successResetStreak 次后才归零并 INFO 收尾（R19-#9），
// 恢复 INFO 不受节流限制。ctx 取消时不记账（进程正在退出）。
func (f *Fetcher) handleResult(ctx context.Context, n int, err error, log *slog.Logger) {
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		f.streak = 0
		f.fails++
		// 节流只看上次 WARN 时刻（lastWarn 零值 → 首个失败立即告警）。
		if time.Since(f.lastWarn) >= warnThrottle {
			log.Warn("headscale sync failed (keeping stale tailnet data)",
				"consecutive_failures", f.fails, "err", err)
			f.lastWarn = time.Now()
		} else {
			log.Debug("headscale sync failed (warn throttled)",
				"consecutive_failures", f.fails, "err", err)
		}
		return
	}
	if f.fails == 0 {
		log.Debug("headscale sync", "nodes", n)
		return
	}
	f.streak++
	if f.streak >= successResetStreak {
		log.Info("headscale sync recovered", "after_failures", f.fails, "nodes", n)
		f.fails = 0
		f.streak = 0
	} else {
		log.Debug("headscale sync ok (recovery pending)",
			"nodes", n, "consecutive_successes", f.streak)
	}
}

// Run 周期同步循环（启动即拉一次，此后按 interval 周期）。拉取失败保留旧数据，
// 连续失败计数；WARN 节流 1/h（窗口独立于计数/streak，任意相邻两条 WARN 间隔
// ≥1h，R21-#2），失败计数在连续成功 3 次后才归零并 INFO 一条收尾（R19-#9）。
// ctx 取消即返回。
func (f *Fetcher) Run(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	sync := func() {
		n, err := f.Sync(ctx)
		f.handleResult(ctx, n, err, log)
	}

	sync()
	log.Info("headscale fetch loop started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sync()
		}
	}
}
