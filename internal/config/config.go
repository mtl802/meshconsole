// Package config 加载 console 与 agent 的 YAML 配置。
// 字段口径见 DESIGN.md §4.1/§4.3/§7；缺省值在代码内收敛，配置文件可覆盖。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FlexTime 兼容带引号/不带引号的 RFC3339 时间戳（yaml.v3 仅把裸时间戳解析为
// time.Time，带引号的是 !!str，需自行解析）。
type FlexTime struct{ time.Time }

// UnmarshalYAML 实现两种标量形态的解析；无法解析时报出字段语义清晰的错误。
func (t *FlexTime) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		if s == "" {
			t.Time = time.Time{}
			return nil
		}
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("时间戳须为 RFC3339（如 2026-12-31T23:59:59Z）: %w", err)
		}
		t.Time = parsed
		return nil
	}
	var raw time.Time
	if err := unmarshal(&raw); err != nil {
		return fmt.Errorf("时间戳须为 RFC3339（如 2026-12-31T23:59:59Z）: %w", err)
	}
	t.Time = raw
	return nil
}

// tokenExampleYAML 为注册 token 映射写法的报错提示样例（纯字符串写法已移除）。
const tokenExampleYAML = `  - token: "替换为-openssl-rand-hex-16-生成值"
    expires_at: "2026-12-31T23:59:59Z"
    expected_node: "node-1"`

// RegistrationToken 为一次性注册 token 及其强制约束
// （DESIGN §4.1-A：短期、一次性、绑定预期节点身份，用后即废）。
// R3-#4 强制化：只接受映射写法，expires_at 与 expected_node 均为必填——
// 无到期、不绑节点的 token 等于永久凭据后门，配置加载即拒绝。
type RegistrationToken struct {
	// Token 为注册 token 明文（仅存在于 console 配置文件，不入库不出接口）。
	Token string `yaml:"token"`
	// ExpiresAt 为到期时刻（RFC3339，必填，如 2026-10-08T00:00:00Z）。
	// 到期后该 token 认证一律拒绝（401）；启动时已过期的 token 直接拒绝加载。
	ExpiresAt FlexTime `yaml:"expires_at"`
	// ExpectedNode 为预期节点名绑定（必填）：仅该节点名可用此 token 注册，
	// 其他节点名一律 401（不泄露 token 有效性）。
	ExpectedNode string `yaml:"expected_node"`
}

// UnmarshalYAML 只接受映射写法（R3-#4 强制化）。纯字符串标量在解析层即报错，
// 并提示新写法；缺失 expires_at/expected_node 由 LoadConsole 的启动校验拒绝。
func (t *RegistrationToken) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type plain RegistrationToken
	var p plain
	if err := unmarshal(&p); err == nil {
		if p.Token == "" {
			return fmt.Errorf("注册 token 须为映射写法且 token 字段非空，例如:\n%s", tokenExampleYAML)
		}
		*t = RegistrationToken(p)
		return nil
	}
	var s string
	if err := unmarshal(&s); err == nil {
		return fmt.Errorf("注册 token 不再接受纯字符串写法（无到期、不绑定节点，等于永久凭据）: %q\n请改为映射写法，例如:\n%s", s, tokenExampleYAML)
	}
	return fmt.Errorf("注册 token 须为映射写法（token + expires_at + expected_node），例如:\n%s", tokenExampleYAML)
}

// Console 为 meshconsole（控制台）配置。
type Console struct {
	Listen string `yaml:"listen"`
	// DBPath 为 SQLite 数据库文件路径；目录不存在时启动自动创建。支持 ~ 展开。
	DBPath string `yaml:"db_path"`
	// RegistrationTokens 为一次性注册 token 列表（每个仅可成功使用一次，用后即废），
	// 由部署时带外注入，不入库、不出现在任何接口响应中。只接受映射写法，
	// 且每个 token 必须含未来 expires_at 与非空 expected_node（R3-#4 强制化）。
	RegistrationTokens []RegistrationToken `yaml:"registration_tokens"`
	// OfflineAfterS 为无心跳判定离线的秒数（DESIGN §4.1：默认 60s）。
	OfflineAfterS int `yaml:"offline_after_s"`
	// MetricsRetentionDays 为 metrics 保留天数（默认 7）。
	MetricsRetentionDays int    `yaml:"metrics_retention_days"`
	LogLevel             string `yaml:"log_level"`
}

// Agent 为 meshagent（节点代理）配置。
type Agent struct {
	// ConsoleURL 留空时使用本地 state 文件里注册时记录的地址。
	ConsoleURL string `yaml:"console_url"`
	// StateFile 为节点 token 持久化路径（0600）。空值取平台默认。支持 ~ 展开。
	StateFile string `yaml:"state_file"`
	Role      string `yaml:"role"`
	// CollectIntervalS 为采集上报间隔（DESIGN §4.1：每 15s）。
	CollectIntervalS int `yaml:"collect_interval_s"`
	// DiskMount 为磁盘指标采集挂载点；空值按平台取 "/"（Windows 取 "C:\\"）。
	DiskMount string `yaml:"disk_mount"`
	LogLevel  string `yaml:"log_level"`
}

func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return data, nil
}

// ExpandHome 展开路径开头的 ~（Go 的 os 包不做 shell 式展开，须显式处理）。
// 仅支持 "~" 与 "~/…"（"~user" 不支持，原样返回）；无法取得 home 时原样返回。
func ExpandHome(p string) string {
	if p == "" || p[0] != '~' || (len(p) > 1 && p[1] != '/' && p[1] != '\\') {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if len(p) == 1 {
		return home
	}
	return filepath.Join(home, p[1:])
}

// LoadConsole 载入控制台配置。path 为空或文件不存在时返回纯缺省配置。
func LoadConsole(path string) (*Console, error) {
	cfg := &Console{
		Listen:               "127.0.0.1:7700",
		DBPath:               "./data/meshconsole.db",
		OfflineAfterS:        60,
		MetricsRetentionDays: 7,
		LogLevel:             "info",
	}
	if path == "" {
		return cfg, nil
	}
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.DBPath = ExpandHome(cfg.DBPath)
	if cfg.Listen == "" || cfg.DBPath == "" {
		return nil, fmt.Errorf("console.listen / console.db_path 不得为空")
	}
	if len(cfg.RegistrationTokens) == 0 {
		return nil, fmt.Errorf("console.registration_tokens: 至少需要一个一次性注册 token")
	}
	for i, t := range cfg.RegistrationTokens {
		if len(t.Token) < 16 {
			return nil, fmt.Errorf("console.registration_tokens[%d]: token 长度不得少于 16 字符", i)
		}
		// R3-#4 强制化：到期与绑定缺失的 token 等于永久凭据，加载即拒绝。
		if t.ExpiresAt.Time.IsZero() {
			return nil, fmt.Errorf("console.registration_tokens[%d]: 缺少 expires_at（所有 token 必须为映射写法且含到期时刻），例如 expires_at: \"2026-12-31T23:59:59Z\"", i)
		}
		// 已过期的 token 毫无用处，留在配置里只会造成「注册 401」的排查噪音：拒绝启动。
		if expiresAt := t.ExpiresAt.Time; !time.Now().Before(expiresAt) {
			return nil, fmt.Errorf("console.registration_tokens[%d]: token 已过期（expires_at=%s），请移除或换新",
				i, expiresAt.Format(time.RFC3339))
		}
		t.ExpectedNode = strings.TrimSpace(t.ExpectedNode)
		if t.ExpectedNode == "" {
			return nil, fmt.Errorf("console.registration_tokens[%d]: 缺少 expected_node（token 必须绑定预期节点名）", i)
		}
		cfg.RegistrationTokens[i] = t
	}
	if cfg.OfflineAfterS <= 0 || cfg.MetricsRetentionDays <= 0 {
		return nil, fmt.Errorf("console.offline_after_s / metrics_retention_days 必须为正数")
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return cfg, nil
}

// LoadAgent 载入代理配置。path 为空或文件不存在时返回纯缺省配置。
func LoadAgent(path string) (*Agent, error) {
	cfg := &Agent{
		Role:             "node",
		CollectIntervalS: 15,
		DiskMount:        DefaultDiskMount(),
		StateFile:        DefaultStatePath(),
		LogLevel:         "info",
	}
	if path == "" {
		return cfg, nil
	}
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.CollectIntervalS <= 0 {
		return nil, fmt.Errorf("agent.collect_interval_s 必须为正数")
	}
	if cfg.Role == "" {
		cfg.Role = "node"
	}
	if cfg.DiskMount == "" {
		cfg.DiskMount = DefaultDiskMount()
	}
	cfg.StateFile = ExpandHome(strings.TrimSpace(cfg.StateFile))
	if cfg.StateFile == "" {
		cfg.StateFile = DefaultStatePath()
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return cfg, nil
}

// DefaultStatePath 返回 agent 本地 state 默认路径：$HOME/.meshagent/state.json。
// 与样例配置 deploy/agent.example.yaml 的 state_file 一致（~ 由 ExpandHome 展开）。
func DefaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "meshagent-state.json"
	}
	return filepath.Join(home, ".meshagent", "state.json")
}

// DefaultDiskMount 按平台返回默认磁盘挂载点。
func DefaultDiskMount() string {
	if runtime.GOOS == "windows" {
		return `C:\`
	}
	return "/"
}
