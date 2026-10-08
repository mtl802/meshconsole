package registry

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/cmdsvc"
	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

// ---- M1d（SPEC-M1d §2/§5）心跳命令协议端到端测试 ----
// 覆盖：caps 落库、提交→心跳领取（响应携带 commands 且 claimed_at 回显）、
// 回执→ACK（下一拍）、无归属回执 drop、running 报告、重复回执幂等。

// seedCmdNode 经注册 API 建节点（返回明文 token）并打开 l2 白名单
// （与 console 侧 SyncL2Allowed 同效）。
func seedCmdNode(t *testing.T, h *Handler, st *store.Store, name string) string {
	t.Helper()
	tok, _ := mustRegister(t, h, name)
	if err := st.SyncL2Allowed(t.Context(), []string{name}); err != nil {
		t.Fatal(err)
	}
	return tok
}

func decodeHeartbeatResp(t *testing.T, body []byte) heartbeatResp {
	t.Helper()
	var out heartbeatResp
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode heartbeat resp: %v\n%s", err, body)
	}
	return out
}

func TestHeartbeatCommandProtocol(t *testing.T) {
	h, st := newTestHandler(t)
	ctx := t.Context()
	macTok := seedCmdNode(t, h, st, "mac-mini")

	// 心跳（带 caps）→ nodes.caps 落库；响应无命令。
	rec := doReq(t, h, "/api/agent/heartbeat", macTok, map[string]any{
		"node": "mac-mini", "agent_version": "m1d",
		"metrics": map[string]any{"cpu_pct": 1.0},
		"caps":    []string{"linux-systemd", "linux-docker"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat 1: %d %s", rec.Code, rec.Body.String())
	}
	resp1 := decodeHeartbeatResp(t, rec.Body.Bytes())
	if len(resp1.Commands) != 0 {
		t.Fatalf("no commands submitted yet, got %d", len(resp1.Commands))
	}
	node, _ := st.GetNodeByName(ctx, "mac-mini")
	if node.Caps != `["linux-systemd","linux-docker"]` {
		t.Fatalf("caps persisted = %s", node.Caps)
	}

	// 提交（cmdsvc 全闸通过：l2/在线/caps）。
	res, err := cmdsvc.Submit(ctx, st, &config.Console{L2AllowedNodes: []string{"mac-mini"}},
		&cmdsvc.SubmitRequest{NodeName: "mac-mini", Kind: "ps_snapshot",
			CreatedBy: "lunge", Scope: "operator"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 心跳 2：领取（响应携带命令；claimed_at 回显）。
	rec = doReq(t, h, "/api/agent/heartbeat", macTok, map[string]any{
		"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0},
		"caps": []string{"linux-systemd", "linux-docker"},
	})
	resp2 := decodeHeartbeatResp(t, rec.Body.Bytes())
	if len(resp2.Commands) != 1 || resp2.Commands[0].CommandID != res.Command.CommandID {
		t.Fatalf("claim: %+v", resp2.Commands)
	}
	claimedAt := resp2.Commands[0].ClaimedAt
	if claimedAt == 0 {
		t.Fatal("claimed_at must be set")
	}
	if resp2.Commands[0].LeaseEpoch != 1 {
		t.Fatalf("first claim lease_epoch = %d, want 1（R41-B1 代次令牌）", resp2.Commands[0].LeaseEpoch)
	}
	leaseEpoch := resp2.Commands[0].LeaseEpoch
	row, _ := st.GetCommand(ctx, res.Command.CommandID)
	if row.Status != store.CommandClaimed || row.ClaimedAt.Int64 != claimedAt {
		t.Fatalf("claimed row: %+v", row)
	}
	// 领取原子性：响应写出前已置 claimed（上一断言已证）。

	// 心跳 3：running 报告 + 回执（lease_epoch 回显）。
	rec = doReq(t, h, "/api/agent/heartbeat", macTok, map[string]any{
		"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0},
		"caps":            []string{"linux-systemd", "linux-docker"},
		"command_running": []string{res.Command.CommandID},
		"command_results": []map[string]any{{
			"command_id": res.Command.CommandID, "claimed_at": claimedAt,
			"lease_epoch": leaseEpoch,
			"exit_code":   0, "result_text": "ps output here",
		}},
		"command_unacked": []string{res.Command.CommandID},
	})
	resp3 := decodeHeartbeatResp(t, rec.Body.Bytes())
	if len(resp3.AckIDs) != 1 || resp3.AckIDs[0].CommandID != res.Command.CommandID {
		t.Fatalf("ack_ids: %+v", resp3.AckIDs)
	}
	if resp3.AckIDs[0].LeaseEpoch != leaseEpoch {
		t.Fatalf("ack entry must carry the recorded epoch: %+v", resp3.AckIDs[0])
	}
	row, _ = st.GetCommand(ctx, res.Command.CommandID)
	if row.Status != store.CommandSucceeded || row.ResultText != "ps output here" {
		t.Fatalf("terminal row: %+v", row)
	}

	// 重复回执（ACK 未达 agent 的重发窗口）→ 幂等命中，不覆盖结果。
	rec = doReq(t, h, "/api/agent/heartbeat", macTok, map[string]any{
		"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0},
		"command_results": []map[string]any{{
			"command_id": res.Command.CommandID, "claimed_at": claimedAt,
			"lease_epoch": leaseEpoch,
			"exit_code":   0, "result_text": "DUPLICATED",
		}},
		"command_unacked": []string{res.Command.CommandID},
	})
	resp4 := decodeHeartbeatResp(t, rec.Body.Bytes())
	if len(resp4.AckIDs) != 1 {
		t.Fatalf("duplicate receipt must re-ack: %+v", resp4.AckIDs)
	}
	row, _ = st.GetCommand(ctx, res.Command.CommandID)
	if row.ResultText != "ps output here" {
		t.Fatalf("duplicate receipt must not overwrite: %q", row.ResultText)
	}
}

// TestHeartbeatReceiptAttribution 结果归属验证：跨节点回执 drop、租约不匹配
// drop、未知命令 drop——agent 收 drop_ids 后可安全删除本地副本。
func TestHeartbeatReceiptAttribution(t *testing.T) {
	// 两个节点需要两个一次性注册 token（newTestHandler 只带一个）。
	h, st := newTestHandlerWithTokens(t,
		config.RegistrationToken{Token: regToken},
		config.RegistrationToken{Token: "second-token-0123456789ab"})
	ctx := t.Context()
	macTok := seedCmdNode(t, h, st, "mac-mini")
	// 第二个节点用第二个注册 token 注册（mustRegister 固定用首个 token）。
	rec2 := doReq(t, h, "/api/agent/register", "second-token-0123456789ab", registerBody("cloud-nanjing"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("register cloud: %d", rec2.Code)
	}
	if err := st.SyncL2Allowed(ctx, []string{"mac-mini", "cloud-nanjing"}); err != nil {
		t.Fatal(err)
	}
	cloudTok := "second-node-token" // 注册响应用不到：cloud 回执走 drop 路径，token 需有效
	// 从注册响应取真实 node token。
	var regResp struct {
		NodeToken string `json:"node_token"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &regResp)
	cloudTok = regResp.NodeToken

	// mac 先心跳上报 caps（能力协商前提），随后提交并领取。
	rec := doReq(t, h, "/api/agent/heartbeat", macTok, map[string]any{
		"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0},
		"caps": []string{"linux-systemd"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("mac heartbeat: %d", rec.Code)
	}
	res, err := cmdsvc.Submit(ctx, st, &config.Console{L2AllowedNodes: []string{"mac-mini"}},
		&cmdsvc.SubmitRequest{NodeName: "mac-mini", Kind: "ps_snapshot", Scope: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, "/api/agent/heartbeat", macTok, map[string]any{
		"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0},
		"caps": []string{"linux-systemd"},
	})
	claimedCmd := decodeHeartbeatResp(t, rec.Body.Bytes()).Commands[0]
	claimedAt, leaseEpoch := claimedCmd.ClaimedAt, claimedCmd.LeaseEpoch

	// cloud 节点拿 mac 的命令回执 → 无归属，drop。
	rec = doReq(t, h, "/api/agent/heartbeat", cloudTok, map[string]any{
		"node": "cloud-nanjing", "metrics": map[string]any{"cpu_pct": 1.0},
		"command_results": []map[string]any{{
			"command_id": res.Command.CommandID, "claimed_at": claimedAt,
			"lease_epoch": leaseEpoch,
			"exit_code":   0, "result_text": "forged",
		}},
	})
	resp := decodeHeartbeatResp(t, rec.Body.Bytes())
	if len(resp.DropIDs) != 1 || resp.DropIDs[0].CommandID != res.Command.CommandID {
		t.Fatalf("cross-node receipt must drop: %+v", resp.DropIDs)
	}
	// 旧代次迟到回执（代次 0/不持有）→ drop（stale epoch）且计入 degraded 审计
	// 计数器（R41-B1：忽略并计数）。
	rec = doReq(t, h, "/api/agent/heartbeat", macTok, map[string]any{
		"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0},
		"command_results": []map[string]any{{
			"command_id": res.Command.CommandID, "claimed_at": claimedAt - 999,
			"lease_epoch": 0,
			"exit_code":   0, "result_text": "stale instance",
		}},
	})
	resp = decodeHeartbeatResp(t, rec.Body.Bytes())
	if len(resp.DropIDs) != 1 {
		t.Fatalf("stale epoch receipt must drop: %+v", resp.DropIDs)
	}
	// drop 条目必须携带被拒回执自陈的代次（agent 定点删该代次本地副本的依据；
	// 缺了它 agent 找不到文件、旧回执会被无限重发）。
	if resp.DropIDs[0].LeaseEpoch != 0 {
		t.Fatalf("drop entry must echo the rejected receipt's epoch: %+v", resp.DropIDs[0])
	}
	if got := h.staleReceipts.Load(); got != 1 {
		t.Fatalf("stale receipt counter = %d, want 1", got)
	}
	// 正确归属回执（当前代次回显）照常收。
	rec = doReq(t, h, "/api/agent/heartbeat", macTok, map[string]any{
		"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0},
		"command_results": []map[string]any{{
			"command_id": res.Command.CommandID, "claimed_at": claimedAt,
			"lease_epoch": leaseEpoch,
			"exit_code":   0, "result_text": "genuine",
		}},
	})
	resp = decodeHeartbeatResp(t, rec.Body.Bytes())
	acked := map[string]bool{}
	for _, e := range resp.AckIDs {
		acked[e.CommandID] = true
	}
	if !acked[res.Command.CommandID] {
		t.Fatalf("genuine receipt must ack: %+v", resp.AckIDs)
	}
}

// TestClaimL2GateOnHeartbeat 领取路径 l2 复核②：云节点心跳不返回任何命令，
// 即便有 pending（提交侧复核①由 cmdsvc 测试覆盖）。
func TestClaimL2GateOnHeartbeat(t *testing.T) {
	h, st := newTestHandler(t)
	ctx := t.Context()
	cloudTok := seedCmdNode(t, h, st, "cloud-nanjing")
	// 关闸（模拟 config 只允许 mac-mini）。
	if err := st.SyncL2Allowed(ctx, []string{"mac-mini"}); err != nil {
		t.Fatal(err)
	}
	// 直接落一条 pending（绕过提交闸的最坏情形）。
	if _, _, err := st.SubmitCommand(ctx, &store.CommandRow{
		CommandID: "cmd-cloud-direct", NodeID: 1, Kind: "ps_snapshot", ArgsJSON: "{}",
		Status: store.CommandPending, TimeoutS: 30, CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, h, "/api/agent/heartbeat", cloudTok, map[string]any{
		"node": "cloud-nanjing", "metrics": map[string]any{"cpu_pct": 1.0},
	})
	resp := decodeHeartbeatResp(t, rec.Body.Bytes())
	if len(resp.Commands) != 0 {
		t.Fatalf("cloud must not receive commands: %+v", resp.Commands)
	}
}

// TestHeartbeatProtocolViolations 协议违规：caps 畸形、回执 id 畸形、显式 null
// ——整条 400 不落库（与 metrics 值域同口径）。
func TestHeartbeatProtocolViolations(t *testing.T) {
	h, st := newTestHandler(t)
	macTok := seedCmdNode(t, h, st, "mac-mini")
	bodies := []map[string]any{
		{"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0}, "caps": []string{"bad cap with space"}},
		{"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0}, "command_results": []map[string]any{{"command_id": "../evil", "claimed_at": 1}}},
		{"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0}, "command_results": []map[string]any{{"command_id": "cmd-x", "claimed_at": 1, "lease_epoch": -3}}},
		{"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0}, "command_results": nil},
		{"node": "mac-mini", "metrics": map[string]any{"cpu_pct": 1.0}, "command_running": []string{"NOT-VALID-ID"}},
	}
	for i, b := range bodies {
		rec := doReq(t, h, "/api/agent/heartbeat", macTok, b)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("case %d: code = %d, want 400 (%s)", i, rec.Code, rec.Body.String())
		}
	}
}
