package cmdsvc

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

// ---- M1d（SPEC-M1d §1-§5）提交编排测试：提交侧安全闸全链 ----

func openSvc(t *testing.T) (*store.Store, *config.Console, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "svc.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if _, err := st.RegisterNode(ctx, "mac-mini", "server", "darwin", "arm64", "h1", "r1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode(ctx, "cloud-nanjing", "cloud", "linux", "amd64", "h2", "r2", 0); err != nil {
		t.Fatal(err)
	}
	// 两节点都打开 l2（提交侧复核①），cloud 的拒绝由 TestSubmitGates 在关闸后验证。
	if err := st.SyncL2Allowed(ctx, []string{"mac-mini", "cloud-nanjing"}); err != nil {
		t.Fatal(err)
	}
	// mac-mini 心跳：caps=mac-launchd + 受管服务 headscale（unit 集合校验依据）。
	mac := `["mac-launchd"]`
	if ok, err := st.HeartbeatFull(ctx, &store.MetricsRow{NodeID: 1, TS: 1, CPUPct: fptr(1)},
		"v", &[]store.ServiceRow{{Name: "headscale", Type: "systemd", Target: "headscale", Status: "active"}},
		nil, nil, false, nil, &mac); err != nil || !ok {
		t.Fatalf("seed mac: %v", err)
	}
	// cloud-nanjing 心跳：caps=linux-systemd/linux-docker。
	dockerCap := 0
	cloud := `["linux-systemd","linux-docker"]`
	if ok, err := st.HeartbeatFull(ctx, &store.MetricsRow{NodeID: 2, TS: 1, CPUPct: fptr(1)},
		"v", &[]store.ServiceRow{{Name: "headscale", Type: "systemd", Target: "headscale", Status: "active"}},
		nil, nil, false, nil, &cloud); err != nil || !ok {
		t.Fatalf("seed cloud: %v", err)
	}
	_ = dockerCap // cloud 的 docker 能力仅声明语义，测试不依赖
	return st, &config.Console{}, dbPath
}

func fptr(v float64) *float64 { return &v }

func submit(t *testing.T, st *store.Store, cfg *config.Console, req *SubmitRequest) (*SubmitResult, error) {
	t.Helper()
	return Submit(context.Background(), st, cfg, req)
}

func TestSubmitHappyPath(t *testing.T) {
	st, cfg, _ := openSvc(t)
	// mac-mini 为 darwin（mac-launchd）→ ps_snapshot 可下发；受管 unit 命中。
	res, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "mac-mini", Kind: "ps_snapshot", CreatedBy: "lunge", Scope: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Command.Status != "pending" || res.Command.NodeName != "mac-mini" {
		t.Fatalf("res: %+v", res.Command)
	}
	if res.Command.CreatedBy != "lunge" || res.Command.Scope != "operator" {
		t.Fatalf("audit attribution missing: %+v", res.Command)
	}
	// systemd 系 kind 在 mac 节点上 caps 不匹配（mac-launchd 不满足 linux-systemd）。
	if _, err := submit(t, st, cfg, &SubmitRequest{NodeName: "mac-mini", Kind: "systemctl_status"}); !errors.Is(err, ErrCapsUnsupported) {
		t.Fatalf("caps mismatch: err=%v, want ErrCapsUnsupported", err)
	}
}

func TestSubmitManagedUnitAndExtras(t *testing.T) {
	st, cfg, _ := openSvc(t)
	// 受管 unit 命中（cloud-nanjing 为 linux-systemd）。
	res, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "cloud-nanjing", Kind: "journalctl_tail",
		ArgsJSON: `{"n":100,"unit":"headscale"}`, CreatedBy: "lunge", Scope: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 规范化产物键序稳定。
	if res.Command.ArgsJSON != `{"n":"100","unit":"headscale"}` {
		t.Fatalf("args canonical = %s", res.Command.ArgsJSON)
	}
	// 非受管 unit → ErrBadArgs。
	if _, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "cloud-nanjing", Kind: "journalctl_tail",
		ArgsJSON: `{"n":100,"unit":"nginx"}`}); !errors.Is(err, ErrBadArgs) {
		t.Fatalf("unmanaged unit: err=%v, want ErrBadArgs", err)
	}
	// l2_extra_commands 声明该参数实例后放行（扩展点；kind 决定二进制不变）。
	// 生产路径经 LoadConsole 加载期规范化（cmdkind.ValidateArgs 产物）；测试
	// 直接构造时须给规范化形态（与提交侧产物精确比对）。
	cfg.L2ExtraCommands = []config.L2ExtraCommand{{
		Kind: "journalctl_tail", ArgsJSON: `{"n":"100","unit":"nginx"}`,
	}}
	res2, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "cloud-nanjing", Kind: "journalctl_tail",
		ArgsJSON: `{"unit":"nginx","n":100}`})
	if err != nil {
		t.Fatalf("extra instance must pass: %v", err)
	}
	// 加载期规范化（键序/字符串化）与提交侧规范化产物精确匹配。
	if res2.Command.ArgsJSON != `{"n":"100","unit":"nginx"}` {
		t.Fatalf("extra args = %s", res2.Command.ArgsJSON)
	}
	// extra 未覆盖的其他非法实例仍拒绝（单横线注入）。
	if _, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "cloud-nanjing", Kind: "journalctl_tail",
		ArgsJSON: `{"unit":"-h","n":100}`}); !errors.Is(err, ErrBadArgs) {
		t.Fatalf("injected unit via extras path: err=%v", err)
	}
}

func TestSubmitGates(t *testing.T) {
	st, cfg, _ := openSvc(t)
	cases := []struct {
		name string
		req  SubmitRequest
		want error
	}{
		{"节点不存在", SubmitRequest{NodeName: "nope", Kind: "ps_snapshot"}, ErrNodeNotFound},
		{"非白名单 kind", SubmitRequest{NodeName: "mac-mini", Kind: "shutdown_now"}, ErrBadKind},
		{"超时越界", SubmitRequest{NodeName: "mac-mini", Kind: "ps_snapshot", TimeoutS: 301}, ErrBadTimeout},
		{"超时为零取默认", SubmitRequest{NodeName: "mac-mini", Kind: "ps_snapshot"}, nil},
		{"参数槽非法", SubmitRequest{NodeName: "mac-mini", Kind: "mac_log_show", ArgsJSON: `{"n":999}`}, ErrBadArgs},
		{"幂等键超长", SubmitRequest{NodeName: "mac-mini", Kind: "ps_snapshot", SubmissionKey: string(make([]byte, 129))}, ErrBadSubmissionKey},
	}
	for _, c := range cases {
		_, err := submit(t, st, cfg, &c.req)
		if c.want == nil && err != nil {
			t.Errorf("%s: unexpected err %v", c.name, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v, want %v", c.name, err, c.want)
		}
	}
	// 离线节点（last_seen 回拨 + 离线巡检判定）。
	ctx := context.Background()
	if err := st.SetNodeLastSeen(ctx, 1, time.Now().Unix()-600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SweepOffline(ctx, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := submit(t, st, cfg, &SubmitRequest{NodeName: "mac-mini", Kind: "ps_snapshot"}); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("offline: err=%v", err)
	}
	mac := `["mac-launchd"]`
	if ok, err := st.HeartbeatFull(ctx, &store.MetricsRow{NodeID: 1, TS: 1, CPUPct: fptr(1)},
		"v", nil, nil, nil, false, nil, &mac); err != nil || !ok {
		t.Fatal(err)
	}
	// l2 白名单关闭（云节点 gate 的提交侧复核①）。
	if err := st.SyncL2Allowed(ctx, []string{"mac-mini"}); err != nil {
		t.Fatal(err)
	}
	_, err := submit(t, st, cfg, &SubmitRequest{NodeName: "cloud-nanjing", Kind: "ps_snapshot"})
	if !errors.Is(err, ErrNodeNotAllowed) {
		t.Fatalf("cloud submit must be rejected by l2 gate: err=%v", err)
	}
}

// TestSubmitIdempotentWindow 幂等窗口全链：同键同参命中原命令；同键异参 409；
// 窗口过期（调用方归档）后复用。
func TestSubmitIdempotentWindow(t *testing.T) {
	st, cfg, _ := openSvc(t)
	ctx := context.Background()
	r1, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "cloud-nanjing", Kind: "docker_ps", SubmissionKey: "k-1",
		CreatedBy: "lunge", Scope: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	dup, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "cloud-nanjing", Kind: "docker_ps", SubmissionKey: "k-1",
		CreatedBy: "lunge", Scope: "operator",
	})
	if err != nil || !dup.Idempotent || dup.Command.CommandID != r1.Command.CommandID {
		t.Fatalf("idempotent: %+v err=%v", dup, err)
	}
	// 同键异参 → 409（窗口内）。
	if _, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "cloud-nanjing", Kind: "ps_snapshot", SubmissionKey: "k-1"}); !errors.Is(err, store.ErrKeyConflict) {
		t.Fatalf("conflict: err=%v", err)
	}
	// 窗口过期：归档后同键异参可新建。
	if _, err := st.ArchiveSubmissionKey(ctx, "k-1", 0); err != nil {
		t.Fatal(err)
	}
	r2, err := submit(t, st, cfg, &SubmitRequest{
		NodeName: "cloud-nanjing", Kind: "ps_snapshot", SubmissionKey: "k-1"})
	if err != nil || r2.Command.CommandID == r1.Command.CommandID {
		t.Fatalf("reuse after archive: %+v err=%v", r2, err)
	}
	// 旧行的提交键已归档、command_id 原样保留（永不复用）。
	old, err := st.GetCommand(ctx, r1.Command.CommandID)
	if err != nil || old == nil || old.ArchivedKey.String != "k-1" || old.SubmittedKey.Valid {
		t.Fatalf("archived old row: %+v err=%v", old, err)
	}
}

// TestSubmitSameKeyBeyondWindowReuse 同键同参超窗 → 归档 → 可重新提交
// （R39-#6）：幂等命中统一过窗口检查——同参超窗不再永久返回旧命令，归档
// （archived_key 只收提交键、command_id 原样）后同键同参得到新命令。
func TestSubmitSameKeyBeyondWindowReuse(t *testing.T) {
	st, cfg, dbPath := openSvc(t)
	ctx := context.Background()
	req := &SubmitRequest{NodeName: "cloud-nanjing", Kind: "docker_ps", SubmissionKey: "k-old",
		CreatedBy: "lunge", Scope: "operator"}
	r1, err := submit(t, st, cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	// 把旧行 created_at 拨回窗口之外（直接改库模拟时间流逝）。
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	window := int64((10 * time.Minute) / time.Second)
	if _, err := db.ExecContext(ctx, `UPDATE commands SET created_at = ? WHERE command_id = ?`,
		time.Now().Unix()-window-60, r1.Command.CommandID); err != nil {
		t.Fatal(err)
	}
	// 同键同参重新提交：超窗命中 → 归档 → 新建（非幂等命中）。
	r2, err := submit(t, st, cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Idempotent || r2.Command.CommandID == r1.Command.CommandID {
		t.Fatalf("beyond-window same-args resubmit: idem=%v id=%s", r2.Idempotent, r2.Command.CommandID)
	}
	// 旧行提交键已归档、command_id 原样保留（永不复用）；新行持有该键。
	old, err := st.GetCommand(ctx, r1.Command.CommandID)
	if err != nil || old == nil || old.ArchivedKey.String != "k-old" || old.SubmittedKey.Valid {
		t.Fatalf("archived old row: %+v err=%v", old, err)
	}
	if r2.Command.SubmittedKey.String != "k-old" {
		t.Fatalf("new row must hold the key: %+v", r2.Command)
	}
	// 窗口内同键同参仍幂等命中（对照，证明检查以窗口为界而非一律新建）。
	dup, err := submit(t, st, cfg, req)
	if err != nil || !dup.Idempotent || dup.Command.CommandID != r2.Command.CommandID {
		t.Fatalf("in-window idempotent: %+v err=%v", dup, err)
	}
}

func TestSubmitQuota429(t *testing.T) {
	st, cfg, _ := openSvc(t)
	for i := 0; i < store.CommandQuotaPerNode; i++ {
		if _, err := submit(t, st, cfg, &SubmitRequest{
			NodeName: "cloud-nanjing", Kind: "ps_snapshot"}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	_, err := submit(t, st, cfg, &SubmitRequest{NodeName: "cloud-nanjing", Kind: "ps_snapshot"})
	if !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("quota: err=%v", err)
	}
}

// TestUUIDShape command_id 为 uuid v4 文本（永不复用的全局唯一主键形态）。
func TestUUIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := UUID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 36 || id[14] != '4' {
			t.Fatalf("uuid shape: %s", id)
		}
		if seen[id] {
			t.Fatalf("uuid collision: %s", id)
		}
		seen[id] = true
	}
}
