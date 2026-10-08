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

	"github.com/mtl802/meshconsole/internal/config"
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
	listActiveSessionsOut struct {
		// Sessions 为活跃会话清单（SPEC-M1d §3.5：agent 会话目录 mtime<30min
		// 的会话文件，轻解析主题与当前动作）。
		Sessions []meshview.AgentSession `json:"sessions"`
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
	listActiveSessionsIn struct {
		Node string `json:"node,omitempty" jsonschema:"可选，按节点名过滤"`
	}
)

// New 构造注册好只读工具与命令工具的 MCP server。cfg 提供 l2_extra_commands
// 等提交侧配置（命令工具的统一服务端授权在 addCommandTools/HTTP 中间件）。
// 注意：本函数注册的工具集含 submit_command，其调用受 scope 校验——stdio 场景
// 无凭据恒 readonly（SPEC-M1d §1），调用即被拒绝。
func New(st *store.Store, cfg *config.Console, version string, opts *mcp.ServerOptions) *mcp.Server {
	if opts == nil {
		opts = &mcp.ServerOptions{}
	}
	opts.Instructions = "MeshConsole 查询工具：节点、指标、受管服务、AI agent 与 tailnet 状态（只读）；" +
		"命令查询 get_command/list_commands（只读）；submit_command 下发白名单命令（需 operator scope，stdio 为只读不可用）。"
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
		// 任务清单与截断标记在同一只读事务内取得（R31-#2 → R33-#7）：并发心跳
		// 下不再出现旧任务快照配新截断标记的错配。并集口径：未过滤=全网任一
		// 节点截断即 true，按节点过滤=该节点自己的标记；无涉及节点=false。
		tasks, truncated, err := q.AgentTasksSnapshot(ctx, in.Node)
		if err != nil {
			return nil, listAgentTasksOut{}, err
		}
		return nil, listAgentTasksOut{Tasks: tasks, TasksTruncated: truncated}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_active_sessions",
		Description: "列出当前活跃的 agent 会话（SPEC-M1d §3.5：各 agent 会话目录中最近 30 分钟有活动的会话文件），含会话文件路径、开始时刻与最近活动时刻，可按节点名过滤。已知限制：会话主题（topic）与当前动作（recent_action）暂不可用（恒为空串，各 CLI 会话文件为嵌套 JSONL、内容解析留待后续版本按真实样本实现）——判断某终端上的 agent「正在干嘛」目前以会话文件活跃度为依据",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listActiveSessionsIn) (*mcp.CallToolResult, listActiveSessionsOut, error) {
		sessions, err := q.AgentSessions(ctx, in.Node)
		if err != nil {
			return nil, listActiveSessionsOut{}, err
		}
		return nil, listActiveSessionsOut{Sessions: sessions}, nil
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

	// M1d：命令查询与下发三工具（scope 校验在工具内 + HTTP 中间件 403 前置）。
	addCommandTools(s, st, cfg)

	return s
}
