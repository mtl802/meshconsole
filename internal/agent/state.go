// Package agent 实现 meshagent 运行时：本地 state 持久化、注册与心跳上报。
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State 为 agent 本地持久化的注册凭据（JSON 文件，0600）。
// 断线/睡眠恢复后凭此续接心跳，不重复注册（DESIGN §4.1）。
type State struct {
	ConsoleURL   string `json:"console_url"`
	NodeID       int64  `json:"node_id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	NodeToken    string `json:"node_token"`
	RegisteredAt int64  `json:"registered_at"`
}

// ErrNotRegistered 表示本地尚无注册 state。
var ErrNotRegistered = fmt.Errorf("not registered: run `meshagent register` first")

// LoadState 读取本地 state。
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ErrNotRegistered
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	st := &State{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if st.NodeToken == "" || st.ConsoleURL == "" {
		return nil, fmt.Errorf("state %s incomplete: node_token/console_url missing", path)
	}
	return st, nil
}

// SaveState 原子写入 state 文件（唯一临时文件 + rename，创建即 0600）。
// 临时文件用 os.CreateTemp 生成唯一名并直接以 0600 创建，避免固定名
// 并发覆盖/符号链接注入风险（审查 R1-#12）；不再事后 chmod 补救。
func SaveState(path string, st *State) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create state dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("create state tmp: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil { // 防御 umask 差异
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod state tmp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write state tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close state tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}
