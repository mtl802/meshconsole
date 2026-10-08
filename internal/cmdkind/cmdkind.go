// Package cmdkind 定义 L2 任务下发的命令白名单 kind 注册表（SPEC-M1d §3）。
//
// 结构化口径：**无自由 shell 串**——每个 kind = 固定绝对路径 argv 模板 + 类型化
// 参数槽；单横线选项（-u/-n/--no-pager 等）是模板固定部分，参数槽只填占位。
// console（提交校验）与 agent（执行前复核）**双重校验共用本包**，两端规则由
// 同一份代码保证不漂移；agent 对未知 kind 一律拒绝。
//
// 安全基线（DESIGN §4.1-D）：
//   - 可执行文件为固定受信绝对路径（docker_ps 的 {docker_bin} 为唯一例外，
//     由 agent 配置的只读包装路径解析，DESIGN §7-5）；
//   - 参数全串匹配字符集白名单且限长 128，exec 直接 argv 不经 shell；
//   - unit 槽还须落在受管服务集合内（console 按心跳上报的服务清单、agent 按
//     自身配置声明，两边各自校验——本包只做字符集/长度层，集合层由调用方传入）。
package cmdkind

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 槽位参数的全局硬上限（SPEC-M1d §3：参数全串匹配字符集且限长 128）。
const MaxParamLen = 128

// ArgsJSONMax 为 args_json 序列化后的字节上限（结构化参数槽，远小于请求体上限）。
const ArgsJSONMax = 4 << 10

// slotKind 参数槽类型。
type slotKind int

const (
	// SlotUnit 为 systemd unit 名槽：[a-zA-Z0-9_@.-]+，且须通过受管集合判定。
	SlotUnit slotKind = iota
	// slotInt 为整数槽：十进制、闭区间 [Min, Max]。
	slotInt
)

// slot 为一个类型化参数槽。
type slot struct {
	Name    string
	Kind    slotKind
	Min     int64 // slotInt 值域下限
	Max     int64 // slotInt 值域上限
	Managed bool  // true = 该槽还须命中受管服务集合（unit 槽）
}

// Kind 为一个白名单命令 kind 的完整定义。
type Kind struct {
	Name string
	// Platforms 为可执行平台（linux/mac/windows；windows 仅预留，v1 无 kind）。
	Platforms []string
	// RequiredCaps 为能力协商矩阵（SPEC-M1d §5）：任一组合被节点上报 caps 完整
	// 覆盖即可下发（组合内各 cap 须同时具备，组合间为 OR）。
	RequiredCaps [][]string
	// Template 为固定 argv 模板；占位符 {unit}/{n}/{docker_bin} 由校验过的参数
	// 值填充（docker_bin 由 agent 配置解析，console 侧仅校验不展开）。
	Template []string
	// Slots 为参数槽（顺序即文档顺序；空 = 该 kind 无参数）。
	Slots []slot
}

// v1 kinds（SPEC-M1d §3 平台矩阵）。注册表为包级固定值，运行期不可增删——
// l2_extra_commands 扩展的只是「已审核 kind 的参数化实例」，不引入新可执行路径。
var registry = map[string]*Kind{
	"systemctl_status": {
		Name:         "systemctl_status",
		Platforms:    []string{"linux"},
		RequiredCaps: [][]string{{"linux-systemd"}},
		Template:     []string{"/usr/bin/systemctl", "status", "{unit}"},
		Slots:        []slot{{Name: "unit", Kind: SlotUnit, Managed: true}},
	},
	"journalctl_tail": {
		Name:         "journalctl_tail",
		Platforms:    []string{"linux"},
		RequiredCaps: [][]string{{"linux-systemd"}},
		Template:     []string{"/usr/bin/journalctl", "-u", "{unit}", "-n", "{n}", "--no-pager"},
		Slots: []slot{
			{Name: "unit", Kind: SlotUnit, Managed: true},
			{Name: "n", Kind: slotInt, Min: 50, Max: 500},
		},
	},
	"ps_snapshot": {
		Name:         "ps_snapshot",
		Platforms:    []string{"linux", "mac"},
		RequiredCaps: [][]string{{"linux-systemd"}, {"mac-launchd"}},
		Template:     []string{"/bin/ps", "aux"},
	},
	"df_report": {
		Name:         "df_report",
		Platforms:    []string{"linux", "mac"},
		RequiredCaps: [][]string{{"linux-systemd"}, {"mac-launchd"}},
		Template:     []string{"/bin/df", "-h"},
	},
	"launchctl_list": {
		Name:         "launchctl_list",
		Platforms:    []string{"mac"},
		RequiredCaps: [][]string{{"mac-launchd"}},
		Template:     []string{"/bin/launchctl", "list"},
	},
	"mac_log_show": {
		Name:         "mac_log_show",
		Platforms:    []string{"mac"},
		RequiredCaps: [][]string{{"mac-launchd"}},
		Template:     []string{"/usr/bin/log", "show", "--last", "{n}m", "--style", "compact"},
		Slots:        []slot{{Name: "n", Kind: slotInt, Min: 10, Max: 120}},
	},
	"docker_ps": {
		Name:         "docker_ps",
		Platforms:    []string{"linux"},
		RequiredCaps: [][]string{{"linux-docker"}},
		// {docker_bin} 为 agent 配置的只读包装路径（DESIGN §7-5），console 侧
		// 不解析、不校验其值——该 kind 无参数槽，可执行路径完全由 agent 本地
		// 配置决定，console 无法借参数注入新路径。
		Template: []string{"{docker_bin}", "ps", "-a"},
	},
}

// Names 返回全部已注册 kind 名（排序稳定，供工具描述/测试用）。
func Names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lookup 取 kind 定义；未知 kind 返回 nil（agent 据此拒绝）。
func Lookup(name string) *Kind {
	return registry[name]
}

// unitValid 校验 unit 槽字符集白名单（[a-zA-Z0-9_@.-]+，非空、不以 - 开头、
// 限长 128）。与受管服务 target 同一字符集（config.ValidServiceTarget 同口径，
// 本包独立实现避免 config（YAML）依赖进入 agent 侧执行路径）。
func unitValid(s string) bool {
	if s == "" || len(s) > MaxParamLen || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '@' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateArgs 校验参数槽并返回规范化后的 args JSON（键序稳定）。
// 规则：键集合与槽完全一致（缺槽/多键都拒绝——多余键可能是注入载体）、值匹配
// 槽类型、字符串槽全串字符集 + 限长。managed 为 Managed 槽（unit）的集合判定
// 谓词：console 提交侧按「受管服务集合 ∪ l2_extra_commands」判定，agent 执行侧
// 按自身配置声明判定，配置加载期扩展实例传「恒 true」（字符集/值域不豁免）。
// 返回值可直接入库/下发，console/agent 双端字节一致。
func ValidateArgs(kindName, argsJSON string, managed func(unit string) bool) (string, error) {
	k := registry[kindName]
	if k == nil {
		return "", fmt.Errorf("未知 kind %q", kindName)
	}
	if len(argsJSON) > ArgsJSONMax {
		return "", fmt.Errorf("args 过大（>%d 字节）", ArgsJSONMax)
	}
	var in map[string]any
	if strings.TrimSpace(argsJSON) == "" {
		in = map[string]any{}
	} else if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("args 不是合法 JSON 对象")
	}
	// 键集合精确匹配：缺槽拒绝、多余键拒绝。
	if len(in) != len(k.Slots) {
		return "", fmt.Errorf("args 键数不符：kind %q 需要 %d 个参数槽，收到 %d 个", kindName, len(k.Slots), len(in))
	}
	out := make(map[string]string, len(k.Slots))
	for _, s := range k.Slots {
		v, ok := in[s.Name]
		if !ok {
			return "", fmt.Errorf("args 缺少参数 %q", s.Name)
		}
		switch s.Kind {
		case SlotUnit:
			str, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("参数 %q 须为字符串", s.Name)
			}
			if !unitValid(str) {
				return "", fmt.Errorf("参数 %q 不匹配字符集白名单 [a-zA-Z0-9_@.-] 或超长（≤%d）", s.Name, MaxParamLen)
			}
			if s.Managed && !managed(str) {
				return "", fmt.Errorf("参数 %q=%q 不在受管服务集合内", s.Name, str)
			}
			out[s.Name] = str
		case slotInt:
			n, ok := jsonNumber(v)
			if !ok {
				return "", fmt.Errorf("参数 %q 须为整数（JSON 数字或整数字符串）", s.Name)
			}
			if n < s.Min || n > s.Max {
				return "", fmt.Errorf("参数 %q 须在 %d-%d，收到 %d", s.Name, s.Min, s.Max, n)
			}
			out[s.Name] = fmt.Sprintf("%d", n)
		}
	}
	// 序列化带排序键（Go map 序列化本身按键排序），console/agent 双端字节一致。
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// jsonNumber 接受整数槽的两种合法形态（R39-#1 校验与序列化闭环一致）：JSON
// 整数（float64 且整数值）与**整数字符串**——console 侧 ValidateArgs 的规范化
// 产物把整数槽序列化为字符串（out 为 map[string]string），agent 复检同一份
// args_json 时若只认数字形态，合法的 journalctl_tail/mac_log_show 会被误拒。
// 归一后再过值域校验；拒绝浮点（100.5）、指数写法（1e3）、非数字字符串与
// 其他 JSON 类型。
func jsonNumber(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		if t != float64(int64(t)) {
			return 0, false
		}
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// CapsOK 报告节点上报的 caps 是否满足该 kind 的能力矩阵（任一组合被完整覆盖）。
func CapsOK(kindName string, caps []string) bool {
	k := registry[kindName]
	if k == nil {
		return false
	}
	set := make(map[string]bool, len(caps))
	for _, c := range caps {
		set[c] = true
	}
	for _, combo := range k.RequiredCaps {
		ok := true
		for _, c := range combo {
			if !set[c] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// BuildArgv 用已校验的 args JSON 填充模板得到最终 argv。dockerBin 为 agent
// 侧配置的 docker 只读包装路径（仅 docker_ps 需要；空值时该 kind 报错）。
// 本函数只做模板填充，不重复参数校验——调用方必须先过 ValidateArgs（agent
// 侧执行前同样先校验再填充，双重校验的两端各自完整走一遍）。
func BuildArgv(kindName, argsJSON, dockerBin string) ([]string, error) {
	k := registry[kindName]
	if k == nil {
		return nil, fmt.Errorf("未知 kind %q", kindName)
	}
	var args map[string]string
	if len(k.Slots) > 0 {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return nil, fmt.Errorf("args 解析失败")
		}
	}
	out := make([]string, 0, len(k.Template))
	for _, t := range k.Template {
		switch t {
		case "{docker_bin}":
			if dockerBin == "" {
				return nil, fmt.Errorf("kind %q 需要 agent 配置 docker_bin（只读包装）", kindName)
			}
			out = append(out, dockerBin)
		default:
			for _, s := range k.Slots {
				t = strings.ReplaceAll(t, "{"+s.Name+"}", args[s.Name])
			}
			out = append(out, t)
		}
	}
	return out, nil
}
