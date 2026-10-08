// commands.go 为 MCP 的命令查询与下发工具（SPEC-M1d §4）：
//
//	readonly：既有只读工具 + get_command / list_commands（只读查询）
//	operator：+ submit_command（下发白名单命令）
//
// 授权口径：所有入口统一服务端检查——stdio MCP 无凭据恒为 readonly（submit_command
// 调用即拒绝）；HTTP MCP 按 api_tokens.scope 判定（token 上下文由 HTTP 中间件
// 注入，见 http.go）。description 一律中文（SPEC §4）。
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mtl802/meshconsole/internal/cmdkind"
	"github.com/mtl802/meshconsole/internal/cmdsvc"
	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

// ctxKeyToken 为 HTTP 中间件放入请求上下文的 API token 认证结果键。
type ctxKeyToken struct{}

// WithTokenAuth 注入认证结果（http.go 中间件调用）。
func WithTokenAuth(ctx context.Context, t *store.APITokenAuth) context.Context {
	return context.WithValue(ctx, ctxKeyToken{}, t)
}

// scopeOf 取调用方 scope：HTTP 路径 = token scope；stdio 无凭据恒 readonly
// （SPEC-M1d §1：stdio MCP = readonly）。
func scopeOf(ctx context.Context) string {
	if t, ok := ctx.Value(ctxKeyToken{}).(*store.APITokenAuth); ok && t != nil {
		return t.Scope
	}
	return store.ScopeReadonly
}

// ErrScopeDenied 为 scope 不足的统一错误（中文文案；HTTP 层另有 403 前置拦截，
// 此处兜底覆盖 stdio 与 SDK 内层路径）。
var ErrScopeDenied = errors.New("权限不足：submit_command 需要 operator scope 的 API token（stdio MCP 为只读，请经 HTTP MCP 调用）")

// ---- 工具入参/出参 ----

type submitCommandIn struct {
	Node string `json:"node" jsonschema:"目标节点名（须在线且在 l2_allowed 白名单，云节点禁止）"`
	// Kind 为白名单命令类型（systemctl_status/journalctl_tail/ps_snapshot/
	// df_report/launchctl_list/mac_log_show/docker_ps）。
	Kind string `json:"kind" jsonschema:"命令类型（白名单 kind，见工具描述）"`
	// Args 为参数槽对象（随 kind 而定；无参 kind 传空对象或省略）。
	Args map[string]any `json:"args,omitempty" jsonschema:"参数槽对象（如 {\"unit\":\"headscale\",\"n\":100}；键集合须与 kind 定义精确一致）"`
	// TimeoutS 缺省 30，上限 300。
	TimeoutS int64 `json:"timeout_s,omitempty" jsonschema:"执行超时秒数（缺省 30，上限 300）"`
	// SubmissionKey 客户端幂等键（可空）：同键同参 10 分钟窗口内重复提交返回
	// 原命令；同键异参 409；窗口过期键归档后可复用。
	SubmissionKey string `json:"submission_key,omitempty" jsonschema:"可选，客户端生成的提交幂等键（重试防重）"`
}

type submitCommandOut struct {
	CommandID  string           `json:"command_id"`
	Status     string           `json:"status"`
	Node       string           `json:"node"`
	Kind       string           `json:"kind"`
	Args       map[string]any   `json:"args"`
	TimeoutS   int64            `json:"timeout_s"`
	Idempotent bool             `json:"idempotent"`
	Detail     *commandViewFull `json:"detail,omitempty"`
}

type getCommandIn struct {
	CommandID string `json:"command_id" jsonschema:"命令 id（提交时返回的 command_id）"`
}

type getCommandOut struct {
	Command *commandViewFull `json:"command"`
}

type listCommandsIn struct {
	Node   string `json:"node,omitempty" jsonschema:"可选，按节点名过滤"`
	Status string `json:"status,omitempty" jsonschema:"可选，按状态过滤（pending/claimed/running/succeeded/failed/unknown）"`
	Limit  int    `json:"limit,omitempty" jsonschema:"可选，返回条数上限（缺省 20，最大 100）"`
}

type listCommandsOut struct {
	Commands []commandViewList `json:"commands"`
}

// argsObj 把 args_json 解码为对象（MCP 输出结构用——SDK 按输出结构生成
// outputSchema 并校验，map 形态才与对象 schema 相符）。
func argsObj(argsJSON string) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal([]byte(argsJSON), &out)
	return out
}

// commandViewFull 为单条命令完整视图（get_command/submit 返回；result_text 全文）。
type commandViewFull struct {
	CommandID  string         `json:"command_id"`
	Node       string         `json:"node"`
	Kind       string         `json:"kind"`
	Args       map[string]any `json:"args"`
	Status     string         `json:"status"`
	Degraded   bool           `json:"degraded"`
	TimeoutS   int64          `json:"timeout_s"`
	ResultText string         `json:"result_text,omitempty"`
	ExitCode   *int64         `json:"exit_code,omitempty"`
	CreatedBy  string         `json:"created_by,omitempty"`
	Scope      string         `json:"scope,omitempty"`
	ClaimedAt  *int64         `json:"claimed_at,omitempty"`
	FinishedAt *int64         `json:"finished_at,omitempty"`
	Archived   bool           `json:"archived"`
	CreatedAt  int64          `json:"created_at"`
	UpdatedAt  int64          `json:"updated_at"`
}

// commandViewList 为命令清单条目（result_text 只带 256 字节预览，防长输出撑爆列表）。
type commandViewList struct {
	CommandID     string         `json:"command_id"`
	Node          string         `json:"node"`
	Kind          string         `json:"kind"`
	Args          map[string]any `json:"args"`
	Status        string         `json:"status"`
	Degraded      bool           `json:"degraded"`
	ExitCode      *int64         `json:"exit_code,omitempty"`
	ResultPreview string         `json:"result_preview,omitempty"`
	CreatedBy     string         `json:"created_by,omitempty"`
	CreatedAt     int64          `json:"created_at"`
	FinishedAt    *int64         `json:"finished_at,omitempty"`
}

// kindNamesDoc 为 kind 白名单的文档串（工具 description 用）。
var kindNamesDoc = strings.Join(cmdkind.Names(), "/")

// fullView 把 store 记录转为完整视图。
func fullView(r *store.CommandRecord) *commandViewFull {
	v := &commandViewFull{
		CommandID: r.CommandID, Node: r.NodeName, Kind: r.Kind,
		Args:   argsObj(r.ArgsJSON),
		Status: r.Status, Degraded: r.Degraded, TimeoutS: r.TimeoutS,
		ResultText: r.ResultText, CreatedBy: r.CreatedBy, Scope: r.Scope,
		Archived: r.Archived, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.ExitCode.Valid {
		c := r.ExitCode.Int64
		v.ExitCode = &c
	}
	if r.ClaimedAt.Valid {
		t := r.ClaimedAt.Int64
		v.ClaimedAt = &t
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Int64
		v.FinishedAt = &t
	}
	return v
}

func listView(r *store.CommandRecord) commandViewList {
	v := commandViewList{
		CommandID: r.CommandID, Node: r.NodeName, Kind: r.Kind,
		Args: argsObj(r.ArgsJSON), Status: r.Status, Degraded: r.Degraded,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
	}
	if r.ExitCode.Valid {
		c := r.ExitCode.Int64
		v.ExitCode = &c
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Int64
		v.FinishedAt = &t
	}
	v.ResultPreview = r.ResultText
	if len(v.ResultPreview) > 256 {
		v.ResultPreview = v.ResultPreview[:256] + "…（截断，get_command 取全文）"
	}
	return v
}

// addCommandTools 注册命令三工具（submit_command 带 scope 校验）。
func addCommandTools(s *mcp.Server, st *store.Store, cfg *config.Console) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "submit_command",
		Description: "向指定节点下发白名单内的只读命令（kind 结构化参数，禁止自由 shell）。" +
			"目标须在线、在 l2 下发白名单（mac-mini/windows，云节点禁止）且支持该 kind 平台。" +
			"命令经 agent 领取执行，结果用 get_command 轮询。需要 operator scope。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in submitCommandIn) (*mcp.CallToolResult, submitCommandOut, error) {
		if scopeOf(ctx) != store.ScopeOperator {
			return nil, submitCommandOut{}, ErrScopeDenied
		}
		argsJSON := "{}"
		if len(in.Args) > 0 {
			b, err := json.Marshal(in.Args)
			if err != nil {
				return nil, submitCommandOut{}, fmt.Errorf("args 序列化失败")
			}
			argsJSON = string(b)
		}
		// 调用方身份归因：token 所属用户名（scope 同步记录；审计不可改写）。
		createdBy, scope := "", store.ScopeReadonly
		if t, ok := ctx.Value(ctxKeyToken{}).(*store.APITokenAuth); ok && t != nil {
			createdBy, scope = t.Username, t.Scope
		}
		res, err := cmdsvc.Submit(ctx, st, cfg, &cmdsvc.SubmitRequest{
			NodeName: in.Node, Kind: in.Kind, ArgsJSON: argsJSON,
			TimeoutS: in.TimeoutS, SubmissionKey: in.SubmissionKey,
			CreatedBy: createdBy, Scope: scope,
		})
		if err != nil {
			return nil, submitCommandOut{}, err
		}
		out := submitCommandOut{
			CommandID: res.Command.CommandID, Status: res.Command.Status,
			Node: res.Command.NodeName, Kind: res.Command.Kind,
			Args: argsObj(res.Command.ArgsJSON), TimeoutS: res.Command.TimeoutS,
			Idempotent: res.Idempotent, Detail: fullView(res.Command),
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_command",
		Description: "查询单条下发命令：状态（pending/claimed/running/succeeded/failed/unknown）、退出码与完整结果文本。只读工具。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getCommandIn) (*mcp.CallToolResult, getCommandOut, error) {
		if strings.TrimSpace(in.CommandID) == "" {
			return nil, getCommandOut{}, fmt.Errorf("缺少 command_id 参数")
		}
		rec, err := st.GetCommand(ctx, in.CommandID)
		if err != nil {
			return nil, getCommandOut{}, err
		}
		if rec == nil {
			return nil, getCommandOut{}, fmt.Errorf("命令 %q 不存在", in.CommandID)
		}
		return nil, getCommandOut{Command: fullView(rec)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_commands",
		Description: "列出最近下发的命令（按创建时间倒序；result_preview 为结果前 256 字节，全文用 get_command）。可按节点/状态过滤。只读工具。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listCommandsIn) (*mcp.CallToolResult, listCommandsOut, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}
		status := in.Status
		switch status {
		case "", "pending", "claimed", "running", "succeeded", "failed", "unknown":
		default:
			return nil, listCommandsOut{}, fmt.Errorf("status 须为 pending/claimed/running/succeeded/failed/unknown")
		}
		rows, err := st.ListCommands(ctx, in.Node, status, limit)
		if err != nil {
			return nil, listCommandsOut{}, err
		}
		out := make([]commandViewList, 0, len(rows))
		for i := range rows {
			out = append(out, listView(&rows[i]))
		}
		return nil, listCommandsOut{Commands: out}, nil
	})
}
