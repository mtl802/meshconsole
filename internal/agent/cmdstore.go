// cmdstore.go 为 agent 本地的命令执行对账存储（SPEC-M1d §2：执行完本地持久化
// 结果；ACK 前重启恢复重发）。SPEC 指定「结果目录 .pipeline 或 state 同目录」
// ——取 state 文件同目录的 commands/ 子目录，一命令一代次一 JSON 文件
// （<command_id>.e<lease_epoch>.json，0600，原子写与 state 文件同款）。
//
// 对账文件以租约代次命名（R41-B1 深修）：同 command_id 的新旧代次各占一个
// 文件，旧代次的清理（drop/ACK/retire）只触达其自身代次及更早的文件，结构上
// 不可能误删新代次已写入的结果——原「一命令一文件」形态下旧代次 drop 会把
// 新代次刚落盘的结果一并删掉，导致回执丢失、console 判 unknown 后重执行。
//
// 两个阶段（DESIGN §4.1-D 去重记录持久化）：
//   - intent（执行前落盘）：command_id + 租约代次 + 领取参数——崩溃重启后据此
//     识别「已领取」，重复 command_id 不重复执行；
//   - result（执行后覆盖）：exit_code + 输出——重启后继续上报直至 console ACK
//     （ack 条目带代次，到达才删该代次及更早的文件），防丢回执。
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CommandPhase 为本地对账文件阶段。
type CommandPhase string

const (
	PhaseIntent CommandPhase = "intent"
	PhaseResult CommandPhase = "result"
)

// CommandRecord 为本地对账文件内容（一条命令一行对账）。
type CommandRecord struct {
	CommandID  string       `json:"command_id"`
	LeaseEpoch int64        `json:"lease_epoch"` // 租约代次（文件名的一部分，清理的归属依据）
	Kind       string       `json:"kind"`
	ArgsJSON   string       `json:"args_json"`
	TimeoutS   int64        `json:"timeout_s"`
	ClaimedAt  int64        `json:"claimed_at"`
	Phase      CommandPhase `json:"phase"`
	// 以下仅 result 阶段有效。
	ExitCode    *int64 `json:"exit_code"`
	ResultText  string `json:"result_text"`
	Interrupted bool   `json:"interrupted,omitempty"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
}

// recordName 匹配带代次的对账文件名（<command_id>.e<epoch>.json；id 由 console
// 签发 uuid，此处只按「.e<数字>.json 结尾」的形态判定）。目录内其余 *.json 一律
// 视为旧版「一命令一文件」升级残留（PurgeLegacy 清除）——临时文件（CreateTemp
// 产物）不带 .json 后缀，不受影响。
var recordName = regexp.MustCompile(`\.e[1-9][0-9]*\.json\z`)

// CommandStore 为本地命令对账目录（state 同目录 commands/）。
type CommandStore struct {
	dir string
}

// NewCommandStore 打开（并确保存在）本地对账目录；目录权限 0700、文件 0600
// ——执行输出可能含服务名/路径等主机信息，不给全局读。
func NewCommandStore(stateFile string) (*CommandStore, error) {
	dir := filepath.Join(filepath.Dir(stateFile), "commands")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create command store dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod command store dir: %w", err)
	}
	return &CommandStore{dir: dir}, nil
}

// PurgeLegacy 删除旧版无代次对账文件（升级残留：无法归属到任何租约代次，
// 上报必被 console 拒且永不清理）。命令的租约过期后由 console 重发，不丢账。
// 返回被清除的文件名清单（调用方记日志）。
func (cs *CommandStore) PurgeLegacy() ([]string, error) {
	entries, err := os.ReadDir(cs.dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || recordName.MatchString(name) {
			continue
		}
		if err := os.Remove(filepath.Join(cs.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("remove legacy record %s: %w", name, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}

func (cs *CommandStore) path(commandID string, epoch int64) string {
	return filepath.Join(cs.dir, fmt.Sprintf("%s.e%d.json", commandID, epoch))
}

// save 原子写入对账文件（唯一临时文件 + rename，创建即 0600，与 SaveState 同款）。
func (cs *CommandStore) save(rec *CommandRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(cs.dir, rec.CommandID+".e"+fmt.Sprint(rec.LeaseEpoch)+".tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(name, cs.path(rec.CommandID, rec.LeaseEpoch)); err != nil {
		os.Remove(name)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// SaveIntent 执行前落盘 intent（同代次已存在同 id 文件时不覆盖——intent 幂等）。
func (cs *CommandStore) SaveIntent(rec *CommandRecord) error {
	rec.Phase = PhaseIntent
	rec.ExitCode, rec.ResultText, rec.FinishedAt = nil, "", 0
	if _, err := os.Stat(cs.path(rec.CommandID, rec.LeaseEpoch)); err == nil {
		return nil // 已有本代次 intent/result：不覆盖（重复下发不重置对账状态）
	}
	return cs.save(rec)
}

// SaveResult 执行后落盘结果（覆盖本代次 intent）。
func (cs *CommandStore) SaveResult(rec *CommandRecord) error {
	rec.Phase = PhaseResult
	return cs.save(rec)
}

// Load 读单条对账记录（command_id + 代次定位文件）；不存在返回 nil。
func (cs *CommandStore) Load(commandID string, epoch int64) (*CommandRecord, error) {
	data, err := os.ReadFile(cs.path(commandID, epoch))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec := &CommandRecord{}
	if err := json.Unmarshal(data, rec); err != nil {
		return nil, fmt.Errorf("parse command record %s e%d: %w", commandID, epoch, err)
	}
	return rec, nil
}

// LoadAll 列出全部对账记录（损坏文件跳过并记入返回的告警清单，不阻塞心跳）。
func (cs *CommandStore) LoadAll() (recs []*CommandRecord, bad []string, err error) {
	entries, err := os.ReadDir(cs.dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue // 临时文件（.tmp）非对账记录
		}
		data, err := os.ReadFile(filepath.Join(cs.dir, name))
		if err != nil {
			bad = append(bad, name)
			continue
		}
		rec := &CommandRecord{}
		if err := json.Unmarshal(data, rec); err != nil {
			bad = append(bad, name)
			continue
		}
		recs = append(recs, rec)
	}
	return recs, bad, nil
}

// Delete 删除指定代次的对账文件（drop/retire 的定点清理——只触达该代次）。
func (cs *CommandStore) Delete(commandID string, epoch int64) error {
	err := os.Remove(cs.path(commandID, epoch))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// DeleteUpTo 删除 command_id 代次 ≤ maxEpoch 的全部对账文件（ACK 后清理：
// console 确认的是已记录回执的代次，该代次及更早的残留副本一并出清，更高
// 代次的在途执行与结果不可触达——R41-B1）。目录扫描实现：不按 epoch 逐个
// 试删（畸形大 epoch 不产生空转）。
func (cs *CommandStore) DeleteUpTo(commandID string, maxEpoch int64) error {
	entries, err := os.ReadDir(cs.dir)
	if err != nil {
		return err
	}
	prefix := commandID + ".e"
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		var epoch int64
		if _, err := fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json"), "%d", &epoch); err != nil {
			continue // 非本实现的命名（理论上不存在），不动
		}
		if epoch > maxEpoch {
			continue
		}
		if err := os.Remove(filepath.Join(cs.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
