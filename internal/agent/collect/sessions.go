// 会话活跃采集（SPEC-M1d §3.5，伦哥 20:35 需求：能看到当前 agent 会话在干嘛）。
// 进程级采集（agenttasks.go）把常驻 daemon（codex app-server，elapsed 数天）
// 误当任务，真正的活跃会话反而不可见——本文件以会话文件为信号源补齐逻辑视图：
// 各 agent 会话目录中 mtime 在活跃窗口（30min）内的会话文件为活跃会话。
//
// 内容解析降级（R41-B2，伦哥定案 ②：降级收口）：topic/recent_action 字段保留
// 但恒为空串——各 CLI 会话文件格式不一（zcode rollout 为 model_io 嵌套结构、
// Codex 为 response_item.payload.content[] 嵌套 JSONL），无真实样本支撑的轻
// 解析只会产出恒空的「假内容」；面板对空主题显示会话文件名 + mtime（普通
// 空态样式，不报错）。Codex/各格式解析留 M1e 按真实样本实现。附带收益：采集
// 不再读会话文件内容（只 stat），FIFO/大文件/坏编码不再有任何影响面。
//
// 目录遍历与 statDirTree 同款分批 readdir + ctx + 条目配额（R29-#1 口径），
// 触顶/超预算如实上报截断或缺席，不无界读盘。
package collect

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// SessionActiveWindow 为会话活跃窗口：mtime 距今小于该值的会话文件算活跃
	// （SPEC §3.5：30 分钟）。
	SessionActiveWindow = 30 * time.Minute
	// MaxAgentSessions 为单拍上报的活跃会话数上限（与 agent_tasks 64 对齐）。
	MaxAgentSessions = 64
	// MaxSessionEntries 由 agenttasks.go 定义（目录遍历条目配额，复用同款）。
)

// AgentSession 为一个活跃 agent 会话（JSON 对应 console agent_sessions 表行）。
// Topic/RecentAction 自 R41-B2 降级起恒为空串（协议字段保留，M1e 恢复解析）。
type AgentSession struct {
	AgentName    string `json:"agent_name"`
	SessionFile  string `json:"session_file"`
	StartedAt    int64  `json:"started_at,omitempty"`
	LastActivity int64  `json:"last_activity"`
	Topic        string `json:"topic,omitempty"`
	RecentAction string `json:"recent_action,omitempty"`
}

// ScanSessions 扫描各 agent 会话目录中的活跃会话。返回 (sessions, truncated,
// ok)：ctx 取消/超预算 → ok=false（整轮未知，调用方按字段缺席处理，不外泄
// 半程结果）；truncated = 活跃会话数触顶 MaxAgentSessions 或目录遍历条目触顶
// （清单是子集，调用方如实并进 collect_errors）。单 agent 目录缺失/无权限照常
// 跳过（该 agent 无会话上报）。
func (s *AgentTaskScanner) ScanSessions(ctx context.Context) (sessions []AgentSession, truncated, ok bool) {
	if err := ctx.Err(); err != nil {
		return nil, false, false
	}
	now := s.now().Unix()
	windowCutoff := now - int64(SessionActiveWindow/time.Second)
	sessions = make([]AgentSession, 0, 8)
	names := make([]string, 0, len(s.sessionDirs))
	for name := range s.sessionDirs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dir := s.sessionDirs[name]
		if dir == "" {
			continue
		}
		// 目录缺失/非目录：该 agent 无会话，照常跳过（与 scanActivity 的缺席
		// 语义一致，不算扫描失败）——walkSessionTree 的 ok=false 只保留给
		// ctx 取消/超预算（整轮未知）。
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		localTrunc, localOK := walkSessionTree(ctx, dir, MaxSessionEntries, dirReadBatch,
			func(path string, fi os.FileInfo) {
				if truncated || len(sessions) >= MaxAgentSessions {
					truncated = true
					return
				}
				mt := fi.ModTime().Unix()
				if mt <= windowCutoff {
					return // 非活跃会话（SPEC：mtime < 30min 严格小于，恰满 30min 不活跃）
				}
				// 内容解析降级（R41-B2）：topic/recent_action 恒空，会话以
				// 文件路径 + mtime 在列（面板空态显示文件名 + 修改时间）。
				sessions = append(sessions, AgentSession{
					AgentName:    name,
					SessionFile:  path,
					StartedAt:    fileBirthTime(fi),
					LastActivity: mt,
				})
			})
		if !localOK {
			return nil, false, false // ctx 取消/超预算：整轮未知
		}
		if localTrunc {
			truncated = true
		}
	}
	return sessions, truncated, true
}

// walkSessionTree 遍历目录树并对每个普通文件调用 visit。与 statDirTree 的
// walkDirTree 同款显式栈式 DFS + 分批 readdir（R29-#1）：批间查 ctx、批内查
// 条目累计配额（文件+目录合计，根占 1 位），触顶立即收敛返回 truncated=true；
// ctx 取消/超预算返回 ok=false。visit 内只 stat（不读文件内容），调用方
// （ScanSessions）以 MaxAgentSessions 计数触顶后自行短路。
func walkSessionTree(ctx context.Context, root string, entryCap int64, batchSize int, visit func(path string, fi os.FileInfo)) (truncated, ok bool) {
	if err := ctx.Err(); err != nil {
		return false, false
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return false, false // 目录缺失/无权限：该 agent 无会话，照常跳过
	}
	entries := int64(1) // 根目录自身占 1 位（与 walkDirTree 计数口径一致）
	if entries >= entryCap {
		return true, true
	}
	dirs := []string{root}
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		f, err := os.Open(dir)
		if err != nil {
			continue // 单目录打开失败跳过该子树（与 walkDirTree 的 SkipDir 口径）
		}
		for {
			if cerr := ctx.Err(); cerr != nil {
				f.Close()
				return false, false
			}
			batch, rerr := f.ReadDir(batchSize)
			for i := range batch {
				e := batch[i]
				join := filepath.Join(dir, e.Name())
				if e.IsDir() {
					dirs = append(dirs, join)
				} else if fi, ierr := e.Info(); ierr == nil {
					visit(join, fi)
				}
				entries++
				if entries >= entryCap {
					f.Close()
					return true, true
				}
			}
			if rerr != nil {
				f.Close()
				break
			}
		}
	}
	return false, true
}
