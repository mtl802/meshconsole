package registry

// M1d 修复轮心跳协议单测（SPEC-M1d §3.5）：agent_sessions 三态语义（同
// services/agent_tasks R11-A 口径）、按节点全量替换、agent_tasks.background
// daemon 过滤标记透传入库。

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mtl802/meshconsole/internal/config"
)

// sessJSON 构造一条合法会话上报（last_activity 恒新鲜）。
func sessJSON(file, agent, topic string) string {
	return `{"agent_name":"` + agent + `","session_file":"` + file + `","started_at":` +
		itoa(int(time.Now().Unix()-300)) + `,"last_activity":` + itoa(int(time.Now().Unix())) +
		`,"topic":"` + topic + `","recent_action":"正在跑 log show"}`
}

// withSessions 给 beatBody 挂 agent_sessions 字段。
func withSessions(b beatBody, raw *json.RawMessage) beatBody {
	b.AgentSessions = raw
	return b
}

// TestHeartbeatAgentSessionsThreeStates 三态（R11-A 口径）：显式数组落库 →
// 缺席无变化 → 空数组清空（全部安静）→ 显式 null 400 且整条心跳不落。
func TestHeartbeatAgentSessionsThreeStates(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	// ① 显式数组 → 落库。
	body := withSessions(beatBody{}, raw("["+sessJSON("/home/m/rollout/a.jsonl", "zcode", "排查 headscale")+","+sessJSON("/home/m/codex/b.jsonl", "codex", "跑测试")+"]"))
	if code := beat(t, h, tok, body); code != http.StatusOK {
		t.Fatalf("beat1 = %d", code)
	}
	rows, err := st.ListAgentSessions(t.Context(), nodeID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].AgentName != "codex" || rows[0].Topic != "跑测试" {
		t.Fatalf("rows[0] = %+v", rows[0])
	}
	if rows[1].Topic != "排查 headscale" || rows[1].RecentAction != "正在跑 log show" {
		t.Fatalf("rows[1] = %+v", rows[1])
	}

	// ② 缺席 → 无变化。
	if code := beat(t, h, tok, beatBody{}); code != http.StatusOK {
		t.Fatalf("beat2 = %d", code)
	}
	rows, _ = st.ListAgentSessions(t.Context(), nodeID)
	if len(rows) != 2 {
		t.Fatalf("absent field must not touch sessions, got %d", len(rows))
	}

	// ③ 空数组 → 清空（全部安静）。
	if code := beat(t, h, tok, withSessions(beatBody{}, raw("[]"))); code != http.StatusOK {
		t.Fatalf("beat3 = %d", code)
	}
	rows, _ = st.ListAgentSessions(t.Context(), nodeID)
	if len(rows) != 0 {
		t.Fatalf("empty array must clear sessions, got %d", len(rows))
	}

	// ④ 显式 null → 协议违规 400，整条心跳不落。
	if code := beat(t, h, tok, withSessions(beatBody{}, raw("null"))); code != http.StatusBadRequest {
		t.Fatalf("explicit null = %d, want 400", code)
	}
	n, err := st.CountMetrics(t.Context(), nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("metrics rows = %d, want 3（400 整条拒绝不落库）", n)
	}
}

// TestHeartbeatAgentSessionsFullReplace 按节点全量替换：第二拍清单恰为第二集合
// （旧会话文件静默 = 会话结束，不留历史行）；跨节点互不干扰。
func TestHeartbeatAgentSessionsFullReplace(t *testing.T) {
	// 注册 token 一次性：n2 用独立绑定 token（R1-#3 消耗语义）。
	h, st := newTestHandlerWithTokens(t,
		config.RegistrationToken{Token: regToken},
		config.RegistrationToken{Token: regToken + "-n2", ExpectedNode: "n2"})
	tok1, id1 := mustRegister(t, h, "n1")
	tok2, id2 := mustRegisterWithToken(t, h, "n2", regToken+"-n2")

	body1 := withSessions(beatBody{}, raw("["+sessJSON("/m/a.jsonl", "zcode", "t1")+"]"))
	body1.Node = "n1"
	if code := beat(t, h, tok1, body1); code != http.StatusOK {
		t.Fatalf("beat n1 = %d", code)
	}
	body2 := withSessions(beatBody{}, raw("["+sessJSON("/m/x.jsonl", "codex", "t2")+","+sessJSON("/m/y.jsonl", "claude", "t3")+"]"))
	body2.Node = "n2"
	if code := beat(t, h, tok2, body2); code != http.StatusOK {
		t.Fatalf("beat n2 = %d", code)
	}
	// n1 第二拍换成不同集合。
	body1b := withSessions(beatBody{}, raw("["+sessJSON("/m/z.jsonl", "gemini", "t4")+"]"))
	body1b.Node = "n1"
	if code := beat(t, h, tok1, body1b); code != http.StatusOK {
		t.Fatalf("beat n1-2 = %d", code)
	}
	rows1, err := st.ListAgentSessions(t.Context(), id1)
	if err != nil || len(rows1) != 1 || rows1[0].SessionFile != "/m/z.jsonl" {
		t.Fatalf("n1 rows = %+v err=%v（恰为第二集合）", rows1, err)
	}
	rows2, err := st.ListAgentSessions(t.Context(), id2)
	if err != nil || len(rows2) != 2 {
		t.Fatalf("n2 rows = %+v err=%v", rows2, err)
	}
	// 全网视图恰为 3 条（全量替换不留旧行）。
	all, err := st.ListAllAgentSessions(t.Context())
	if err != nil || len(all) != 3 {
		t.Fatalf("all rows = %+v err=%v", all, err)
	}
}

// TestHeartbeatAgentSessionsValidation 值域校验：缺 agent_name/session_file、
// 缺 last_activity、时间越界、同文件重复 → 整条 400 不落。
func TestHeartbeatAgentSessionsValidation(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")
	future := itoa(int(time.Now().Unix()) + 2*86400)
	bad := map[string]string{
		"empty name":     `[{"agent_name":"","session_file":"/m/a.jsonl","last_activity":1}]`,
		"empty file":     `[{"agent_name":"zcode","session_file":"","last_activity":1}]`,
		"dup file":       `[{"agent_name":"zcode","session_file":"/m/a.jsonl","last_activity":1},{"agent_name":"codex","session_file":"/m/a.jsonl","last_activity":1}]`,
		"missing last":   `[{"agent_name":"zcode","session_file":"/m/a.jsonl"}]`,
		"future last":    `[{"agent_name":"zcode","session_file":"/m/a.jsonl","last_activity":` + future + `}]`,
		"neg started":    `[{"agent_name":"zcode","session_file":"/m/a.jsonl","last_activity":1,"started_at":-5}]`,
		"future started": `[{"agent_name":"zcode","session_file":"/m/a.jsonl","last_activity":1,"started_at":` + future + `}]`,
	}
	for name, v := range bad {
		if code := beat(t, h, tok, withSessions(beatBody{}, raw(v))); code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400", name, code)
		}
	}
	// >64 条截断拒绝。
	many := "["
	for i := 0; i < 65; i++ {
		if i > 0 {
			many += ","
		}
		many += sessJSON("/m/f"+itoa(i)+".jsonl", "zcode", "t")
	}
	many += "]"
	if code := beat(t, h, tok, withSessions(beatBody{}, raw(many))); code != http.StatusBadRequest {
		t.Fatalf("too many: code = %d, want 400", code)
	}
	if rows, _ := st.ListAgentSessions(t.Context(), nodeID); len(rows) != 0 {
		t.Fatalf("validation failures must not store rows: %+v", rows)
	}
}

// TestHeartbeatTaskBackgroundFlag daemon 过滤标记（SPEC-M1d §3.5）：background
// = true 随 agent_tasks 入库；缺省 false。
func TestHeartbeatTaskBackgroundFlag(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")
	body := beatBody{AgentTasks: raw(`[
		{"pid":11,"agent_name":"codex","cmd":"codex app-server","elapsed_s":90001,"background":true},
		{"pid":12,"agent_name":"zcode","cmd":"zcode run","elapsed_s":42}
	]`)}
	if code := beat(t, h, tok, body); code != http.StatusOK {
		t.Fatalf("beat = %d", code)
	}
	rows, err := st.ListAgentTasks(t.Context(), nodeID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if !rows[0].Background || rows[0].PID != 11 {
		t.Fatalf("daemon task must be background: %+v", rows[0])
	}
	if rows[1].Background {
		t.Fatalf("short task must not be background: %+v", rows[1])
	}
}
