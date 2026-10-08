package cmdkind

import (
	"strings"
	"testing"
)

// ---- M1d（SPEC-M1d §3）白名单与注入向量测试 ----
// 验收：白名单注入向量全拒（单横线注入/路径遍历/超长/非法 unit）、caps 矩阵、
// argv 模板填充。

func TestValidateArgsAccepts(t *testing.T) {
	managed := func(u string) bool { return u == "headscale" }
	cases := []struct {
		kind, args string
	}{
		{"systemctl_status", `{"unit":"headscale"}`},
		{"systemctl_status", `{"unit":"headscale"}`},        // 谓词内受管 unit
		{"journalctl_tail", `{"unit":"headscale","n":50}`},  // n 下边界
		{"journalctl_tail", `{"unit":"headscale","n":500}`}, // n 上边界
		{"ps_snapshot", `{}`},
		{"ps_snapshot", ``}, // 空串 = 无参
		{"df_report", `{}`},
		{"launchctl_list", `{}`},
		{"mac_log_show", `{"n":10}`},
		{"mac_log_show", `{"n":120}`},
		{"docker_ps", `{}`},
	}
	for _, c := range cases {
		if _, err := ValidateArgs(c.kind, c.args, managed); err != nil {
			t.Errorf("ValidateArgs(%s, %s): %v", c.kind, c.args, err)
		}
	}
}

// TestValidateArgsInjectionVectors 注入向量全拒：单横线选项注入、路径遍历、
// 超长、非法字符、命令分隔符、多余键、缺槽、n 越界/非整数、未知 kind。
func TestValidateArgsInjectionVectors(t *testing.T) {
	managed := func(u string) bool { return u == "headscale" }
	bad := []struct {
		name, kind, args string
	}{
		{"单横线注入", "systemctl_status", `{"unit":"-h"}`},
		{"双横线注入", "systemctl_status", `{"unit":"--no-pager"}`},
		{"路径遍历", "systemctl_status", `{"unit":"../../etc/passwd"}`},
		{"绝对路径", "systemctl_status", `{"unit":"/etc/passwd"}`},
		{"命令分隔符", "systemctl_status", `{"unit":"a;rm -rf /"}`},
		{"管道", "systemctl_status", `{"unit":"a|b"}`},
		{"反引号", "systemctl_status", "{\"unit\":\"a`b\"}"},
		{"空白", "systemctl_status", `{"unit":"head scale"}`},
		{"美元展开", "systemctl_status", `{"unit":"$(whoami)"}`},
		{"超长 129", "systemctl_status", `{"unit":"` + strings.Repeat("a", MaxParamLen+1) + `"}`},
		{"非受管 unit", "systemctl_status", `{"unit":"nginx"}`},
		{"缺槽", "systemctl_status", `{}`},
		{"多余键", "systemctl_status", `{"unit":"headscale","extra":"x"}`},
		{"n 越界低", "journalctl_tail", `{"unit":"headscale","n":49}`},
		{"n 越界高", "journalctl_tail", `{"unit":"headscale","n":501}`},
		{"n 小数", "journalctl_tail", `{"unit":"headscale","n":100.5}`},
		{"n 指数写法", "journalctl_tail", `{"unit":"headscale","n":1e3}`},
		{"n 非数字字符串", "journalctl_tail", `{"unit":"headscale","n":"abc"}`},
		{"n 数字字符串夹私货", "journalctl_tail", `{"unit":"headscale","n":" 100"}`},
		{"n 布尔", "journalctl_tail", `{"unit":"headscale","n":true}`},
		{"n 负数", "journalctl_tail", `{"unit":"headscale","n":-50}`},
		{"mac_log_show n 越界", "mac_log_show", `{"n":9}`},
		{"mac_log_show n 高越界", "mac_log_show", `{"n":121}`},
		{"无参 kind 带参数", "ps_snapshot", `{"unit":"headscale"}`},
		{"未知 kind", "rm_rf_slash", `{}`},
		{"args 非 JSON", "ps_snapshot", `not-json`},
		{"args 数组", "ps_snapshot", `[]`},
	}
	for _, c := range bad {
		if _, err := ValidateArgs(c.kind, c.args, managed); err == nil {
			t.Errorf("%s: ValidateArgs(%s, %s) 必须拒绝，却通过了", c.name, c.kind, c.args)
		}
	}
	// 超长 args 整体（4KB 上限）。
	if _, err := ValidateArgs("ps_snapshot", `{"pad":"`+strings.Repeat("x", ArgsJSONMax)+"\"}", managed); err == nil {
		t.Errorf("超长 args 必须拒绝")
	}
}

// TestValidateArgsManagedPredicate 集合谓词为恒真时（配置加载期扩展实例路径）
// 字符集/值域仍强制——扩展只豁免集合、不豁免形态。
func TestValidateArgsManagedPredicate(t *testing.T) {
	always := func(string) bool { return true }
	if _, err := ValidateArgs("systemctl_status", `{"unit":"nginx"}`, always); err != nil {
		t.Fatalf("扩展实例（恒真谓词）应通过: %v", err)
	}
	if _, err := ValidateArgs("systemctl_status", `{"unit":"-x"}`, always); err == nil {
		t.Fatalf("恒真谓词下仍须拒绝单横线注入")
	}
}

// TestValidateArgsJSONRoundTrip 真实下发链路形态钉住（R39-#1）：console 序列化
// 规范化把整数槽写为字符串（map[string]string 产物），agent 复检同一份字节必须
// 通过——数字形态与数字字符串形态在双端各自成立；浮点/非数字/越界两端同拒。
func TestValidateArgsJSONRoundTrip(t *testing.T) {
	managed := func(u string) bool { return u == "headscale" }
	for _, submit := range []string{
		`{"unit":"headscale","n":200}`,   // JSON 数字形态
		`{"unit":"headscale","n":"200"}`, // 整数字符串形态
	} {
		canonical, err := ValidateArgs("journalctl_tail", submit, managed) // console 侧
		if err != nil {
			t.Fatalf("console 侧 ValidateArgs(%s): %v", submit, err)
		}
		if canonical != `{"n":"200","unit":"headscale"}` {
			t.Fatalf("规范化产物 = %s", canonical)
		}
		// agent 执行前复检的正是这份规范化字节（双重校验的两端）。
		if _, err := ValidateArgs("journalctl_tail", canonical, managed); err != nil {
			t.Fatalf("agent 侧复检规范化产物 %s 被拒: %v", canonical, err)
		}
	}
	// mac_log_show 同口径（字符串数字形态经 agent 复检通过）。
	canon, err := ValidateArgs("mac_log_show", `{"n":"30"}`, managed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateArgs("mac_log_show", canon, managed); err != nil {
		t.Fatalf("mac_log_show 复检被拒: %v", err)
	}
	// 拒绝形态在 JSON 往返后依旧拒绝（不受规范化影响）。
	for _, bad := range []string{`{"unit":"headscale","n":12.5}`, `{"unit":"headscale","n":"abc"}`, `{"unit":"headscale","n":501}`} {
		canon, err := ValidateArgs("journalctl_tail", bad, managed)
		if err == nil {
			t.Fatalf("%s 必须拒绝", bad)
		}
		if canon != "" {
			t.Fatalf("拒绝形态不得产出规范化产物")
		}
	}
}

func TestCapsOK(t *testing.T) {
	cases := []struct {
		kind string
		caps []string
		want bool
	}{
		{"systemctl_status", []string{"linux-systemd"}, true},
		{"systemctl_status", []string{"mac-launchd"}, false},
		{"systemctl_status", nil, false},
		{"ps_snapshot", []string{"linux-systemd"}, true},
		{"ps_snapshot", []string{"mac-launchd"}, true},
		{"ps_snapshot", []string{"windows"}, false},
		{"launchctl_list", []string{"mac-launchd"}, true},
		{"launchctl_list", []string{"linux-systemd"}, false},
		{"docker_ps", []string{"linux-docker"}, true},
		{"docker_ps", []string{"linux-systemd"}, false},
		{"mac_log_show", []string{"mac-launchd"}, true},
		{"未知", []string{"linux-systemd"}, false},
	}
	for _, c := range cases {
		if got := CapsOK(c.kind, c.caps); got != c.want {
			t.Errorf("CapsOK(%s, %v) = %v, want %v", c.kind, c.caps, got, c.want)
		}
	}
}

func TestBuildArgv(t *testing.T) {
	// canonical args（ValidateArgs 产物）的整数为字符串形态。
	argv, err := BuildArgv("journalctl_tail", `{"n":"200","unit":"headscale"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/journalctl", "-u", "headscale", "-n", "200", "--no-pager"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}
	// mac_log_show 的 {n}m 槽内嵌填充。
	argv, err = BuildArgv("mac_log_show", `{"n":"30"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if argv[2] != "--last" || argv[3] != "30m" || argv[4] != "--style" || argv[5] != "compact" {
		t.Fatalf("mac_log_show argv = %v", argv)
	}
	// docker_ps 的 {docker_bin} 由 agent 配置解析；空值拒绝。
	if _, err := BuildArgv("docker_ps", `{}`, ""); err == nil {
		t.Fatalf("docker_ps 缺 docker_bin 必须拒绝")
	}
	argv, err = BuildArgv("docker_ps", `{}`, "/usr/local/bin/docker-ro")
	if err != nil || argv[0] != "/usr/local/bin/docker-ro" || argv[1] != "ps" || argv[2] != "-a" {
		t.Fatalf("docker_ps argv = %v err=%v", argv, err)
	}
}

func TestRegistryFixedPaths(t *testing.T) {
	// 白名单硬约束的可执行路径锚点：模板首元素必须为受信绝对路径
	// （docker_ps 的 {docker_bin} 为唯一受控例外）。
	for _, name := range Names() {
		k := Lookup(name)
		if k == nil {
			t.Fatalf("Lookup(%s) = nil", name)
		}
		if k.Template[0] == "{docker_bin}" {
			continue
		}
		if !strings.HasPrefix(k.Template[0], "/usr/bin/") && !strings.HasPrefix(k.Template[0], "/bin/") {
			t.Errorf("kind %s executable %q 不是受信绝对路径", name, k.Template[0])
		}
	}
}
