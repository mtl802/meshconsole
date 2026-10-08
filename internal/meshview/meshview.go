// Package meshview 为只读 mesh 视图服务层（SPEC-M1b-b §2/§4）：MCP 五个工具
// 与 Web 面板 overview 端点的同源数据出口——SQL 全部经 internal/store，本层只
// 做组装、过滤与派生（不复制查询、不触任何写路径）。全部输出结构带 JSON tag，
// 即 MCP 结构化结果与面板 JSON 的契约。
package meshview

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/store"
)

// serviceIssueGrace 服务异常判定的数据新鲜宽限：非 active 状态行仅在
// updated_at 距今不超过该窗口时计入异常名单——超过宽限未更新的行视为离线节点
// 的陈旧数据，不再当现行异常（避免把断连节点的旧状态当服务故障）。
const serviceIssueGrace = 300 * time.Second

// metricsWindow get_node 指标摘要回看窗口（SPEC-M1b-b §2：最近 24h）。
const metricsWindow = 24 * time.Hour

// sparkPoints 面板 sparkline 采样点数上限（取窗口内最近样本）。
const sparkPoints = 48

// Query 为只读视图查询入口。
type Query struct {
	st *store.Store
}

func New(st *store.Store) *Query { return &Query{st: st} }

// ---- 视图类型（JSON 契约）----

// Node 为节点行视图；时间为 unix 秒。ID 为 nodes 表主键（R19-#2：SPEC §2
// list_nodes 契约含 id，MCP 与面板消费方都靠它关联）。
type Node struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Status       string `json:"status"`
	AgentVersion string `json:"agent_version,omitempty"`
	TailnetIP    string `json:"tailnet_ip,omitempty"`
	PublicIP     string `json:"public_ip,omitempty"`
	LastSeen     *int64 `json:"last_seen,omitempty"`
	LastSuccess  *int64 `json:"last_success,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// Service 为受管服务行视图。
type Service struct {
	Node      string `json:"node"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Target    string `json:"target,omitempty"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// Agent 为 AI agent 行视图；Invokable 恒 false（发现 ≠ 可调用，DESIGN §4.2-B）。
type Agent struct {
	Node      string `json:"node"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Version   string `json:"version,omitempty"`
	Path      string `json:"path,omitempty"`
	Status    string `json:"status"`
	Invokable bool   `json:"invokable"`
	Detail    string `json:"detail,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// TailnetNode 为 tailnet_nodes 行视图（Headscale 只读镜像）。
type TailnetNode struct {
	ID        int64    `json:"id"`
	Name      string   `json:"machine_name"`
	IPs       []string `json:"ips"`
	Online    bool     `json:"online"`
	LastSeen  *int64   `json:"last_seen,omitempty"`
	UpdatedAt int64    `json:"updated_at"`
}

// MetricsSummary 为时间窗口内的指标摘要；nil = 窗口内无该指标数据（不填 0）。
type MetricsSummary struct {
	WindowSeconds int64    `json:"window_seconds"`
	Samples       int      `json:"samples"`
	CPUAvg        *float64 `json:"cpu_avg,omitempty"`
	CPUMax        *float64 `json:"cpu_max,omitempty"`
	MemPctAvg     *float64 `json:"mem_pct_avg,omitempty"`
	MemPctMax     *float64 `json:"mem_pct_max,omitempty"`
	DiskPctMax    *float64 `json:"disk_pct_max,omitempty"`
}

// LatestMetrics 为节点最新一条指标行。
type LatestMetrics struct {
	TS        int64    `json:"ts"`
	CPUPct    *float64 `json:"cpu_pct,omitempty"`
	Load1     *float64 `json:"load1,omitempty"`
	MemUsed   *int64   `json:"mem_used,omitempty"`
	MemTotal  *int64   `json:"mem_total,omitempty"`
	DiskUsed  *int64   `json:"disk_used,omitempty"`
	DiskTotal *int64   `json:"disk_total,omitempty"`
	NetRx     *int64   `json:"net_rx,omitempty"`
	NetTx     *int64   `json:"net_tx,omitempty"`
	UptimeS   *int64   `json:"uptime_s,omitempty"`
}

// SparkPoint 为 sparkline 单点。
type SparkPoint struct {
	TS     int64    `json:"ts"`
	CPUPct *float64 `json:"cpu_pct,omitempty"`
	MemPct *float64 `json:"mem_pct,omitempty"`
}

// NodeDetail 为 get_node 输出：单节点 + 24h 摘要 + 最新指标 + 服务/agent 清单。
type NodeDetail struct {
	Node     Node            `json:"node"`
	Metrics  *MetricsSummary `json:"metrics,omitempty"`
	Latest   *LatestMetrics  `json:"latest,omitempty"`
	Services []Service       `json:"services"`
	Agents   []Agent         `json:"agents"`
}

// NodeCard 为面板节点卡数据：节点 + 24h 指标摘要 + 最新指标 + sparkline 采样。
type NodeCard struct {
	Node    Node            `json:"node"`
	Metrics *MetricsSummary `json:"metrics,omitempty"`
	Latest  *LatestMetrics  `json:"latest,omitempty"`
	Spark   []SparkPoint    `json:"spark,omitempty"`
}

// ServiceIssue 为 mesh 现行异常服务条目（非 active 且数据未过宽限）。
type ServiceIssue struct {
	Node      string `json:"node"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	UpdatedAt int64  `json:"updated_at"`
}

// TailnetSummary 为 headscale 拉取数据概况。
type TailnetSummary struct {
	Tracked   int           `json:"tracked"`
	Online    int           `json:"online"`
	UpdatedAt *int64        `json:"updated_at,omitempty"` // 最近一次成功替换时刻
	Nodes     []TailnetNode `json:"nodes"`
}

// MeshStatus 为 get_mesh_status 输出（全网汇总）。
type MeshStatus struct {
	GeneratedAt   int64           `json:"generated_at"`
	NodesTotal    int             `json:"nodes_total"`
	NodesOnline   int             `json:"nodes_online"`
	OfflineNodes  []string        `json:"offline_nodes"`
	ServiceIssues []ServiceIssue  `json:"service_issues"`
	Tailnet       *TailnetSummary `json:"tailnet,omitempty"`
	// Freshness 为库内最新 metrics 时刻（unix 秒；无任何指标时缺省）。
	Freshness *int64 `json:"freshness,omitempty"`
}

// Overview 为面板聚合输出（SPEC-M1b-b §4.1：nodes/services/agents/tailnet/
// metrics 摘要一次取齐）。ServiceIssues 为现行异常服务名单——与
// get_mesh_status 同一口径（status != active、非 stale、未过 300s 宽限），
// 由 service 层算好输出（R19-#4：前端只消费名单，不做业务计算）。
type Overview struct {
	GeneratedAt   int64           `json:"generated_at"`
	Freshness     *int64          `json:"freshness,omitempty"`
	Nodes         []NodeCard      `json:"nodes"`
	Services      []Service       `json:"services"`
	ServiceIssues []ServiceIssue  `json:"service_issues"`
	Agents        []Agent         `json:"agents"`
	Tailnet       *TailnetSummary `json:"tailnet,omitempty"`
}

// ---- 内部转换 ----

func nodeView(n *store.NodeRecord) Node {
	nv := Node{
		ID:   n.ID,
		Name: n.Name, Role: n.Role, OS: n.OS, Arch: n.Arch, Status: n.Status,
		AgentVersion: n.AgentVersion, TailnetIP: n.TailnetIP, PublicIP: n.PublicIP,
		CreatedAt: n.CreatedAt,
	}
	if n.LastSeen.Valid {
		v := n.LastSeen.Int64
		nv.LastSeen = &v
	}
	if n.LastSuccess.Valid {
		v := n.LastSuccess.Int64
		nv.LastSuccess = &v
	}
	return nv
}

func serviceView(nodeName string, r store.ServiceRecord) Service {
	return Service{Node: nodeName, Name: r.Name, Type: r.Type, Target: r.Target,
		Status: r.Status, Detail: r.Detail, UpdatedAt: r.UpdatedAt}
}

func agentView(nodeName string, r store.AgentRecord) Agent {
	return Agent{Node: nodeName, Name: r.Name, Type: r.Type, Version: r.Version,
		Path: r.Path, Status: r.Status, Invokable: r.Invokable, Detail: r.Detail,
		UpdatedAt: r.UpdatedAt}
}

func tailnetView(r store.TailnetNodeRecord) TailnetNode {
	tn := TailnetNode{ID: r.ID, Name: r.MachineName, Online: r.Online, UpdatedAt: r.UpdatedAt}
	if r.IPs != "" {
		tn.IPs = strings.Split(r.IPs, ",")
	}
	if r.LastSeen.Valid {
		v := r.LastSeen.Int64
		tn.LastSeen = &v
	}
	return tn
}

func latestView(rec *store.MetricsRecord) *LatestMetrics {
	lm := &LatestMetrics{TS: rec.TS}
	if rec.CPUPct.Valid {
		v := rec.CPUPct.Float64
		lm.CPUPct = &v
	}
	if rec.Load1.Valid {
		v := rec.Load1.Float64
		lm.Load1 = &v
	}
	if rec.MemUsed.Valid {
		v := rec.MemUsed.Int64
		lm.MemUsed = &v
	}
	if rec.MemTotal.Valid {
		v := rec.MemTotal.Int64
		lm.MemTotal = &v
	}
	if rec.DiskUsed.Valid {
		v := rec.DiskUsed.Int64
		lm.DiskUsed = &v
	}
	if rec.DiskTotal.Valid {
		v := rec.DiskTotal.Int64
		lm.DiskTotal = &v
	}
	if rec.NetRx.Valid {
		v := rec.NetRx.Int64
		lm.NetRx = &v
	}
	if rec.NetTx.Valid {
		v := rec.NetTx.Int64
		lm.NetTx = &v
	}
	if rec.UptimeS.Valid {
		v := rec.UptimeS.Int64
		lm.UptimeS = &v
	}
	return lm
}

func nullF(f sql.NullFloat64) *float64 {
	if !f.Valid {
		return nil
	}
	v := f.Float64
	return &v
}

// statsView 把 store 的 24h 摘要行转为视图（get_node 与 overview 共用，
// 查询一律复用 store.MetricsStatsSince，不新写 SQL）。
func statsView(st *store.MetricsStats) *MetricsSummary {
	return &MetricsSummary{
		WindowSeconds: st.WindowSeconds, Samples: st.Samples,
		CPUAvg: nullF(st.CPUAvg), CPUMax: nullF(st.CPUMax),
		MemPctAvg: nullF(st.MemPctAvg), MemPctMax: nullF(st.MemPctMax),
		DiskPctMax: nullF(st.DiskPctMax),
	}
}

// serviceIsIssue 报告服务行是否为现行异常：status != active、非 stale 簿记态、
// 且数据未过 300s 宽限（超过宽限未更新的行视为离线节点遗留数据，不当现行
// 故障）。get_mesh_status 与面板 overview 共用此唯一口径（R19-#4）。
func serviceIsIssue(r store.ServiceRecord, cutoff int64) bool {
	return r.Status != "active" && r.Status != "stale" && r.UpdatedAt >= cutoff
}

// sortIssues 异常名单按（节点, 服务名）稳定排序。
func sortIssues(issues []ServiceIssue) {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Node != issues[j].Node {
			return issues[i].Node < issues[j].Node
		}
		return issues[i].Name < issues[j].Name
	})
}

// ---- 查询方法 ----

// Nodes 列出全部节点（按名称排序）。
func (q *Query) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := q.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(rows))
	for i := range rows {
		out = append(out, nodeView(&rows[i]))
	}
	return out, nil
}

// NodeDetail 取单节点详情（含 24h 指标摘要、最新指标、服务与 agent 清单）。
// 节点不存在返回 nil, nil。
func (q *Query) NodeDetail(ctx context.Context, name string) (*NodeDetail, error) {
	n, err := q.st.GetNodeByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, nil
	}
	rec := &store.NodeRecord{
		ID: n.ID, Name: n.Name, Role: n.Role, OS: n.OS, Arch: n.Arch,
		TailnetIP: n.TailnetIP, PublicIP: n.PublicIP, AgentVersion: n.AgentVersion,
		Status: n.Status, LastSeen: n.LastSeen, LastSuccess: n.LastSuccess, CreatedAt: n.CreatedAt,
	}
	detail := &NodeDetail{Node: nodeView(rec)}

	svcs, err := q.st.ListServices(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	detail.Services = make([]Service, 0, len(svcs))
	for _, s := range svcs {
		detail.Services = append(detail.Services, serviceView(n.Name, s))
	}
	ags, err := q.st.ListAgents(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	detail.Agents = make([]Agent, 0, len(ags))
	for _, a := range ags {
		detail.Agents = append(detail.Agents, agentView(n.Name, a))
	}
	since := time.Now().Add(-metricsWindow).Unix()
	st, err := q.st.MetricsStatsSince(ctx, n.ID, since)
	if err != nil {
		return nil, err
	}
	detail.Metrics = statsView(st)
	latest, err := q.st.LatestMetrics(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		detail.Latest = latestView(latest)
	}
	return detail, nil
}

// Services 列出服务行；nodeName 为空返回全网。未知节点返回空清单（工具语义友好）。
func (q *Query) Services(ctx context.Context, nodeName string) ([]Service, error) {
	rows, err := q.st.ListAllServices(ctx)
	if err != nil {
		return nil, err
	}
	names, err := q.st.NodeNameIDMap(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Service, 0, len(rows))
	for _, r := range rows {
		name, ok := names[r.NodeID]
		if !ok || (nodeName != "" && name != nodeName) {
			continue
		}
		out = append(out, serviceView(name, r))
	}
	return out, nil
}

// Agents 列出 AI agent 行；nodeName 为空返回全网。
func (q *Query) Agents(ctx context.Context, nodeName string) ([]Agent, error) {
	rows, err := q.st.ListAllAgents(ctx)
	if err != nil {
		return nil, err
	}
	names, err := q.st.NodeNameIDMap(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(rows))
	for _, r := range rows {
		name, ok := names[r.NodeID]
		if !ok || (nodeName != "" && name != nodeName) {
			continue
		}
		out = append(out, agentView(name, r))
	}
	return out, nil
}

// Status 汇总全网状态（get_mesh_status 输出）。
func (q *Query) Status(ctx context.Context) (*MeshStatus, error) {
	nodes, err := q.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	names, err := q.st.NodeNameIDMap(ctx)
	if err != nil {
		return nil, err
	}
	out := &MeshStatus{
		GeneratedAt: time.Now().Unix(),
		// 空清单序列化为 [] 而非 null（MCP/面板消费方少一层特判）。
		OfflineNodes:  []string{},
		ServiceIssues: []ServiceIssue{},
	}
	for i := range nodes {
		n := &nodes[i]
		out.NodesTotal++
		if n.Status == "online" {
			out.NodesOnline++
		} else {
			out.OfflineNodes = append(out.OfflineNodes, n.Name)
		}
	}
	// 服务异常口径（SPEC-M1b-b §2，R19-#4 统一在 service 层）：status != active
	// 且数据未过宽限。stale 为「配置中已移除」的簿记态，不计运行异常；超出
	// 宽限未更新的行视为离线节点的陈旧数据，同样不计。
	issues, err := q.st.ListAllServices(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-serviceIssueGrace).Unix()
	for _, r := range issues {
		nodeName, ok := names[r.NodeID]
		if !ok || !serviceIsIssue(r, cutoff) {
			continue
		}
		out.ServiceIssues = append(out.ServiceIssues, ServiceIssue{
			Node: nodeName, Name: r.Name, Status: r.Status, UpdatedAt: r.UpdatedAt,
		})
	}
	sortIssues(out.ServiceIssues)
	if ts, ok, err := q.st.LatestMetricsTime(ctx); err != nil {
		return nil, err
	} else if ok {
		out.Freshness = &ts
	}
	tn, err := q.tailnet(ctx)
	if err != nil {
		return nil, err
	}
	out.Tailnet = tn
	return out, nil
}

// Overview 面板聚合：节点卡（含 24h 摘要、最新指标与 sparkline）+ 服务 +
// 现行异常名单 + agent + tailnet。
func (q *Query) Overview(ctx context.Context) (*Overview, error) {
	nodes, err := q.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := &Overview{
		GeneratedAt:   time.Now().Unix(),
		Nodes:         []NodeCard{},
		ServiceIssues: []ServiceIssue{},
	}
	since := time.Now().Add(-metricsWindow).Unix()
	for i := range nodes {
		n := &nodes[i]
		card := NodeCard{Node: nodeView(n)}
		// 24h 摘要复用 store 既有 MetricsStatsSince 查询（R19-#3：不新写 SQL）。
		st, err := q.st.MetricsStatsSince(ctx, n.ID, since)
		if err != nil {
			return nil, err
		}
		card.Metrics = statsView(st)
		rec, err := q.st.LatestMetrics(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			card.Latest = latestView(rec)
		}
		pts, err := q.st.MetricsSeriesSince(ctx, n.ID, since, sparkPoints)
		if err != nil {
			return nil, err
		}
		if len(pts) > 0 {
			card.Spark = make([]SparkPoint, 0, len(pts))
			for _, p := range pts {
				card.Spark = append(card.Spark, SparkPoint{TS: p.TS, CPUPct: nullF(p.CPUPct), MemPct: nullF(p.MemPct)})
			}
		}
		out.Nodes = append(out.Nodes, card)
	}
	// 服务行与现行异常名单同源一次取齐（同一份 ListAllServices；异常口径
	// serviceIsIssue 与 get_mesh_status 完全一致，R19-#4）。
	svcRows, err := q.st.ListAllServices(ctx)
	if err != nil {
		return nil, err
	}
	names, err := q.st.NodeNameIDMap(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-serviceIssueGrace).Unix()
	out.Services = make([]Service, 0, len(svcRows))
	for _, r := range svcRows {
		name, ok := names[r.NodeID]
		if !ok {
			continue
		}
		out.Services = append(out.Services, serviceView(name, r))
		if serviceIsIssue(r, cutoff) {
			out.ServiceIssues = append(out.ServiceIssues, ServiceIssue{
				Node: name, Name: r.Name, Status: r.Status, UpdatedAt: r.UpdatedAt,
			})
		}
	}
	sortIssues(out.ServiceIssues)
	if out.Agents, err = q.Agents(ctx, ""); err != nil {
		return nil, err
	}
	if out.Tailnet, err = q.tailnet(ctx); err != nil {
		return nil, err
	}
	if ts, ok, err := q.st.LatestMetricsTime(ctx); err != nil {
		return nil, err
	} else if ok {
		out.Freshness = &ts
	}
	return out, nil
}

// tailnet 组装 tailnet 概况；无数据（集成禁用/尚未拉到）返回 nil。
func (q *Query) tailnet(ctx context.Context) (*TailnetSummary, error) {
	rows, err := q.st.ListTailnetNodes(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	sum := &TailnetSummary{Tracked: len(rows), Nodes: []TailnetNode{}}
	for _, r := range rows {
		if r.Online {
			sum.Online++
		}
		if sum.UpdatedAt == nil || r.UpdatedAt > *sum.UpdatedAt {
			v := r.UpdatedAt
			sum.UpdatedAt = &v
		}
		sum.Nodes = append(sum.Nodes, tailnetView(r))
	}
	return sum, nil
}
