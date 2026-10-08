package registry

// M1b-c 心跳协议单测（SPEC-M1b-c §2.1/§2.2）：agent_tasks 三态语义沿用
// （缺席 = 无变化 / 空数组 = 清空 / 显式 null = 400）、任务与活跃度值域校验、
// agents.last_activity 透传入库。

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"
)

type beatBody struct {
	Node          string            `json:"node"`
	AgentVersion  string            `json:"agent_version"`
	Metrics       map[string]any    `json:"metrics"`
	CollectErrors map[string]string `json:"collect_errors,omitempty"`
	// 以下字段用 json.RawMessage 表达「缺席 / [] / null / 数组」四态。
	Services   *json.RawMessage `json:"services,omitempty"`
	Agents     *json.RawMessage `json:"agents,omitempty"`
	AgentTasks *json.RawMessage `json:"agent_tasks,omitempty"`
	// R27-#4：任务清单截断标记（真值随显式 agent_tasks 数组一同上报）。
	AgentTasksTruncated *bool `json:"agent_tasks_truncated,omitempty"`
}

func raw(s string) *json.RawMessage {
	r := json.RawMessage(s)
	return &r
}

func taskJSON(pid int, name string) string {
	return `{"pid":` + itoa(pid) + `,"agent_name":"` + name + `","cmd":"` + name + ` run","elapsed_s":42,"cpu_pct":1.5,"mem_pct":0.5,"started_at":` + itoa(int(time.Now().Unix()-42)) + `}`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func metricsOK() map[string]any {
	return map[string]any{"cpu_pct": 1.0, "mem_used": 1, "mem_total": 4, "uptime_s": 5}
}

// beat 发一条 metrics 合法的心跳，返回响应码。
func beat(t *testing.T, h *Handler, tok string, body beatBody) int {
	t.Helper()
	body.Node = "n1"
	body.AgentVersion = "v"
	if body.Metrics == nil {
		body.Metrics = metricsOK()
	}
	rec := doReq(t, h, "/api/agent/heartbeat", tok, body)
	return rec.Code
}

// TestHeartbeatAgentTasksThreeStates R11-A 三态沿用：缺席不覆盖、[] 清空、
// 数组全量替换、显式 null 400 整条拒绝。
func TestHeartbeatAgentTasksThreeStates(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	// ① 显式数组 → 落库。
	if code := beat(t, h, tok, beatBody{AgentTasks: raw("[" + taskJSON(101, "zcode") + "," + taskJSON(102, "codex") + "]")}); code != http.StatusOK {
		t.Fatalf("beat1 = %d", code)
	}
	rows, err := st.ListAgentTasks(t.Context(), nodeID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].AgentName != "codex" || rows[0].PID != 102 { // agent_name, pid 排序
		t.Fatalf("rows[0] = %+v", rows[0])
	}

	// ② 缺席 → 无变化。
	if code := beat(t, h, tok, beatBody{}); code != http.StatusOK {
		t.Fatalf("beat2 = %d", code)
	}
	rows, _ = st.ListAgentTasks(t.Context(), nodeID)
	if len(rows) != 2 {
		t.Fatalf("absent field must not touch tasks, got %d", len(rows))
	}

	// ③ 空数组 → 清空该节点任务。
	if code := beat(t, h, tok, beatBody{AgentTasks: raw("[]")}); code != http.StatusOK {
		t.Fatalf("beat3 = %d", code)
	}
	rows, _ = st.ListAgentTasks(t.Context(), nodeID)
	if len(rows) != 0 {
		t.Fatalf("empty array must clear tasks, got %d", len(rows))
	}

	// ④ 显式 null → 协议违规 400，整条心跳不落（上一拍状态保持）。
	if code := beat(t, h, tok, beatBody{AgentTasks: raw("null")}); code != http.StatusBadRequest {
		t.Fatalf("explicit null = %d, want 400", code)
	}
	// 400 不落 metrics：库内该节点最后一次心跳仍是 beat3 的 ts —— 用 count 校验。
	n, err := st.CountMetrics(t.Context(), nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("metrics rows = %d, want 3（400 整条拒绝不落库）", n)
	}
}

// TestHeartbeatAgentTasksValidation 值域校验：非法条目整条 400、不落任何行。
// cpu_pct 只拒 NaN/Inf/负数（多核 %CPU 超 100 是 ps 语义，R27-#1）；mem_pct
// 仍受 [0,100] 封顶。
func TestHeartbeatAgentTasksValidation(t *testing.T) {
	h, _ := newTestHandler(t)
	tok, _ := mustRegister(t, h, "n1")
	bad := map[string]string{
		"empty name":   `[{"pid":1,"agent_name":"","cmd":"x"}]`,
		"bad pid":      `[{"pid":0,"agent_name":"zcode"}]`,
		"dup pid":      `[{"pid":5,"agent_name":"zcode"},{"pid":5,"agent_name":"codex"}]`,
		"neg elapsed":  `[{"pid":1,"agent_name":"zcode","elapsed_s":-1}]`,
		"neg cpu":      `[{"pid":1,"agent_name":"zcode","cpu_pct":-0.5}]`,
		"mem range":    `[{"pid":1,"agent_name":"zcode","mem_pct":100.5}]`,
		"neg mem":      `[{"pid":1,"agent_name":"zcode","mem_pct":-0.1}]`,
		"future start": `[{"pid":1,"agent_name":"zcode","started_at":` + itoa(int(time.Now().Add(48*time.Hour).Unix())) + `}]`,
	}
	for name, body := range bad {
		if code := beat(t, h, tok, beatBody{AgentTasks: raw(body)}); code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400", name, code)
		}
	}

	// 超上限（>64 条）→ 400。
	s := "["
	for i := 0; i <= 64; i++ {
		if i > 0 {
			s += ","
		}
		s += `{"pid":` + itoa(i+1) + `,"agent_name":"zcode"}`
	}
	s += "]"
	if code := beat(t, h, tok, beatBody{AgentTasks: raw(s)}); code != http.StatusBadRequest {
		t.Fatalf("too many tasks: code = %d, want 400", code)
	}
}

// TestHeartbeatAgentTasksCPUAbove100 多核 %CPU 超 100 放行（R27-#1）：8 核打满
// ps 报 750，原值通过并落库，不丢整条心跳；mem_pct 同拍照常校验。
func TestHeartbeatAgentTasksCPUAbove100(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	if code := beat(t, h, tok, beatBody{AgentTasks: raw(
		`[{"pid":700,"agent_name":"codex","cmd":"codex run","elapsed_s":60,"cpu_pct":750.0,"mem_pct":42.0}]`)}); code != http.StatusOK {
		t.Fatalf("cpu_pct=750 → %d, want 200（多核 %%CPU 超 100 是 ps 语义）", code)
	}
	rows, err := st.ListAgentTasks(t.Context(), nodeID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if !rows[0].CPUPct.Valid || rows[0].CPUPct.Float64 != 750.0 {
		t.Fatalf("cpu_pct stored = %v, want 750", rows[0].CPUPct)
	}
	if !rows[0].MemPct.Valid || rows[0].MemPct.Float64 != 42.0 {
		t.Fatalf("mem_pct stored = %v, want 42", rows[0].MemPct)
	}
}

// TestHeartbeatAgentTasksTruncatedFlag 截断标志链路（R27-#4）：显式数组 +
// agent_tasks_truncated:true → nodes.tasks_truncated 落库；显式数组不带字段 →
// 复位 false；数组缺席 → 不改动（面板按节点名标注「清单不完整」）。
func TestHeartbeatAgentTasksTruncatedFlag(t *testing.T) {
	h, st := newTestHandler(t)
	tok, _ := mustRegister(t, h, "n1")
	flag := func(t *testing.T) bool {
		t.Helper()
		n, err := st.GetNodeByName(t.Context(), "n1")
		if err != nil || n == nil {
			t.Fatalf("node lookup: %v", err)
		}
		return n.TasksTruncated
	}
	yes := true

	// ① 截断快照 → true。
	if code := beat(t, h, tok, beatBody{AgentTasks: raw(`[{"pid":1,"agent_name":"zcode"}]`), AgentTasksTruncated: &yes}); code != http.StatusOK {
		t.Fatalf("beat1 = %d", code)
	}
	if !flag(t) {
		t.Fatal("truncated beat must set nodes.tasks_truncated")
	}

	// ② 数组缺席 → 不改动（旧快照仍 displayed，其标记仍然成立）。
	if code := beat(t, h, tok, beatBody{}); code != http.StatusOK {
		t.Fatalf("beat2 = %d", code)
	}
	if !flag(t) {
		t.Fatal("absent array must not touch flag")
	}

	// ③ 未截断的显式快照（字段缺席 = false）→ 复位。
	if code := beat(t, h, tok, beatBody{AgentTasks: raw(`[{"pid":2,"agent_name":"codex"}]`)}); code != http.StatusOK {
		t.Fatalf("beat3 = %d", code)
	}
	if flag(t) {
		t.Fatal("untruncated snapshot must reset flag")
	}
}

// TestValidateAgentTasksNonFinite NaN/Inf/负数拒绝（防御性直测）：JSON 解码层
// 本就不会放行 NaN/Inf 字面量，校验函数仍须自成一道防线；cpu=750 合法、
// mem=100 边界合法、mem>100 拒绝。
func TestValidateAgentTasksNonFinite(t *testing.T) {
	inf := math.Inf(1)
	ninf := math.Inf(-1)
	nan := math.NaN()
	neg := -1.5
	over := 100.5
	in := 100.0
	for name, tasks := range map[string][]agentTaskIn{
		"cpu +inf": {{PID: 1, AgentName: "zcode", CPUPct: &inf}},
		"cpu -inf": {{PID: 1, AgentName: "zcode", CPUPct: &ninf}},
		"cpu nan":  {{PID: 1, AgentName: "zcode", CPUPct: &nan}},
		"cpu neg":  {{PID: 1, AgentName: "zcode", CPUPct: &neg}},
		"mem +inf": {{PID: 1, AgentName: "zcode", MemPct: &inf}},
		"mem nan":  {{PID: 1, AgentName: "zcode", MemPct: &nan}},
		"mem neg":  {{PID: 1, AgentName: "zcode", MemPct: &neg}},
		"mem >100": {{PID: 1, AgentName: "zcode", MemPct: &over}},
	} {
		if _, err := validateAgentTasks(tasks); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
	cpu := 750.0
	rows, err := validateAgentTasks([]agentTaskIn{{PID: 1, AgentName: "zcode", CPUPct: &cpu, MemPct: &in}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("cpu=750/mem=100 must pass, err=%v", err)
	}
}

// TestHeartbeatAgentsLastActivity agents 条目的会话活跃度透传入库 + 非法值拒绝。
func TestHeartbeatAgentsLastActivity(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	now := time.Now().Unix()
	agents := `[{"name":"zcode","type":"cli","status":"active","last_activity":` + itoa(int(now-60)) + `,"session_files":412}]`
	if code := beat(t, h, tok, beatBody{Agents: raw(agents)}); code != http.StatusOK {
		t.Fatalf("beat = %d", code)
	}
	recs, err := st.ListAgents(t.Context(), nodeID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("agents=%v err=%v", recs, err)
	}
	if recs[0].LastActivity == nil || *recs[0].LastActivity != now-60 {
		t.Fatalf("last_activity = %v", recs[0].LastActivity)
	}
	if recs[0].SessionFiles == nil || *recs[0].SessionFiles != 412 {
		t.Fatalf("session_files = %v", recs[0].SessionFiles)
	}
	if recs[0].Invokable {
		t.Fatal("invokable must stay false")
	}

	// 非法：负值 / 超前时间戳。
	for _, badAgents := range []string{
		`[{"name":"zcode","type":"cli","status":"active","last_activity":-1}]`,
		`[{"name":"zcode","type":"cli","status":"active","session_files":-5}]`,
		`[{"name":"zcode","type":"cli","status":"active","last_activity":` + itoa(int(now+48*3600)) + `}]`,
	} {
		if code := beat(t, h, tok, beatBody{Agents: raw(badAgents)}); code != http.StatusBadRequest {
			t.Fatalf("bad agents %q → %d, want 400", badAgents, code)
		}
	}
}

// TestHeartbeatTaskCmdSanitized cmd 超长截断入库（≤200 字节）+ 控制字符清除。
func TestHeartbeatTaskCmdSanitized(t *testing.T) {
	h, st := newTestHandler(t)
	tok, nodeID := mustRegister(t, h, "n1")

	long := "zcode " + string(rune(7)) + "run " + repeat("界", 300)
	body := `[{"pid":9,"agent_name":"zcode","cmd":` + jsonStr(long) + `}]`
	if code := beat(t, h, tok, beatBody{AgentTasks: raw(body)}); code != http.StatusOK {
		t.Fatalf("beat = %d", code)
	}
	rows, err := st.ListAgentTasks(t.Context(), nodeID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if len(rows[0].Cmd) > 200 {
		t.Fatalf("cmd len = %d, want ≤200", len(rows[0].Cmd))
	}
	for _, r := range rows[0].Cmd {
		if r < 0x20 && r != '\t' {
			t.Fatalf("control char survived sanitize: %q", rows[0].Cmd)
		}
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
