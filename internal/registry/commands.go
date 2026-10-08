// commands.go 为 /api/agent/heartbeat 的任务下发协议处理（SPEC-M1d §2/§5）：
//
// 请求扩展：caps（能力协商）、command_results（回执）、command_running（执行中
// 报告）、command_unacked（agent 仍持有的未确认结果清单）。
// 响应扩展：commands（领取的待执行命令，≤3/次——响应写入前已原子置 claimed +
// claimed_at）、ack_ids（已落库回执的确认，agent 收到才删本地持久化）、
// drop_ids（无归属/畸形回执的丢弃指示，防 agent 无限重发）。
//
// 安全口径：回执归属验证（本节点 + claimed/running/unknown 修正路径 + claimed_at
// 匹配）在 store.RecordCommandResults 事务内逐条判定；领取的 l2_allowed 双重
// 复核在 ClaimNodeCommands 事务内重读库列——云节点在提交与领取两端都被拦。
package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/mtl802/meshconsole/internal/store"
)

// 协议数组上限（SPEC-M1d §2：commands ≤3/次；回执/运行报告/未确认清单按在途
// 配额与单拍执行能力约束）。
const (
	maxCommandResults = 32
	maxCommandIDs     = 64
	maxCaps           = 16
	maxCapsEntryLen   = 64
)

// commandOut 为心跳响应下发的单条命令（agent 依此执行）。
type commandOut struct {
	CommandID string `json:"command_id"`
	Kind      string `json:"kind"`
	ArgsJSON  string `json:"args_json"`
	TimeoutS  int64  `json:"timeout_s"`
	// ClaimedAt 为本次领取的租约起点（unix 秒；租约过期判定与审计留痕用）。
	ClaimedAt int64 `json:"claimed_at"`
	// LeaseEpoch 为本次领取的租约代次（R41-B1：首次领取=1，过期重领自增）——
	// 回执必须回显该值供归属验证（唯一权威令牌，代次严格单调）。
	LeaseEpoch int64 `json:"lease_epoch"`
}

// commandResultIn 为 agent 上报的单条回执。
type commandResultIn struct {
	CommandID string `json:"command_id"`
	// ClaimedAt 为 agent 收到命令时的租约起点回显（审计留痕；归属验证以
	// LeaseEpoch 为准）。
	ClaimedAt int64 `json:"claimed_at"`
	// LeaseEpoch 为 agent 收到命令时的租约代次回显（归属验证核心字段，
	// R41-B1）。
	LeaseEpoch int64 `json:"lease_epoch"`
	// ExitCode 可空：超时/信号终止无退出码（null）。
	ExitCode *int64 `json:"exit_code"`
	// ResultText 为 stdout+stderr 合并采集（agent 侧 64KB 硬上限）。
	ResultText string `json:"result_text"`
	// Interrupted=true 表示 agent 重启发现 intent 已落盘但无结果（不重执行），
	// console 置 unknown 对齐（DESIGN §4.1-D 崩溃恢复对账）。
	Interrupted bool `json:"interrupted"`
}

// ackEntry / dropEntry 为心跳响应 ack_ids / drop_ids 的条目形态（R41-B1：携带
// 租约代次——agent 据此只清理对应代次的本地对账副本，不触达更高代次的在途
// 执行与结果）。drop 条目不携带原因（原因走 console 审计日志，不进协议）。
type ackEntry struct {
	CommandID  string `json:"command_id"`
	LeaseEpoch int64  `json:"lease_epoch"`
}

type dropEntry struct {
	CommandID  string `json:"command_id"`
	LeaseEpoch int64  `json:"lease_epoch"`
}

// validCommandID 校验命令 id 形态（console 签发的 uuid v4 文本；防注入垃圾进
// 查询主键）。
func validCommandID(id string) bool {
	if id == "" || len(id) > store.CommandIDMaxLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r == '-':
		default:
			return false
		}
	}
	return true
}

// validateCaps 净化能力清单：条目数受限、字符集白名单（[a-zA-Z0-9._*-]，如
// "linux-systemd"/"windows-*"）、无控制字符；违规整条 400（协议违规与 metrics
// 值域同口径——静默截断会让 caps 声明失真）。
func validateCaps(in []string) ([]string, error) {
	if len(in) > maxCaps {
		return nil, errors.New("too many caps")
	}
	out := make([]string, 0, len(in))
	for _, c := range in {
		if !validCapEntry(c) {
			return nil, errors.New("caps entry malformed")
		}
		out = append(out, c)
	}
	return out, nil
}

// validCapEntry 校验单条 caps 形态：非空、≤64 字节、字符集 [a-zA-Z0-9._*-]。
func validCapEntry(s string) bool {
	if s == "" || len(s) > maxCapsEntryLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '*':
		default:
			return false
		}
	}
	return true
}

// parseCommandIDs 解析并校验 id 数组（command_running / command_unacked）。
func parseCommandIDs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	_, ids, err := optionalArray[string](raw)
	if err != nil {
		return nil, err
	}
	if len(ids) > maxCommandIDs {
		return nil, errors.New("too many command ids")
	}
	for _, id := range ids {
		if !validCommandID(id) {
			return nil, errors.New("bad command id")
		}
	}
	return ids, nil
}

// parseCommandResults 解析并校验回执数组（三态同 services 口径：缺席不处理、
// 显式 null 400、显式数组逐条校验）。
func parseCommandResults(raw json.RawMessage) ([]commandResultIn, error) {
	present, in, err := optionalArray[commandResultIn](raw)
	if err != nil || !present {
		return nil, err
	}
	if len(in) > maxCommandResults {
		return nil, errors.New("too many command_results")
	}
	for _, r := range in {
		if !validCommandID(r.CommandID) {
			return nil, errors.New("bad command id")
		}
		if r.ClaimedAt < 0 || r.ClaimedAt > time.Now().Unix()+86400 {
			return nil, errors.New("bad claimed_at")
		}
		if r.LeaseEpoch < 0 || r.LeaseEpoch > 1<<20 {
			return nil, errors.New("bad lease_epoch")
		}
		if r.ExitCode != nil && (*r.ExitCode < -128 || *r.ExitCode > 255) {
			return nil, errors.New("bad exit_code")
		}
		// result_text 64KB 上限在 RecordCommandResults 事务内判定（超限 drop
		// 而非整条 400——单条畸形不应拖垮同批其余回执的对账）。
	}
	return in, nil
}

// handleHeartbeatCommands 在心跳主流程成功后处理命令协议（回执 → 运行报告 →
// 领取 → ACK 名单），返回响应扩展字段。任何内部错误不使心跳失败（命令协议
// 独立于指标通道：记账失败记日志、下拍重试，回执未 ACK 会被 agent 重发）。
func (h *Handler) handleHeartbeatCommands(ctx context.Context, node *store.Node,
	results []commandResultIn, running []string, unacked []string, now int64,
) (cmds []commandOut, ackIDs []ackEntry, dropIDs []dropEntry) {
	// ACK 条目按命令合并（回执路径与 unacked 名单同拍可能命中同一条命令——
	// 取最大代次；重复条目对 agent 无害但徒增噪音）。出口统一转有序切片。
	acks := map[string]int64{}
	// 回执入库（归属验证在事务内逐条判定；旧代次迟到回执忽略计数——R41-B1
	// degraded 审计，stale_receipts_total 随 AUDIT 日志可辨）。
	if len(results) > 0 {
		in := make([]store.ResultIn, 0, len(results))
		for _, r := range results {
			in = append(in, store.ResultIn{
				CommandID:   r.CommandID,
				ClaimedAt:   r.ClaimedAt,
				LeaseEpoch:  r.LeaseEpoch,
				ExitCode:    nullInt64(r.ExitCode),
				ResultText:  r.ResultText,
				Interrupted: r.Interrupted,
				RecordedAt:  now,
			})
		}
		outcomes, err := h.st.RecordCommandResults(ctx, node.ID, in)
		if err != nil {
			h.log.Error("record command results", "node", node.Name, "err", err)
		} else {
			for _, oc := range outcomes {
				if oc.Drop {
					// 旧代次迟到回执显式计数（R41-B1：忽略并计数，degraded 审计
					// ——回执内容不触达命令状态，累计值随 AUDIT 日志可辨）。
					total := int64(0)
					if oc.DropWhy == "stale epoch" {
						total = h.staleReceipts.Add(1)
					}
					// drop 条目携带被拒回执自陈的代次：agent 据此定点删该代次的
					// 本地副本、停止无效重发（不带代次则 agent 找不到文件，回执
					// 会被无限重发）。
					dropIDs = append(dropIDs, dropEntry{CommandID: oc.CommandID, LeaseEpoch: oc.LeaseEpoch})
					logArgs := []any{
						"audit", "command", "command_id", oc.CommandID, "node", node.Name, "reason", oc.DropWhy,
					}
					if total > 0 {
						logArgs = append(logArgs, "stale_receipts_total", total)
					}
					h.log.Warn("AUDIT command result dropped", logArgs...)
					continue
				}
				// 本拍已落库的回执立即 ACK（agent 收到才删本地持久化结果）。
				if oc.Ack {
					if prev, ok := acks[oc.CommandID]; !ok || oc.LeaseEpoch > prev {
						acks[oc.CommandID] = oc.LeaseEpoch
					}
				}
				if oc.Status == store.CommandSucceeded || oc.Status == store.CommandFailed {
					h.log.Info("AUDIT command finished",
						"audit", "command", "command_id", oc.CommandID, "node", node.Name,
						"status", oc.Status)
				}
			}
		}
	}
	// 执行中报告：claimed → running（SPEC §1 状态机；为 timeout×1.5 的 unknown
	// 判定提供起点）。
	if len(running) > 0 {
		if _, err := h.st.MarkCommandsRunning(ctx, node.ID, running, now); err != nil {
			h.log.Error("mark commands running", "node", node.Name, "err", err)
		}
	}
	// ACK 名单：agent 仍持有且 console 已落库的回执（条目携带已记录回执的租约
	// 代次，agent 收到才删对应代次及更早的本地副本）。
	if len(unacked) > 0 {
		entries, err := h.st.AckableCommandIDs(ctx, node.ID, unacked)
		if err != nil {
			h.log.Error("list ackable commands", "node", node.Name, "err", err)
		} else {
			for _, e := range entries {
				if prev, ok := acks[e.CommandID]; !ok || e.LeaseEpoch > prev {
					acks[e.CommandID] = e.LeaseEpoch
				}
			}
		}
	}
	for id, epoch := range acks {
		ackIDs = append(ackIDs, ackEntry{CommandID: id, LeaseEpoch: epoch})
	}
	sort.Slice(ackIDs, func(i, j int) bool { return ackIDs[i].CommandID < ackIDs[j].CommandID })
	// 领取（l2_allowed 双重复核②：事务内重读库列；云节点不返回任何命令）。
	claimed, err := h.st.ClaimNodeCommands(ctx, node.ID, now, store.CommandLeaseS, store.CommandClaimLimit)
	if err != nil {
		h.log.Error("claim node commands", "node", node.Name, "err", err)
		return cmds, ackIDs, dropIDs
	}
	for i := range claimed {
		c := &claimed[i]
		cmds = append(cmds, commandOut{
			CommandID: c.CommandID, Kind: c.Kind, ArgsJSON: c.ArgsJSON,
			TimeoutS: c.TimeoutS, ClaimedAt: c.ClaimedAt.Int64, LeaseEpoch: c.LeaseEpoch,
		})
		h.log.Info("AUDIT command claimed",
			"audit", "command", "command_id", c.CommandID, "node", node.Name,
			"kind", c.Kind, "claimed_at", c.ClaimedAt.Int64, "lease_epoch", c.LeaseEpoch)
	}
	return cmds, ackIDs, dropIDs
}

// nullInt64 把可空指针转 sql.NullInt64。
func nullInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}
