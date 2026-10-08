package mcpserver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/mcpserver"
	"github.com/mtl802/meshconsole/internal/store"
)

// newSession 在内存传输上起 MCP server 会话（client.Connect 即完成 initialize
// 握手）。库预置：在线节点 cloud-1（metrics + 服务 + agent + 运行任务快照）。
func newSession(t *testing.T) *mcp.ClientSession { return newSessionTasksTrunc(t, false) }

// newSessionTasksTrunc 同 newSession，但可指定节点任务快照的截断标记落库值
// （nodes.tasks_truncated，R27-#4 链路）。
func newSessionTasksTrunc(t *testing.T, tasksTruncated bool) *mcp.ClientSession {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	n, err := st.RegisterNode(ctx, "cloud-1", "server", "linux", "amd64", "h1", "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	cpu := 42.5
	_, err = st.HeartbeatFull(ctx, &store.MetricsRow{
		NodeID: n.ID, TS: time.Now().Unix(), CPUPct: &cpu,
		MemUsed: intp(1 << 30), MemTotal: intp(4 << 30),
	}, "agent-1",
		&[]store.ServiceRow{{Name: "headscale", Type: "systemd", Target: "headscale.service", Status: "active"}},
		&[]store.AgentRow{{Name: "zcode", Type: "cli", Version: "3.14.4", Status: "active"}},
		&[]store.AgentTaskRow{{
			PID: 4242, AgentName: "zcode", Cmd: "zcode m1b-c dev", ElapsedS: 120,
			CPUPct: fltp(12.5), StartedAt: sqlNullInt(time.Now().Unix() - 120),
		}}, tasksTruncated, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	server := mcpserver.New(st, &config.Console{}, "test", nil)
	ct, stt := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, stt, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).
		Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func intp(i int64) *int64 { return &i }

func fltp(f float64) *float64 { return &f }

func sqlNullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// structured 把结果的结构化内容解码进 out（顶层对象 → 传址字段）。
func structured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	if res == nil {
		t.Fatal("nil result")
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode structured content %s: %v", b, err)
	}
}

// TestHandshakeToolsList initialize（Connect 隐含）+ tools/list：只读工具
// 齐备，描述非空（SPEC §7：MCP 三方法握手之「握手 + 列表」；M1b-c §2.2 新增
// list_agent_tasks；M1d §3.5 新增 list_active_sessions）。
func TestHandshakeToolsList(t *testing.T) {
	cs := newSession(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"list_nodes": false, "get_node": false, "list_services": false,
		"list_agents": false, "list_agent_tasks": false, "get_mesh_status": false,
		"list_active_sessions": false,
		// M1d 命令三工具（submit_command 的 scope 拒绝另行覆盖）。
		"get_command": false, "list_commands": false, "submit_command": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
			if tool.Description == "" {
				t.Fatalf("tool %s: empty description", tool.Name)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("tool %s missing from tools/list", name)
		}
	}
	if len(res.Tools) != len(want) {
		t.Fatalf("tools = %d, want exactly %d (只读 + 命令查询，M1d §3.5 后共 10)", len(res.Tools), len(want))
	}
}

// TestCallListNodes tools/call list_nodes → 部署库真数据（节点行 + 状态 + 版本）。
func TestCallListNodes(t *testing.T) {
	cs := newSession(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_nodes"})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Nodes []map[string]any `json:"nodes"`
	}
	structured(t, res, &out)
	if len(out.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(out.Nodes))
	}
	n := out.Nodes[0]
	if n["name"] != "cloud-1" || n["status"] != "online" || n["os"] != "linux" {
		t.Fatalf("node = %+v", n)
	}
	if _, ok := n["last_seen"]; !ok {
		t.Fatalf("last_seen missing (最近心跳时间必须暴露): %+v", n)
	}
}

// TestCallGetNode get_node → 详情 + 24h 摘要 + 服务/agent 清单；未知节点报
// 工具错误（IsError），不 panic。
func TestCallGetNode(t *testing.T) {
	cs := newSession(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_node",
		Arguments: map[string]any{"name": "cloud-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Node struct {
			Node     map[string]any   `json:"node"`
			Metrics  map[string]any   `json:"metrics"`
			Services []map[string]any `json:"services"`
			Agents   []map[string]any `json:"agents"`
		} `json:"node"`
	}
	structured(t, res, &out)
	if out.Node.Node["name"] != "cloud-1" {
		t.Fatalf("detail = %+v", out.Node)
	}
	if out.Node.Metrics == nil || out.Node.Metrics["samples"] == nil {
		t.Fatalf("24h metrics summary missing: %+v", out.Node.Metrics)
	}
	if len(out.Node.Services) != 1 || len(out.Node.Agents) != 1 {
		t.Fatalf("services=%d agents=%d", len(out.Node.Services), len(out.Node.Agents))
	}
	if out.Node.Agents[0]["invokable"] != false {
		t.Fatalf("invokable must be false: %+v", out.Node.Agents[0])
	}

	// 未知节点 → 工具错误（IsError + 文本说明），提示用 list_nodes。
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_node",
		Arguments: map[string]any{"name": "ghost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("unknown node must be a tool error: %+v", res)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "ghost") {
		t.Fatalf("error text should name the node, got %q", text.String())
	}
}

// TestCallListServicesAgentsFiltered list_services / list_agents 按节点过滤与
// 空清单语义（未知节点 → 空数组而非错误）。
func TestCallListServicesAgentsFiltered(t *testing.T) {
	cs := newSession(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_services",
		Arguments: map[string]any{"node": "cloud-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var svcOut struct {
		Services []map[string]any `json:"services"`
	}
	structured(t, res, &svcOut)
	if len(svcOut.Services) != 1 || svcOut.Services[0]["name"] != "headscale" {
		t.Fatalf("services = %+v", svcOut.Services)
	}
	if svcOut.Services[0]["node"] != "cloud-1" {
		t.Fatalf("service must carry node name: %+v", svcOut.Services[0])
	}

	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_agents",
		Arguments: map[string]any{"node": "no-such-node"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var agOut struct {
		Agents []map[string]any `json:"agents"`
	}
	structured(t, res, &agOut)
	if len(agOut.Agents) != 0 {
		t.Fatalf("unknown node must give empty list, got %+v", agOut.Agents)
	}
}

// TestCallListAgentTasks list_agent_tasks → 预置任务真数据 + 节点过滤 + 未知节点
// 空清单语义（SPEC-M1b-c §2.2/§5）。
func TestCallListAgentTasks(t *testing.T) {
	cs := newSession(t)

	// 无过滤：返回预置任务（zcode pid=4242）。
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_agent_tasks"})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Tasks          []map[string]any `json:"tasks"`
		TasksTruncated bool             `json:"tasks_truncated"`
	}
	structured(t, res, &out)
	if len(out.Tasks) != 1 {
		t.Fatalf("tasks = %+v, want 1", out.Tasks)
	}
	if out.TasksTruncated {
		t.Fatal("untruncated snapshot must report tasks_truncated=false (键恒在)")
	}
	task := out.Tasks[0]
	if task["node"] != "cloud-1" || task["agent_name"] != "zcode" {
		t.Fatalf("task = %+v", task)
	}
	if task["pid"].(float64) != 4242 || task["elapsed_s"].(float64) != 120 {
		t.Fatalf("task pid/elapsed = %+v", task)
	}
	if task["cpu_pct"].(float64) != 12.5 {
		t.Fatalf("task cpu = %+v", task)
	}
	if _, ok := task["started_at"]; !ok {
		t.Fatalf("started_at must be exposed when known: %+v", task)
	}

	// 按节点过滤：未知节点 → 空数组（与 list_services/list_agents 同语义）。
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_agent_tasks",
		Arguments: map[string]any{"node": "no-such-node"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var filtered struct {
		Tasks []map[string]any `json:"tasks"`
	}
	structured(t, res, &filtered)
	if len(filtered.Tasks) != 0 {
		t.Fatalf("unknown node must give empty list, got %+v", filtered.Tasks)
	}
}

// TestCallListAgentTasksTruncated 截断标记透出（R29-#3）：节点清单触顶 64 条
// 截断落库（nodes.tasks_truncated=true）后，工具返回 tasks_truncated=true——
// MCP 用户须知清单仅为前 64 条；按节点过滤取该节点自己的标记（无涉及节点
// 为 false）。
func TestCallListAgentTasksTruncated(t *testing.T) {
	cs := newSessionTasksTrunc(t, true)

	// 无过滤：涉及节点（cloud-1）截断 → true。
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_agent_tasks"})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Tasks          []map[string]any `json:"tasks"`
		TasksTruncated bool             `json:"tasks_truncated"`
	}
	structured(t, res, &out)
	if len(out.Tasks) != 1 || !out.TasksTruncated {
		t.Fatalf("tasks=%d truncated=%v, want 1 task + truncated=true", len(out.Tasks), out.TasksTruncated)
	}

	// 过滤到无涉及节点（未知节点）：并集为空 → false（键仍在）。
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_agent_tasks",
		Arguments: map[string]any{"node": "no-such-node"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var filtered struct {
		Tasks          []map[string]any `json:"tasks"`
		TasksTruncated bool             `json:"tasks_truncated"`
	}
	structured(t, res, &filtered)
	if len(filtered.Tasks) != 0 || filtered.TasksTruncated {
		t.Fatalf("empty node set must give truncated=false, got %+v", filtered)
	}
}

// TestCallGetMeshStatus get_mesh_status → 汇总字段齐备（在线/离线/异常/tailnet
// 空缺时省略）。
func TestCallGetMeshStatus(t *testing.T) {
	cs := newSession(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_mesh_status"})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Status struct {
			NodesTotal  int     `json:"nodes_total"`
			NodesOnline int     `json:"nodes_online"`
			Freshness   *int64  `json:"freshness"`
			Tailnet     *string `json:"tailnet"`
		} `json:"status"`
	}
	structured(t, res, &out)
	if out.Status.NodesTotal != 1 || out.Status.NodesOnline != 1 {
		t.Fatalf("status = %+v", out.Status)
	}
	if out.Status.Freshness == nil {
		t.Fatal("freshness missing")
	}
}
