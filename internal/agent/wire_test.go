package agent

import (
	"encoding/json"
	"testing"
)

// TestHeartbeatRespAgentWireCompat agent 侧线上契约（R41-B1）：心跳响应按协议
// 文档形态（commands/ack_ids/drop_ids 及条目内 command_id/lease_epoch）必须被
// agent 解析结构完整还原——解析结构的 JSON tag 漂移会让代次令牌静默丢失（agent
// 端 epoch 归零 → 回执全被 stale epoch 拒收 / drop 条目找不到清理目标、旧回执
// 被无限重发）。console 侧发射形态由 registry 包测试与二进制级 E2E 覆盖。
func TestHeartbeatRespAgentWireCompat(t *testing.T) {
	wire := `{"status":"ok","server_time":123,
		"commands":[{"command_id":"cmd-1","kind":"ps_snapshot","args_json":"{}",
		            "timeout_s":30,"claimed_at":42,"lease_epoch":7}],
		"ack_ids":[{"command_id":"cmd-2","lease_epoch":3}],
		"drop_ids":[{"command_id":"cmd-3","lease_epoch":5}]}`
	hb := heartbeatResponse{}
	if err := json.Unmarshal([]byte(wire), &hb); err != nil {
		t.Fatal(err)
	}
	if len(hb.Commands) != 1 || hb.Commands[0].CommandID != "cmd-1" ||
		hb.Commands[0].ClaimedAt != 42 || hb.Commands[0].LeaseEpoch != 7 {
		t.Fatalf("delivery roundtrip: %+v", hb.Commands)
	}
	if len(hb.AckIDs) != 1 || hb.AckIDs[0].CommandID != "cmd-2" || hb.AckIDs[0].LeaseEpoch != 3 {
		t.Fatalf("ack roundtrip: %+v", hb.AckIDs)
	}
	if len(hb.DropIDs) != 1 || hb.DropIDs[0].CommandID != "cmd-3" || hb.DropIDs[0].LeaseEpoch != 5 {
		t.Fatalf("drop roundtrip: %+v", hb.DropIDs)
	}
}
