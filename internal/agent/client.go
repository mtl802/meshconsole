package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/agent/collect"
)

const httpTimeout = 10 * time.Second

// Register 向控制台申请节点凭据并返回待持久化的 state。
func Register(ctx context.Context, consoleURL, regToken, name, role, osName, arch string) (*State, error) {
	consoleURL = strings.TrimRight(consoleURL, "/")
	body, _ := json.Marshal(map[string]string{
		"name": name, "role": role, "os": osName, "arch": arch,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, consoleURL+"/api/agent/register", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+regToken)

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect console: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read register response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("register rejected: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Status    string `json:"status"`
		NodeID    int64  `json:"node_id"`
		NodeName  string `json:"node_name"`
		NodeToken string `json:"node_token"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse register response: %w", err)
	}
	if out.NodeToken == "" {
		return nil, fmt.Errorf("register response missing node_token")
	}
	return &State{
		ConsoleURL:   consoleURL,
		NodeID:       out.NodeID,
		Name:         out.NodeName,
		Role:         role,
		NodeToken:    out.NodeToken,
		RegisteredAt: time.Now().Unix(),
	}, nil
}

// reportOnce 采集并上报一次心跳。请求绑定调用方 ctx，取消/退出时立即中断（审查 R1-#10）。
func reportOnce(ctx context.Context, client *http.Client, consoleURL, nodeToken, name, version string, c *collect.Collector) error {
	snap := c.Collect()
	body, err := json.Marshal(map[string]any{
		"node":           name,
		"agent_version":  version,
		"metrics":        snap,
		"collect_errors": snap.Errors,
	})
	if err != nil {
		return fmt.Errorf("encode heartbeat: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, consoleURL+"/api/agent/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+nodeToken)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		// 节点 token 失效/被吊销：不自动重注册，持续退避并显式报错，等待人工处置。
		return fmt.Errorf("heartbeat rejected: HTTP 401 (node token invalid or revoked)")
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("heartbeat rejected: HTTP 403 (node identity mismatch)")
	default:
		return fmt.Errorf("heartbeat rejected: HTTP %d", resp.StatusCode)
	}
}
