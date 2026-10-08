// Package mcpserver 把 mesh 只读状态暴露为 MCP 工具（SPEC-M1b-b §2、
// SPEC-M1b-c §2.2 新增 list_agent_tasks）。
//
// 传输：stdio（JSON-RPC 2.0，MCP 规范语义由官方 go-sdk 保证），不监听任何端口
// ——网络暴露为零（M3 再议 HTTP+SSE）。只读：数据出口全部经 internal/meshview
// （与 Web 面板同源），底层句柄为 store.OpenReadOnly（?mode=ro + query_only），
// 不存在任何写路径。
//
// stdin/stdout 是 JSON-RPC 通道：任何日志一律走 stderr（slog JSON → stderr），
// 绝不污染协议帧。
package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mtl802/meshconsole/internal/meshview"
	"github.com/mtl802/meshconsole/internal/store"
)

// serverImpl 为 MCP 实现标识（initialize 响应里的 serverInfo）。
const serverImplName = "meshconsole"

// listNodesOut / getNodeOut 等包装结构：结构化输出顶层一律为对象（MCP
// structuredContent 与 outputSchema 的稳妥形态），数组作为字段承载。
type (
	listNodesOut struct {
		Nodes []meshview.Node `json:"nodes"`
	}
	getNodeOut struct {
		Node *meshview.NodeDetail `json:"node"`
	}
	listServicesOut struct {
		Services []meshview.Service `json:"services"`
	}
	listAgentsOut struct {
		Agents []meshview.Agent `json:"agents"`
	}
	listAgentTasksOut struct {
		Tasks []meshview.AgentTask `json:"tasks"`
		// TasksTruncated 为涉及节点的截断标记并集（R29-#3）：任一节点的任务
		// 清单触顶单拍 64 条上限即 true——tasks 只是前 64 条的不完整快照。
		// 恒输出（false = 完整清单），消费方无需缺席特判。
		TasksTruncated bool `json:"tasks_truncated"`
	}
	getMeshStatusOut struct {
		Status *meshview.MeshStatus `json:"status"`
	}
)

// get_node 等工具的入参。
type (
	getNodeIn struct {
		Name string `json:"name" jsonschema:"节点名称（nodes 表 name 列）"`
	}
	listServicesIn struct {
		// Node 可选：按节点名过滤；缺省返回全网。
		Node string `json:"node,omitempty" jsonschema:"可选，按节点名过滤"`
	}
	listAgentsIn struct {
		Node string `json:"node,omitempty" jsonschema:"可选，按节点名过滤"`
	}
	listAgentTasksIn struct {
		Node string `json:"node,omitempty" jsonschema:"可选，按节点名过滤"`
	}
)

// New 构造注册好五个只读工具的 MCP server。
func New(st *store.Store, version string, opts *mcp.ServerOptions) *mcp.Server {
	if opts == nil {
		opts = &mcp.ServerOptions{}
	}
	opts.Instructions = "MeshConsole 只读查询：节点、指标、受管服务、AI agent 与 tailnet 状态。全部工具只读，无任何写操作。"
	opts.Logger = nil // 日志由调用方的 stderr slog 负责，SDK 侧不再叠加
	s := mcp.NewServer(&mcp.Implementation{Name: serverImplName, Version: version}, opts)
	q := meshview.New(st)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_nodes",
		Description: "列出全部 mesh 节点（名称/角色/系统/在线状态/agent 版本/最近心跳时间）",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listNodesOut, error) {
		nodes, err := q.Nodes(ctx)
		if err != nil {
			return nil, listNodesOut{}, err
		}
		if nodes == nil {
			nodes = []meshview.Node{}
		}
		return nil, listNodesOut{Nodes: nodes}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_node",
		Description: "查询单个节点：基础信息 + 最近 24h 指标摘要（cpu/mem 均值与峰值、磁盘占比）+ 最新一条指标 + 该节点的受管服务与 AI agent 清单",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getNodeIn) (*mcp.CallToolResult, getNodeOut, error) {
		if in.Name == "" {
			return nil, getNodeOut{}, fmt.Errorf("缺少 name 参数（节点名称）")
		}
		detail, err := q.NodeDetail(ctx, in.Name)
		if err != nil {
			return nil, getNodeOut{}, err
		}
		if detail == nil {
			return nil, getNodeOut{}, fmt.Errorf("节点 %q 不存在（可用 list_nodes 查看全部节点名）", in.Name)
		}
		return nil, getNodeOut{Node: detail}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_services",
		Description: "列出受管服务清单（状态 active/inactive/failed/unavailable/unknown/stale 及说明），可按节点名过滤",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listServicesIn) (*mcp.CallToolResult, listServicesOut, error) {
		svcs, err := q.Services(ctx, in.Node)
		if err != nil {
			return nil, listServicesOut{}, err
		}
		if svcs == nil {
			svcs = []meshview.Service{}
		}
		return nil, listServicesOut{Services: svcs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_agents",
		Description: "列出全网 AI agent 清单（类型/版本/健康状态；invokable 恒为 false——发现不等于可调用），可按节点名过滤",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listAgentsIn) (*mcp.CallToolResult, listAgentsOut, error) {
		ags, err := q.Agents(ctx, in.Node)
		if err != nil {
			return nil, listAgentsOut{}, err
		}
		if ags == nil {
			ags = []meshview.Agent{}
		}
		return nil, listAgentsOut{Agents: ags}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_agent_tasks",
		Description: "列出当前运行中的 agent 任务快照（节点/agent 名/命令/已运行时长/cpu/mem/启动时刻；进程消失即从快照移除，无历史），可按节点名过滤。" +
			"tasks_truncated=true 表示涉及节点的清单触顶 64 条截断、返回仅为前 64 条",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listAgentTasksIn) (*mcp.CallToolResult, listAgentTasksOut, error) {
		tasks, err := q.AgentTasks(ctx, in.Node)
		if err != nil {
			return nil, listAgentTasksOut{}, err
		}
		if tasks == nil {
			tasks = []meshview.AgentTask{}
		}
		// 截断标记透出（R29-#3）：nodes.tasks_truncated 已落库（R27-#4），
		// 视图按过滤口径取并集——未过滤=全网任一节点截断即 true，按节点过滤
		// =该节点自己的标记；无涉及节点（未知节点过滤）为 false。
		nodes, err := q.Nodes(ctx)
		if err != nil {
			return nil, listAgentTasksOut{}, err
		}
		out := listAgentTasksOut{Tasks: tasks}
		for _, n := range nodes {
			if in.Node != "" && n.Name != in.Node {
				continue
			}
			if n.TasksTruncated {
				out.TasksTruncated = true
				break
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_mesh_status",
		Description: "全网汇总：节点总数/在线数/离线名单、服务异常名单（非 active 且数据未过宽限）、headscale tailnet 概况、数据新鲜度（库最新指标时间）",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, getMeshStatusOut, error) {
		status, err := q.Status(ctx)
		if err != nil {
			return nil, getMeshStatusOut{}, err
		}
		return nil, getMeshStatusOut{Status: status}, nil
	})

	return s
}
