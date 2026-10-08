// command.go 为 commands 表的存储层（SPEC-M1d §1/§2，migration v8；v10 加
// lease_epoch 租约代次令牌）。
//
// 状态机口径（SPEC §1/§2，协议基线 DESIGN §4.1-D）：
//
//		pending → claimed（含 lease，claimed_at 为租约起点）→ running
//		        → succeeded | failed；unknown 为「无回执」降级态（degraded 标记）
//
//	  - 领取原子性由 ClaimNodeCommands 保证：单条 UPDATE…WHERE 子查询在写锁内
//	    完成「筛选可领 → 置 claimed + claimed_at → RETURNING」，响应写入前的
//	    提交即领取（双实例并发心跳不可能同时领到同一行）。
//	  - lease 过期（claimed/running 且超租约）重新可领 = 重发至终态；unknown
//	    不可领取（永不自动重发，DESIGN §4.1-D；迟到回执走修正路径）。
//	  - 租约代次（R41-B1 深修）：lease_epoch 首次领取置 1、过期重领自增（同
//	    command_id 代次前移）。回执按 epoch 归属验证——epoch 是唯一权威令牌
//	    （代次严格单调，claimed_at 同秒重领无法区分的缺口由 epoch 封死）。
//	  - unknown 触发（SweepUnknown）：lease×2 无回执，或 running 后 timeout×1.5
//	    无回执 → unknown + degraded；当前代次迟到回执修正为实际终态并保留
//	    degraded 历史，不误判 failed。
//	  - 回执归属验证：只接受本节点、当前 claimed/running（或 unknown 修正）且
//	    lease_epoch 匹配的回执；旧代次迟到回执拒绝（ResultOutcome 拒因回传
//	    drop，console 侧计数审计），agent 收 drop 后删该代次本地副本。
//	  - archived 为生命周期属性列而非独立状态：离线节点 pending 24h 过期未领 →
//	    archived（ArchiveStalePending），行保留供审计直至 90 天清理。
//	  - 提交幂等：submission_key 唯一索引；窗口内同键同参 → 原命令（幂等命中），
//	    同键异参 → 冲突；窗口过期 → 旧行 submission_key 移入 archived_key 并置
//	    NULL（不归档 command_id），键立即可复用（ArchiveSubmissionKey）。
//	  - 审计不可改写：本层无任何「改写历史」路径——除回执落结果（终态/unknown
//	    修正）与幂等键归档外，行内容一经落库不更新。
package store

import (
	"context"
	"database/sql"
	"errors"
)

// 命令状态与协议常量（SPEC-M1d §1/§2）。
const (
	// CommandPending 等待领取。
	CommandPending = "pending"
	// CommandClaimed 已下发待执行（租约起点 claimed_at）。
	CommandClaimed = "claimed"
	// CommandRunning agent 已报告执行中（running_at 起点）。
	CommandRunning = "running"
	// CommandSucceeded 执行成功终态。
	CommandSucceeded = "succeeded"
	// CommandFailed 执行失败终态。
	CommandFailed = "failed"
	// CommandUnknown 无回执降级态（degraded=1；迟到回执可修正）。
	CommandUnknown = "unknown"

	// CommandQuotaPerNode 为每节点在途命令配额（pending+claimed+running，
	// SPEC §2：超出 429）。
	CommandQuotaPerNode = 5
	// CommandClaimLimit 为单次心跳最多下发的命令数（SPEC §2：≤3/次）。
	CommandClaimLimit = 3
	// CommandLeaseS 为租约时长（秒）。SweepUnknown 以 lease×2 判 unknown；
	// 取值须数倍于心跳间隔（15s），120s = 8 拍，覆盖本地执行队列排队。
	CommandLeaseS = 120
	// CommandResultsMaxBytes 为回执 result_text 硬上限（64KB，agent 侧采集
	// 同上限；console 超限拒绝该条回执）。
	CommandResultsMaxBytes = 64 << 10
	// CommandIDMaxLen 为 command_id 长度上限（uuid 36 字符；校验入站协议用）。
	CommandIDMaxLen = 64
	// SubmissionKeyMaxLen 为 submission_key 长度上限（客户端生成的幂等键）。
	SubmissionKeyMaxLen = 128
)

// 命令层错误（调用方映射 HTTP 语义：配额 429、冲突 409、未知键/归属拒绝 404/400）。
var (
	// ErrQuotaExceeded 每节点在途命令超出配额。
	ErrQuotaExceeded = errors.New("command quota exceeded for node")
	// ErrKeyConflict 同一 submission_key 在幂等窗口内携带了不同参数。
	ErrKeyConflict = errors.New("submission_key already used with different parameters")
	// ErrCommandNotFound 命令不存在（或无归属）。
	ErrCommandNotFound = errors.New("command not found")
	// ErrResultRejected 回执归属验证失败（非本节点/非可回执状态/claimed_at 不匹配/
	// 超限畸形），该条回执被拒。
	ErrResultRejected = errors.New("command result rejected")
)

// CommandRow 为 commands 表行（写入侧）。
type CommandRow struct {
	CommandID      string
	NodeID         int64
	Kind           string
	ArgsJSON       string
	Status         string
	Degraded       bool
	TimeoutS       int64
	ResultText     string
	ExitCode       sql.NullInt64
	CreatedBy      string
	Scope          string // created_by_token_scope
	ClaimedAt      sql.NullInt64
	LeaseEpoch     int64 // 租约代次（0=未领取过；首次领取置 1，过期重领自增）
	RunningAt      sql.NullInt64
	FinishedAt     sql.NullInt64
	SubmittedKey   sql.NullString // submission_key（归档后为 NULL）
	ArchivedKey    sql.NullString
	Archived       bool
	ResultRecorded bool
	CreatedAt      int64
	UpdatedAt      int64
}

// CommandRecord 为读出的命令行（查询侧；join 节点名便于展示）。
type CommandRecord struct {
	CommandID      string
	NodeID         int64
	NodeName       string
	Kind           string
	ArgsJSON       string
	Status         string
	Degraded       bool
	TimeoutS       int64
	ResultText     string
	ExitCode       sql.NullInt64
	CreatedBy      string
	Scope          string
	ClaimedAt      sql.NullInt64
	LeaseEpoch     int64 // 租约代次（0=未领取过；首次领取置 1，过期重领自增）
	RunningAt      sql.NullInt64
	FinishedAt     sql.NullInt64
	SubmittedKey   sql.NullString
	ArchivedKey    sql.NullString
	Archived       bool
	ResultRecorded bool
	CreatedAt      int64
	UpdatedAt      int64
}

// commandColumns 为命令查询列清单（join nodes 取节点名）。
const commandColumns = `
	c.command_id, c.node_id, n.name, c.kind, c.args_json, c.status, c.degraded,
	c.timeout_s, c.result_text, c.exit_code, c.created_by, c.created_by_token_scope,
	c.claimed_at, c.lease_epoch, c.running_at, c.finished_at, c.submission_key, c.archived_key,
	c.archived, c.result_recorded, c.created_at, c.updated_at`

// commandColumnsPlain 为无别名纯列清单（ClaimNodeCommands 的 RETURNING 用——
// UPDATE 无表别名，且 RETURNING 不能 join）。列序与 commandColumns 一致（仅少
// 节点名一列），两处必须同步维护。
const commandColumnsPlain = `
	command_id, node_id, kind, args_json, status, degraded,
	timeout_s, result_text, exit_code, created_by, created_by_token_scope,
	claimed_at, lease_epoch, running_at, finished_at, submission_key, archived_key,
	archived, result_recorded, created_at, updated_at`

func scanCommand(row rowScanner) (*CommandRecord, error) {
	var r CommandRecord
	var degraded, archived, recorded int
	err := row.Scan(&r.CommandID, &r.NodeID, &r.NodeName, &r.Kind, &r.ArgsJSON, &r.Status,
		&degraded, &r.TimeoutS, &r.ResultText, &r.ExitCode, &r.CreatedBy, &r.Scope,
		&r.ClaimedAt, &r.LeaseEpoch, &r.RunningAt, &r.FinishedAt, &r.SubmittedKey, &r.ArchivedKey,
		&archived, &recorded, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Degraded = degraded == 1
	r.Archived = archived == 1
	r.ResultRecorded = recorded == 1
	return &r, nil
}

// scanCommandPlain 扫描 RETURNING 产物（无节点名列——领取方即节点本身，
// NodeName 留空）。
func scanCommandPlain(row rowScanner) (*CommandRecord, error) {
	var r CommandRecord
	var degraded, archived, recorded int
	err := row.Scan(&r.CommandID, &r.NodeID, &r.Kind, &r.ArgsJSON, &r.Status,
		&degraded, &r.TimeoutS, &r.ResultText, &r.ExitCode, &r.CreatedBy, &r.Scope,
		&r.ClaimedAt, &r.LeaseEpoch, &r.RunningAt, &r.FinishedAt, &r.SubmittedKey, &r.ArchivedKey,
		&archived, &recorded, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Degraded = degraded == 1
	r.Archived = archived == 1
	r.ResultRecorded = recorded == 1
	return &r, nil
}

// SubmitCommand 单事务完成一次命令提交（SPEC-M1d §1/§2）：
//  1. 幂等键判定（submission_key 非空时）：命中既有行——同参（kind/args/node/
//     timeout）→ 返回原行（idempotent=true）；异参 → ErrKeyConflict；
//  2. 配额计数：pending+claimed+running（未归档）≥ CommandQuotaPerNode →
//     ErrQuotaExceeded（事务内计数，杜绝并发提交击穿）；
//  3. 插入新行（command_id 由调用方生成 uuid，永不复用）。
//
// 同键过期复用（窗口外）不在此处：ArchiveSubmissionKey 由调用方按窗口判定后
// 调用（读路径定窗口、写路径归档，职责分离）——同键同参与同键异参同口径
// （R39-#6：同参命中超窗同样归档复用，不永久返回旧命令）。
func (s *Store) SubmitCommand(ctx context.Context, in *CommandRow) (row *CommandRecord, idempotent bool, err error) {
	if err := s.writable(); err != nil {
		return nil, false, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if in.SubmittedKey.Valid && in.SubmittedKey.String != "" {
		existing, err := scanCommand(tx.QueryRowContext(ctx,
			`SELECT `+commandColumns+` FROM commands c JOIN nodes n ON n.id = c.node_id
			 WHERE c.submission_key = ?`, in.SubmittedKey.String))
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			same := existing.NodeID == in.NodeID && existing.Kind == in.Kind &&
				existing.ArgsJSON == in.ArgsJSON && existing.TimeoutS == in.TimeoutS
			if !same {
				return nil, false, ErrKeyConflict
			}
			return existing, true, tx.Commit()
		}
	}

	var live int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM commands
WHERE node_id = ? AND archived = 0 AND status IN ('pending','claimed','running')`,
		in.NodeID).Scan(&live); err != nil {
		return nil, false, err
	}
	if live >= CommandQuotaPerNode {
		return nil, false, ErrQuotaExceeded
	}

	degraded, archived := 0, 0
	if in.Degraded {
		degraded = 1
	}
	if in.Archived {
		archived = 1
	}
	var exitCode any
	if in.ExitCode.Valid {
		exitCode = in.ExitCode.Int64
	}
	var claimedAt, runningAt, finishedAt any
	if in.ClaimedAt.Valid {
		claimedAt = in.ClaimedAt.Int64
	}
	if in.RunningAt.Valid {
		runningAt = in.RunningAt.Int64
	}
	if in.FinishedAt.Valid {
		finishedAt = in.FinishedAt.Int64
	}
	var subKey any
	if in.SubmittedKey.Valid {
		subKey = in.SubmittedKey.String
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO commands(command_id, node_id, kind, args_json, status, degraded, timeout_s,
                     result_text, exit_code, created_by, created_by_token_scope,
                     claimed_at, running_at, finished_at, submission_key,
                     archived, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.CommandID, in.NodeID, in.Kind, in.ArgsJSON, in.Status, degraded, in.TimeoutS,
		in.ResultText, exitCode, in.CreatedBy, in.Scope,
		claimedAt, runningAt, finishedAt, subKey,
		archived, in.CreatedAt, in.UpdatedAt); err != nil {
		return nil, false, err
	}
	row, err = scanCommand(tx.QueryRowContext(ctx,
		`SELECT `+commandColumns+` FROM commands c JOIN nodes n ON n.id = c.node_id
		 WHERE c.command_id = ?`, in.CommandID))
	if err != nil {
		return nil, false, err
	}
	return row, false, tx.Commit()
}

// GetSubmissionKeyCreatedAt 取幂等键对应行的创建时刻（cmdsvc 的窗口判定用——
// 窗口内同键异参 409，窗口过期才归档复用）。键不存在返回 ok=false。
func (s *Store) GetSubmissionKeyCreatedAt(ctx context.Context, key string) (createdAt int64, ok bool, err error) {
	err = s.read.QueryRowContext(ctx,
		`SELECT created_at FROM commands WHERE submission_key = ?`, key).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return createdAt, true, nil
}

// ArchiveSubmissionKey 把旧行的 submission_key 移入 archived_key 并置 NULL
// （DESIGN §4.1-D 幂等窗口过期归档：唯一索引放行同名键，command_id 永不复用）。
// keyMissing=false 表示该键已不存在（并发下已被归档/清理）——调用方重试提交即可。
func (s *Store) ArchiveSubmissionKey(ctx context.Context, key string, now int64) (bool, error) {
	if err := s.writable(); err != nil {
		return false, err
	}
	res, err := s.write.ExecContext(ctx, `
UPDATE commands SET archived_key = submission_key, submission_key = NULL, updated_at = ?
WHERE submission_key = ?`, now, key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClaimNodeCommands 原子领取该节点可下发的命令（SPEC §2：领取原子性由 console
// 保证——响应写入前置 claimed + claimed_at）。可领 = 未归档且（pending，或
// claimed/running 且租约已过期——lease 起点回 pending 语义，重发至终态）；
// l2Allowed 双重复核在事务内重新读取 nodes.l2_allowed（提交后配置变更/节点
// 删除的窗口内不再下发）。单条 UPDATE … WHERE id IN (SELECT … LIMIT) 在写锁内
// 完成，双实例并发心跳不可能领到同一行；RETURNING 直接回取已领取行。
// 租约代次（R41-B1）：lease_epoch 首次领取置 1（列默认 0）、过期重领自增——
// 代次前移即旧代次的在途执行与回执整体作废。
func (s *Store) ClaimNodeCommands(ctx context.Context, nodeID int64, now int64, leaseS int64, limit int) ([]CommandRecord, error) {
	if err := s.writable(); err != nil {
		return nil, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var l2 int
	if err := tx.QueryRowContext(ctx, `SELECT l2_allowed FROM nodes WHERE id = ?`, nodeID).Scan(&l2); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // 节点已被删除：无可领
		}
		return nil, err
	}
	if l2 != 1 {
		return nil, nil // 下发白名单外（云节点）：领取路径复核即拒绝
	}
	leaseCutoff := now - leaseS
	rows, err := tx.QueryContext(ctx, `
UPDATE commands SET status = 'claimed', claimed_at = ?, running_at = NULL,
                     lease_epoch = lease_epoch + 1, updated_at = ?
WHERE command_id IN (
	SELECT command_id FROM commands
	WHERE node_id = ? AND archived = 0
	  AND (status = 'pending'
	       OR (status IN ('claimed','running') AND claimed_at IS NOT NULL AND claimed_at <= ?))
	ORDER BY created_at
	LIMIT ?
)
RETURNING `+commandColumnsPlain, now, now, nodeID, leaseCutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommandRecord
	for rows.Next() {
		r, err := scanCommandPlain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// ResultIn 为 agent 回执的入库输入（归属验证所需的协议字段齐全）。
type ResultIn struct {
	CommandID   string
	ClaimedAt   int64 // agent 回显的租约起点（审计留痕；归属验证见 LeaseEpoch）
	LeaseEpoch  int64 // agent 回显的租约代次（与库值一致才接受，唯一权威令牌）
	ExitCode    sql.NullInt64
	ResultText  string
	Interrupted bool // agent 重启发现 intent 无结果：置 unknown 对齐（DESIGN §4.1-D）
	RecordedAt  int64
}

// ResultOutcome 为单条回执的处理结果。
type ResultOutcome struct {
	CommandID  string
	Ack        bool   // 已落库（本轮及后续心跳持续 ACK 直至 agent 删除）
	LeaseEpoch int64  // Ack=行当前代次（回执代次）；Drop=被拒回执自陈代次（agent 本地清理目标）
	Drop       bool   // 拒绝且不可重发（无归属/超限/旧代次/重复冲突）——agent 可删本地副本
	DropWhy    string // Drop=true 时的拒绝原因（审计日志用）
	Status     string // 落库后的命令状态（Ack=true 时有意义）
}

// RecordCommandResults 批量处理回执（同一事务）：逐条归属验证——
//   - 本节点 + lease_epoch 与库值一致（当前代次）且 status ∈ {claimed, running}
//     → 终态（succeeded/failed 按 exit code）；
//   - 本节点 + 当前代次 + status = unknown → 修正为实际终态，degraded 保留
//     （迟到回执修正，不误判 failed）；
//   - 本节点 + 已有 result_recorded 且当前代次 → 幂等命中（ACK 重发窗口内的
//     重复回执），不覆盖既有结果；
//   - 本节点 + 旧代次（lease_epoch < 库值，租约过期重领后的迟到回执）→ 忽略
//     内容并拒绝（DropWhy "stale epoch"；R41-B1：旧代次回执永不触达新代次
//     状态，console 侧计数审计），agent 收 drop 后删该代次本地副本；
//   - 其余（无归属/pending/archived/未知命令）→ 拒绝。
//     interrupted=true 的回执：当前代次匹配时置 unknown + degraded + 记录结果
//     （agent 重启对齐，DESIGN §4.1-D），并 ACK（回执已记录，agent 可删）。
//     超过 64KB 的 result_text 拒绝（协议违规，agent 侧 64KB 硬上限被绕过）。
func (s *Store) RecordCommandResults(ctx context.Context, nodeID int64, results []ResultIn) ([]ResultOutcome, error) {
	if err := s.writable(); err != nil {
		return nil, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]ResultOutcome, 0, len(results))
	for _, r := range results {
		// LeaseEpoch 初始为回执自陈代次：Drop 路径即 agent 本地清理目标
		// （drop_ids 条目携带它，agent 定点删该代次副本——缺了它 agent 找不到
		// 文件、旧回执会被无限重发）；Ack 路径在下方各分支改写为行当前代次。
		oc := ResultOutcome{CommandID: r.CommandID, LeaseEpoch: r.LeaseEpoch}
		if len(r.ResultText) > CommandResultsMaxBytes {
			oc.Drop = true
			oc.DropWhy = "oversize"
			out = append(out, oc)
			continue
		}
		row, err := scanCommand(tx.QueryRowContext(ctx,
			`SELECT `+commandColumns+` FROM commands c JOIN nodes n ON n.id = c.node_id
			 WHERE c.command_id = ?`, r.CommandID))
		if err != nil {
			return nil, err
		}
		if row == nil || row.NodeID != nodeID {
			oc.Drop = true // 未知命令/跨节点回执：无归属，拒绝
			oc.DropWhy = "no attribution"
			out = append(out, oc)
			continue
		}
		epochMatch := row.LeaseEpoch > 0 && row.LeaseEpoch == r.LeaseEpoch
		switch {
		case !epochMatch:
			// 旧代次迟到回执（或未领取/畸形代次）：内容忽略、状态不动（新代次
			// 的执行不受污染——R41-B1 的 console 侧收口），拒绝以停掉 agent 的
			// 无效重发并让其清理该代次副本。
			oc.Drop = true
			oc.DropWhy = "stale epoch"
		case row.ResultRecorded:
			// ACK 重发窗口内的重复回执：幂等命中，不覆盖已落库结果。
			oc.Ack, oc.Status, oc.LeaseEpoch = true, row.Status, row.LeaseEpoch
		case r.Interrupted:
			// agent 重启发现 intent 无结果：置 unknown 对齐（非 failed），
			// 结果记录后即可 ACK——agent 删除本地对账文件，console 保留 degraded 行。
			if _, err := tx.ExecContext(ctx, `
UPDATE commands SET status = 'unknown', degraded = 1, result_text = ?, exit_code = NULL,
                     result_recorded = 1, updated_at = ? WHERE command_id = ?`,
				r.ResultText, r.RecordedAt, r.CommandID); err != nil {
				return nil, err
			}
			oc.Ack, oc.Status, oc.LeaseEpoch = true, CommandUnknown, row.LeaseEpoch
		case row.Status == CommandClaimed || row.Status == CommandRunning:
			status := CommandFailed
			if r.ExitCode.Valid && r.ExitCode.Int64 == 0 {
				status = CommandSucceeded
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE commands SET status = ?, result_text = ?, exit_code = ?, result_recorded = 1,
                     finished_at = ?, updated_at = ? WHERE command_id = ?`,
				status, r.ResultText, boolToNullInt(r.ExitCode.Valid, r.ExitCode.Int64),
				r.RecordedAt, r.RecordedAt, r.CommandID); err != nil {
				return nil, err
			}
			oc.Ack, oc.Status, oc.LeaseEpoch = true, status, row.LeaseEpoch
		case row.Status == CommandUnknown:
			// 迟到回执修正（SPEC §2）：当前代次匹配（command_id + lease_epoch）→
			// 修正为实际终态，degraded 历史保留。
			status := CommandFailed
			if r.ExitCode.Valid && r.ExitCode.Int64 == 0 {
				status = CommandSucceeded
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE commands SET status = ?, result_text = ?, exit_code = ?, result_recorded = 1,
                     finished_at = ?, updated_at = ? WHERE command_id = ?`,
				status, r.ResultText, boolToNullInt(r.ExitCode.Valid, r.ExitCode.Int64),
				r.RecordedAt, r.RecordedAt, r.CommandID); err != nil {
				return nil, err
			}
			oc.Ack, oc.Status, oc.LeaseEpoch = true, status, row.LeaseEpoch
		default:
			oc.Drop = true // pending/archived：未领取或已过期，无归属
			oc.DropWhy = "state " + row.Status
		}
		out = append(out, oc)
	}
	return out, tx.Commit()
}

// MarkCommandsRunning 把 agent 报告的执行中命令 claimed → running（SPEC §1 状态
// 机；仅本节点 claimed 态命令受影响，running 重复报告幂等）。返回受影响行数。
func (s *Store) MarkCommandsRunning(ctx context.Context, nodeID int64, ids []string, now int64) (int64, error) {
	if err := s.writable(); err != nil || len(ids) == 0 {
		return 0, err
	}
	var n int64
	for _, id := range ids {
		res, err := s.write.ExecContext(ctx, `
UPDATE commands SET status = 'running', running_at = ?, updated_at = ?
WHERE node_id = ? AND command_id = ? AND status = 'claimed' AND archived = 0`, now, now, nodeID, id)
		if err != nil {
			return n, err
		}
		if a, err := res.RowsAffected(); err == nil {
			n += a
		}
	}
	return n, nil
}

// AckableEntry 为 ACK 名单条目：命令 id + 已记录回执的租约代次（agent 据此
// 只清理该代次及更早的本地对账副本，不触达更高代次——R41-B1）。
type AckableEntry struct {
	CommandID  string
	LeaseEpoch int64
}

// AckableCommandIDs 返回 agent 仍持有、且 console 已记录回执的命令（ACK 名单
// ——agent 收到后才删本地持久化结果；sweep 产生的 unknown 无回执，不在名单）。
// 回执落库即终态/unknown、此后不再重领，故回执代次恒等于行当前 lease_epoch；
// 仍以库值为准返回（不信任「应该相等」）。
func (s *Store) AckableCommandIDs(ctx context.Context, nodeID int64, unacked []string) ([]AckableEntry, error) {
	out := make([]AckableEntry, 0, len(unacked))
	for _, id := range unacked {
		var recorded int
		var epoch int64
		err := s.read.QueryRowContext(ctx,
			`SELECT result_recorded, lease_epoch FROM commands WHERE node_id = ? AND command_id = ?`,
			nodeID, id).Scan(&recorded, &epoch)
		if errors.Is(err, sql.ErrNoRows) {
			continue // console 已清理（90 天）或无此命令：agent 副本自然过期，跳过
		}
		if err != nil {
			return nil, err
		}
		if recorded == 1 {
			out = append(out, AckableEntry{CommandID: id, LeaseEpoch: epoch})
		}
	}
	return out, nil
}

// SweepUnknown 把无回执的 claimed/running 命令置 unknown + degraded（SPEC §2：
// lease×2 或 running 后 timeout×1.5 到期无回执 → unknown，非 failed）。
// 迁移行数。unknown 不可再领取（永不自动重发），迟到回执经修正路径恢复终态。
func (s *Store) SweepUnknown(ctx context.Context, now int64, leaseS int64) (int64, error) {
	if err := s.writable(); err != nil {
		return 0, err
	}
	res, err := s.write.ExecContext(ctx, `
UPDATE commands SET status = 'unknown', degraded = 1, updated_at = ?
WHERE archived = 0 AND status IN ('claimed','running') AND (
	(claimed_at IS NOT NULL AND claimed_at <= ?)
	OR (running_at IS NOT NULL AND running_at + (3 * timeout_s) / 2 <= ?)
)`, now, now-2*leaseS, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ArchiveStalePending 把离线节点过期未领（创建超 24h）的 pending 命令标记归档
// （SPEC §2：离线 pending 24h → archived；archived 为生命周期属性，行保留审计）。
func (s *Store) ArchiveStalePending(ctx context.Context, now, createdCutoff int64) (int64, error) {
	if err := s.writable(); err != nil {
		return 0, err
	}
	res, err := s.write.ExecContext(ctx, `
UPDATE commands SET archived = 1, updated_at = ?
WHERE archived = 0 AND status = 'pending' AND created_at <= ?
  AND node_id IN (SELECT id FROM nodes WHERE status != 'online')`, now, createdCutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupCommands 删除创建时间早于 cutoff 的命令行（SPEC §5：保留 90 天，
// 随 metrics retention 模式每小时清理）。返回删除行数。
func (s *Store) CleanupCommands(ctx context.Context, cutoff int64) (int64, error) {
	if err := s.writable(); err != nil {
		return 0, err
	}
	res, err := s.write.ExecContext(ctx, `DELETE FROM commands WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetCommand 按 command_id 取命令（join 节点名）；不存在返回 nil, nil。
func (s *Store) GetCommand(ctx context.Context, commandID string) (*CommandRecord, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+commandColumns+` FROM commands c JOIN nodes n ON n.id = c.node_id
		 WHERE c.command_id = ?`, commandID)
	return scanCommand(row)
}

// ListCommands 列出命令（按创建时间倒序；nodeName/status 过滤可为空；limit 上限
// 由调用方约束）。审计查询走本入口，历史范围强制 LIMIT（DESIGN §5 约定）。
func (s *Store) ListCommands(ctx context.Context, nodeName, status string, limit int) ([]CommandRecord, error) {
	q := `SELECT ` + commandColumns + ` FROM commands c JOIN nodes n ON n.id = c.node_id WHERE 1=1`
	var args []any
	if nodeName != "" {
		q += ` AND n.name = ?`
		args = append(args, nodeName)
	}
	if status != "" {
		q += ` AND c.status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY c.created_at DESC, c.command_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommandRecord
	for rows.Next() {
		r, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// NodeManagedUnits 返回该节点心跳上报的受管 systemd unit 集合（type=systemd、
// 非 stale；提交侧 unit 槽集合校验依据——与 agent 本地配置声明双端各自校验）。
func (s *Store) NodeManagedUnits(ctx context.Context, nodeID int64) (map[string]bool, error) {
	rows, err := s.read.QueryContext(ctx, `
SELECT target FROM services WHERE node_id = ? AND type = 'systemd' AND status != 'stale'`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out[t] = true
	}
	return out, rows.Err()
}

func boolToNullInt(valid bool, v int64) any {
	if !valid {
		return nil
	}
	return v
}
