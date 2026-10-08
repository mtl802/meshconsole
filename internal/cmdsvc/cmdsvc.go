// Package cmdsvc 为 L2 任务下发的提交编排服务（SPEC-M1d §1-§5）：MCP
// submit_command 与面板提交表单的统一入口——所有入口（panel/MCP HTTP/stdio）
// 的服务端授权与校验走同一条路径，规则只此一份。
//
// 提交侧安全闸（按序）：
//  1. 节点存在且在线（status=online）；
//  2. l2_allowed 白名单复核①（config 声明同步的 nodes 列；云节点一律拒绝）；
//  3. caps 平台矩阵校验（agent 心跳上报，不支持该 kind 的节点 400）；
//  4. kind 结构化白名单 + 类型化参数槽校验（cmdkind；unit 槽须命中该节点心跳
//     上报的受管服务集合 ∪ l2_extra_commands 扩展实例）；
//  5. timeout 值域（默认 30s，上限 300s）；
//  6. 幂等键（submission_key 可空：同键同参窗口内返回原命令，同键异参 409，
//     窗口过期归档旧键后新建）；
//  7. 配额（每节点在途 ≤5，事务内计数，超出 429）。
//
// agent 领取（registry.ClaimNodeCommands 事务内复核 l2_allowed）与 agent 执行
// （本地 l2_enabled 总开关 + cmdkind 再校验）构成双重复核的另两端。
package cmdsvc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/cmdkind"
	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

// 提交侧协议常量。
const (
	// DefaultTimeoutS 为缺省执行超时（SPEC §3：默认 30s）。
	DefaultTimeoutS = 30
	// MaxTimeoutS 为超时上限（SPEC §3：上限 300s）。
	MaxTimeoutS = 300
	// idempotencyWindow 为幂等键窗口（DESIGN §4.1-D：10 分钟内同键同参返回
	// 原命令、同键异参 409；过期归档旧键立即可复用）。
	idempotencyWindow = 10 * time.Minute
)

// 提交拒绝错误（错误文本即用户可见文案，中文化；调用方映射 HTTP 状态码）。
var (
	// ErrNodeNotFound 节点不存在。
	ErrNodeNotFound = errors.New("节点不存在")
	// ErrNodeOffline 节点不在线（下发要求目标当前在线）。
	ErrNodeOffline = errors.New("节点不在线，无法下发")
	// ErrNodeNotAllowed 节点不在 L2 下发白名单（云节点禁止，伦哥 gate）。
	ErrNodeNotAllowed = errors.New("该节点未开放任务下发（l2 白名单仅限 mac-mini/windows，云节点禁止）")
	// ErrCapsUnsupported 目标节点不具备该 kind 需要的平台能力。
	ErrCapsUnsupported = errors.New("目标节点不支持该命令类型（caps 协商不匹配）")
	// ErrBadKind 非白名单 kind。
	ErrBadKind = errors.New("命令类型不在白名单内")
	// ErrBadArgs 参数槽校验失败（字符集/值域/键集合/受管集合）。
	ErrBadArgs = errors.New("命令参数校验失败")
	// ErrBadTimeout 超时值域非法。
	ErrBadTimeout = errors.New("timeout_s 须在 1-300（缺省 30）")
	// ErrBadSubmissionKey 幂等键非法（超长/控制字符）。
	ErrBadSubmissionKey = errors.New("submission_key 须为非空且 ≤128 字节、无控制字符")
	// ErrTimeout 等待幂等键归档锁超时（内部，映射 500）。
	ErrTimeout = errors.New("提交处理超时")
)

// SubmitRequest 为一次命令提交（入口无关）。
type SubmitRequest struct {
	// NodeName 为目标节点名。
	NodeName string
	// Kind 为白名单命令类型。
	Kind string
	// ArgsJSON 为参数槽 JSON（可为空串/{}——无参 kind）。
	ArgsJSON string
	// TimeoutS 为执行超时秒数；0 取默认 30，>300 拒绝。
	TimeoutS int64
	// SubmissionKey 为客户端生成的幂等键（可空=不幂等，每次新命令）。
	SubmissionKey string
	// CreatedBy 为提交者身份（用户名；审计归因）。
	CreatedBy string
	// Scope 为提交凭据的 scope（operator/readonly/panel；审计归因）。
	Scope string
}

// SubmitResult 为提交结果。
type SubmitResult struct {
	Command *store.CommandRecord
	// Idempotent=true 表示幂等命中返回了原命令（未新建）。
	Idempotent bool
}

// UUID 生成命令主键（uuid v4 文本；command_id 永不复用——全局唯一随机源）。
func UUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// Submit 执行一次命令提交（见包注释的闸门顺序）。cfg 提供 l2_extra_commands
// 扩展实例与（经由节点服务清单的）受管集合之外的第二判定来源。
func Submit(ctx context.Context, st *store.Store, cfg *config.Console, req *SubmitRequest) (*SubmitResult, error) {
	req.NodeName = strings.TrimSpace(req.NodeName)
	req.Kind = strings.TrimSpace(req.Kind)
	if req.Kind == "" || cmdkind.Lookup(req.Kind) == nil {
		return nil, ErrBadKind
	}
	// 幂等键先做形态校验（防把超长垃圾送进唯一索引查询）。
	if req.SubmissionKey != "" {
		if len(req.SubmissionKey) > store.SubmissionKeyMaxLen {
			return nil, ErrBadSubmissionKey
		}
		for _, r := range req.SubmissionKey {
			if r < 0x20 || r == 0x7f {
				return nil, ErrBadSubmissionKey
			}
		}
	}
	timeout := req.TimeoutS
	if timeout == 0 {
		timeout = DefaultTimeoutS
	}
	if timeout < 1 || timeout > MaxTimeoutS {
		return nil, ErrBadTimeout
	}

	// ①②③ 节点状态、l2 白名单、caps（读路径）。
	node, err := st.GetNodeByName(ctx, req.NodeName)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, ErrNodeNotFound
	}
	if node.Status != "online" {
		return nil, ErrNodeOffline
	}
	if !node.L2Allowed {
		return nil, ErrNodeNotAllowed
	}
	caps, err := parseCaps(node.Caps)
	if err != nil {
		return nil, err
	}
	if !cmdkind.CapsOK(req.Kind, caps) {
		return nil, ErrCapsUnsupported
	}

	// ④ 参数槽双重校验的 console 端：先按 kind 规则（字符集/值域/键集合）做
	// 无集合校验取规范化产物，再判集合——unit 槽须命中「心跳上报的受管服务
	// 集合」或「l2_extra_commands 扩展实例」（扩展按 (kind, 规范化 args) 精确
	// 比对，扩展的就是这个参数实例本身，不受集合限制；新可执行路径仍不可能
	// 借道引入——kind 决定二进制）。
	argsJSON, err := cmdkind.ValidateArgs(req.Kind, req.ArgsJSON, func(string) bool { return true })
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArgs, err)
	}
	managed, err := st.NodeManagedUnits(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	if !managedUnitsSatisfied(req.Kind, argsJSON, managed) && !cfg.L2ExtraMatches(req.Kind, argsJSON) {
		return nil, fmt.Errorf("%w: unit 不在受管服务集合内（可用 l2_extra_commands 声明参数化扩展实例）", ErrBadArgs)
	}

	// ⑥⑦ 幂等键 + 配额（写事务）。幂等命中统一过窗口检查（R39-#6）：同键
	// 同参与同键异参同口径——窗口内命中/409，窗口过期归档旧键后重试一次
	// （DESIGN §4.1-D 归档模式；读路径定窗口、写路径归档，职责分离）。
	now := time.Now().Unix()
	for attempt := 0; attempt < 2; attempt++ {
		row, idem, err := st.SubmitCommand(ctx, &store.CommandRow{
			CommandID:    mustUUID(),
			NodeID:       node.ID,
			Kind:         req.Kind,
			ArgsJSON:     argsJSON,
			Status:       store.CommandPending,
			TimeoutS:     timeout,
			CreatedBy:    req.CreatedBy,
			Scope:        req.Scope,
			SubmittedKey: sql.NullString{String: req.SubmissionKey, Valid: req.SubmissionKey != ""},
			CreatedAt:    now,
			UpdatedAt:    now,
		})
		switch {
		case err == nil:
			// 同键同参命中：窗口内返回原命令；超窗（行创建时刻早于窗口）归档
			// 旧键归档后键可复用——否则同键同参将永久返回旧命令，不符声明的
			// 窗口复用语义。归档后重试一次：键已释放，走新建。
			if idem && req.SubmissionKey != "" &&
				now-row.CreatedAt >= int64(idempotencyWindow/time.Second) {
				if _, aerr := st.ArchiveSubmissionKey(ctx, req.SubmissionKey, now); aerr != nil {
					return nil, aerr
				}
				continue
			}
			return &SubmitResult{Command: row, Idempotent: idem}, nil
		case errors.Is(err, store.ErrKeyConflict):
			if attempt == 0 {
				// 同键异参：幂等窗口内一律 409（DESIGN §4.1-D）；窗口过期才
				// 归档旧键（archived_key 只归档提交键，command_id 不动）并重试。
				createdAt, ok, errKey := st.GetSubmissionKeyCreatedAt(ctx, req.SubmissionKey)
				if errKey != nil {
					return nil, errKey
				}
				if ok && time.Now().Unix()-createdAt < int64(idempotencyWindow/time.Second) {
					return nil, store.ErrKeyConflict
				}
				if _, errKey := st.ArchiveSubmissionKey(ctx, req.SubmissionKey, time.Now().Unix()); errKey != nil {
					return nil, errKey
				}
				continue
			}
			return nil, store.ErrKeyConflict
		default:
			return nil, err
		}
	}
	return nil, ErrTimeout
}

// parseCaps 解析 nodes.caps JSON 数组（空串 = 无 caps）。
func parseCaps(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var caps []string
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return nil, fmt.Errorf("节点 caps 数据损坏: %w", err)
	}
	return caps, nil
}

// managedUnitsSatisfied 报告规范化 args 的全部 Managed 槽（unit）是否都命中
// 受管集合（kind 无 Managed 槽时恒真）。
func managedUnitsSatisfied(kind, argsJSON string, managed map[string]bool) bool {
	k := cmdkind.Lookup(kind)
	if k == nil {
		return false
	}
	var args map[string]string
	if len(k.Slots) == 0 {
		return true
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}
	for _, s := range k.Slots {
		if s.Kind == cmdkind.SlotUnit && !managed[args[s.Name]] {
			return false
		}
	}
	return true
}

func mustUUID() string {
	id, err := UUID()
	if err != nil {
		// crypto/rand 失败属系统级故障：拒绝提交优于空主键。
		panic("cmdsvc: generate command_id: " + err.Error())
	}
	return id
}
