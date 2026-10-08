// Package config 加载 console 与 agent 的 YAML 配置。
// 字段口径见 DESIGN.md §4.1/§4.3/§7；缺省值在代码内收敛，配置文件可覆盖。
package config

import (
	"fmt"
	"net"
	"net/url"
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
		// 不回显原值（R15-#1，与 R11-C/R13-#3 同口径）：纯字符串写法本身就是
		// 凭据材料，不得进错误信息/日志；只说明拒绝原因与映射写法。
		return fmt.Errorf("注册 token 不再接受纯字符串写法（无到期、不绑定节点，等于永久凭据）\n请改为映射写法，例如:\n%s", tokenExampleYAML)
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
	MetricsRetentionDays int `yaml:"metrics_retention_days"`
	// PKIDir 为 CA 与服务端证书目录（DESIGN §7-3；默认 ./pki，生产 /opt/meshconsole/pki）。
	// 支持默认值覆盖，路径支持 ~ 展开。console 启动只加载其中证书，不重新生成。
	PKIDir string `yaml:"pki_dir"`
	// TLSCert / TLSKey 显式指定服务端证书与私钥路径；留空取 <pki_dir>/server.crt|key。
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
	// TailnetIP 为本机 Tailnet IP：仅 `meshconsole pki` 生成证书时写入 SAN 用；
	// 留空则 SAN 只含 localhost/主机名/回环地址。
	TailnetIP string `yaml:"tailnet_ip"`
	// MaxConnections 为连接总数上限（DESIGN §7-4/§7-9，LimitListener，默认 256）。
	// 第 max_connections+1 条并发连接在内核 accept 队列排队而非被拒。
	MaxConnections int `yaml:"max_connections"`
	// Headscale 为 Headscale 集成配置（M1b-b，只读拉取）。nil（配置无该段）=
	// 集成禁用（启动日志 INFO 一次，不报错）。
	Headscale *HeadscaleConfig `yaml:"headscale"`
	// PanelAllowedHosts 为面板 Host/Origin 白名单（SPEC-M1b-c2 §4）。nil（配置
	// 未出现该键）= 缺省回环名单（localhost/127.0.0.1/::1，与历史行为一致）；
	// 显式给出（含空数组）→ 整体替换——显式空数组等价「仅接受回环 Host」。
	// 条目为裸主机名/IP 字面量（不带 scheme/端口/path），加载时规范化去重。
	PanelAllowedHosts *[]string `yaml:"panel_allowed_hosts"`
	// AgentAllowedCIDRs 为 /api/agent/* 来源收敛网段（SPEC §5）。nil = 缺省
	// 私网清单（127/8、::1、10/8、172.16/12、192.168/16、100.64/10）；显式空
	// 数组 = 仅回环。注意：这是网络层过滤非身份认证，节点 token 校验独立保留。
	AgentAllowedCIDRs *[]string `yaml:"agent_allowed_cidrs"`
	// TLSExtraSANs 为追加 SAN（SPEC §6）：IP 字面量 → IP SAN，合法 DNS 名 →
	// DNS SAN；URL/端口/CIDR/空值一律拒绝，规范化去重。校验在 pki 生成侧
	// 任何写入之前统一执行（internal/pki NormalizeSANs）。部署值示例
	// ["1.13.158.180"]。
	TLSExtraSANs []string `yaml:"tls_extra_sans"`
	LogLevel     string   `yaml:"log_level"`

	// panelHosts / agentCIDRs 为加载期规范化产物（不入 YAML）。hostAllowed
	// 与 registry 来源过滤直接消费。
	panelHosts []string
	agentCIDRs []*net.IPNet
}

// HeadscaleConfig 为 headscale REST 拉取配置（SPEC-M1b-b §3）。
// url 默认 http://127.0.0.1:8080；interval_s 默认 300s。API key 由部署带外
// 注入（DESIGN §7-9：不入 git）。
type HeadscaleConfig struct {
	// URL 为 headscale API 基地址（如 http://127.0.0.1:8080），不带末尾斜杠。
	URL string `yaml:"url"`
	// APIKey 为 headscale 预生成 API key（Authorization: Bearer）。
	APIKey string `yaml:"api_key"`
	// IntervalS 为拉取周期秒数；缺省 300（DESIGN §4.1-F：低频只读拉取）。
	IntervalS int `yaml:"interval_s"`
}

// CertKeyPaths 返回服务端证书与私钥路径（显式配置优先，缺省落 pki_dir）。
func (c *Console) CertKeyPaths() (cert, key string) {
	cert, key = c.TLSCert, c.TLSKey
	if cert == "" {
		cert = filepath.Join(c.PKIDir, "server.crt")
	}
	if key == "" {
		key = filepath.Join(c.PKIDir, "server.key")
	}
	return cert, key
}

// ServiceDecl 为 agent 配置里声明的受管服务（DESIGN §4.3、SPEC-M1b-a §2）。
// 状态每 15s 随心跳上报；type 决定查询途径，target 为查询对象（白名单字符集）。
type ServiceDecl struct {
	// Name 为服务显示名（console services 表按 (node_id, name) 唯一）。
	Name string `yaml:"name"`
	// Type ∈ systemd | docker | process；非法值拒绝启动。
	Type string `yaml:"type"`
	// Target 为查询对象：systemd unit 名 / docker 容器名 / pgrep -f 模式。
	Target string `yaml:"target"`
}

// CustomAgent 为显式声明的 CLI 型 AI agent（DESIGN §4.2-A；声明优先于 PATH 探测）。
type CustomAgent struct {
	Name string `yaml:"name"`
	// Type 目前仅支持 cli（服务型走 AgentScanCfg.Services 端口探测）。
	Type string `yaml:"type"`
	// Command 必须为绝对路径（相对路径拒绝启动，SPEC §4 配置校验）。
	Command string `yaml:"command"`
	// VersionFlag 为版本探测参数，缺省 "--version"。
	VersionFlag string `yaml:"version_flag"`
	// SessionDir 为该 agent 的会话目录（SPEC-M1b-c §2.1 last_activity 辅证）：
	// 只 stat 不读内容，取最近 mtime 与文件数。支持 ~ 展开；空值 = 不统计
	// （last_activity 上报缺席）。known 清单的目录映射内置在 collect 包。
	SessionDir string `yaml:"session_dir"`
}

// CustomAgentService 为显式声明的服务型 AI agent：仅本地端口探测存活性登记，
// 不可 invoke（DESIGN §1 v1 边界：服务型 agent 只发现登记）。
type CustomAgentService struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
}

// AgentScanCfg 为 AI agent 扫描配置（DESIGN §4.2-A 双轨机制）。
type AgentScanCfg struct {
	// Known 覆盖内置已知 CLI 清单（默认 zcode/codex/claude/gemini/aider）。
	// nil（配置里未出现 known 键）→ 用默认清单；显式给出（含空列表）→ 整体替换，
	// 增删都通过改列表完成。扫描方式：PATH 存在性 + --version（5s 超时）。
	Known *[]string `yaml:"known"`
	// Custom 为自定义 CLI 声明，与 PATH 探测结果合并、同名时声明优先。
	Custom []CustomAgent `yaml:"custom"`
	// Services 为服务型 agent（端口探测登记，不参与 invoke）。
	Services []CustomAgentService `yaml:"services"`
}

// DefaultKnownAgents 为内置已知 AI CLI 清单（SPEC-M1b-a §3）。
func DefaultKnownAgents() []string {
	return []string{"zcode", "codex", "claude", "gemini", "aider"}
}

// maxAgentScanTotal 为 agent_scan 合并去重后的 agent 总数上限（R10-#7），
// 与 console 心跳 agents 数组上限一致（internal/registry maxAgents=64）。
const maxAgentScanTotal = 64

// KnownList 返回生效的已知清单（未配置取默认）。
func (a AgentScanCfg) KnownList() []string {
	if a.Known == nil {
		return DefaultKnownAgents()
	}
	return *a.Known
}

// Agent 为 meshagent（节点代理）配置。
type Agent struct {
	// ConsoleURL 留空时使用本地 state 文件里注册时记录的地址。
	// https:// 时必须配置 CACert 或 Fingerprint 之一（不提供跳过验证的选项）。
	ConsoleURL string `yaml:"console_url"`
	// StateFile 为节点 token 持久化路径（0600）。空值取平台默认。支持 ~ 展开。
	StateFile string `yaml:"state_file"`
	Role      string `yaml:"role"`
	// CollectIntervalS 为采集上报间隔（DESIGN §4.1：每 15s）。
	CollectIntervalS int `yaml:"collect_interval_s"`
	// DiskMount 为磁盘指标采集挂载点；空值按平台取 "/"（Windows 取 "C:\\"）。
	DiskMount string `yaml:"disk_mount"`
	// CACert 为控制台 CA 证书 PEM 路径（DESIGN §7-3 带外分发）；与 Fingerprint
	// 任一配置即启用 TLS 验证。仅对 https:// 上报生效。
	CACert string `yaml:"ca_cert"`
	// Fingerprint 为控制台服务端证书 SHA-256 指纹（`meshconsole pki` 输出）；
	// hex，冒号分隔与大小写均可，加载时归一化。
	Fingerprint string `yaml:"fingerprint"`
	// DockerBin 为 docker CLI 可执行路径（R10-#4，默认 "docker"）。生产部署中
	// docker 访问必须以只读 helper 或 socket-proxy 提供（DESIGN §7-5，M1b-b 部署
	// 落地），本项用于指向受限包装脚本。
	DockerBin string `yaml:"docker_bin"`
	// Services 为受管服务声明（每 15s 查询状态随心跳上报）。
	Services []ServiceDecl `yaml:"services"`
	// AgentScan 为 AI agent 发现配置；nil（未配置）等价零值（默认清单、无自定义）。
	AgentScan *AgentScanCfg `yaml:"agent_scan"`
	LogLevel  string        `yaml:"log_level"`
}

// ScanCfg 返回 agent 扫描配置（未配置时返回零值结构）。
func (a *Agent) ScanCfg() AgentScanCfg {
	if a.AgentScan == nil {
		return AgentScanCfg{}
	}
	return *a.AgentScan
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

// parseConsole 解析 console 配置文件并做与注册 token 无关的公共校验。
// 返回 loaded=false 表示空路径或文件不存在（调用方拿到纯缺省配置、不做强制校验，
// 与 M1a 行为一致）。
func parseConsole(path string) (*Console, bool, error) {
	cfg := &Console{
		Listen:               "127.0.0.1:7700",
		DBPath:               "./data/meshconsole.db",
		OfflineAfterS:        60,
		MetricsRetentionDays: 7,
		PKIDir:               "./pki",
		MaxConnections:       256,
		LogLevel:             "info",
	}
	if path == "" {
		return cfg, false, nil
	}
	data, err := readFile(path)
	if err != nil {
		return nil, false, err
	}
	if data == nil {
		return cfg, false, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, false, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.DBPath = ExpandHome(cfg.DBPath)
	cfg.PKIDir = ExpandHome(cfg.PKIDir)
	cfg.TLSCert = ExpandHome(cfg.TLSCert)
	cfg.TLSKey = ExpandHome(cfg.TLSKey)
	if cfg.Listen == "" || cfg.DBPath == "" {
		return nil, false, fmt.Errorf("console.listen / console.db_path 不得为空")
	}
	if cfg.MaxConnections <= 0 {
		return nil, false, fmt.Errorf("console.max_connections 必须为正数（LimitListener 连接总数上限）")
	}
	if cfg.OfflineAfterS <= 0 || cfg.MetricsRetentionDays <= 0 {
		return nil, false, fmt.Errorf("console.offline_after_s / metrics_retention_days 必须为正数")
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	// SPEC-M1b-c2 §4/§5：Host 白名单与 agent 来源网段加载期统一校验、规范化
	// 去重后落内部字段——非法项启动即报错，不留到运行期。
	hosts, err := normalizeHostList(cfg.PanelAllowedHosts, "console.panel_allowed_hosts")
	if err != nil {
		return nil, false, err
	}
	cfg.panelHosts = hosts
	cidrs, err := parseCIDRList(cfg.AgentAllowedCIDRs, "console.agent_allowed_cidrs")
	if err != nil {
		return nil, false, err
	}
	cfg.agentCIDRs = cidrs
	if err := cfg.validateHeadscale(); err != nil {
		return nil, false, err
	}
	return cfg, true, nil
}

// defaultPanelHosts 为 Host 白名单缺省名单（回环形态，与历史硬编码一致）。
func defaultPanelHosts() []string {
	return []string{"localhost", "127.0.0.1", "::1"}
}

// defaultAgentCIDRs 为 /api/agent/* 来源缺省网段（SPEC §5：回环 + 私网 +
// Tailnet CGNAT 段）。
func defaultAgentCIDRs() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"} {
		_, n, _ := net.ParseCIDR(s)
		out = append(out, n)
	}
	return out
}

// normalizeHostList 规范化 Host 白名单：nil → 缺省回环名单；显式空数组 → 仅
// 回环（与缺省同值，SPEC §4 口径——公网形态下回环 Host 是最低可用面）；条目
// 须为裸主机名/IP 字面量——不带 scheme、端口、path/query/fragment/userinfo，
// 小写规范化去重（保持原序）。
func normalizeHostList(raw *[]string, field string) ([]string, error) {
	if raw == nil || len(*raw) == 0 {
		return defaultPanelHosts(), nil
	}
	var out []string
	seen := map[string]bool{}
	for i, item := range *raw {
		s := strings.TrimSpace(item)
		if s == "" {
			return nil, fmt.Errorf("%s[%d]: 条目不得为空（裸主机名或 IP 字面量，如 1.13.158.180）", field, i)
		}
		if strings.ContainsAny(s, " \t\r\n") {
			return nil, fmt.Errorf("%s[%d]: %q 含空白字符", field, i, item)
		}
		// IP 字面量直通（裸 IPv6 形如 ::1 含多个冒号，url.Parse 无法按
		// authority 解析——先剥方括号再 ParseIP，规范化取 ip.String()）。
		bare := s
		if strings.HasPrefix(bare, "[") && strings.HasSuffix(bare, "]") {
			bare = bare[1 : len(bare)-1]
		}
		if ip := net.ParseIP(bare); ip != nil {
			if host := ip.String(); !seen[host] {
				seen[host] = true
				out = append(out, host)
			}
			continue
		}
		if strings.Contains(s, "://") {
			return nil, fmt.Errorf("%s[%d]: %q 是 URL——条目为裸主机名/IP 字面量", field, i, item)
		}
		u, err := url.Parse("http://" + s)
		if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%s[%d]: %q 不是合法的裸主机名/IP 字面量（不得含 scheme/端口/path）", field, i, item)
		}
		if u.Port() != "" {
			return nil, fmt.Errorf("%s[%d]: %q 不得携带端口（名单匹配的是主机名，端口在请求侧任意）", field, i, item)
		}
		host := strings.ToLower(u.Hostname())
		if host == "" {
			return nil, fmt.Errorf("%s[%d]: %q 缺主机名", field, i, item)
		}
		if !validHostname(host) {
			return nil, fmt.Errorf("%s[%d]: %q 不是合法 DNS 主机名", field, i, item)
		}
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	return out, nil
}

// validHostname 按 RFC 1123 校验主机名（标签 1-63 字符、[a-z0-9-]、首尾不连字符、
// 总长 ≤253；单标签合法——localhost 即单标签）。
func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// parseCIDRList 解析 agent 来源网段：nil → 缺省私网清单；显式空数组 → 仅回环
// （SPEC §5 口径）。条目须为合法 CIDR。
func parseCIDRList(raw *[]string, field string) ([]*net.IPNet, error) {
	if raw == nil {
		return defaultAgentCIDRs(), nil
	}
	if len(*raw) == 0 {
		return []*net.IPNet{
			mustCIDR("127.0.0.0/8"),
			mustCIDR("::1/128"),
		}, nil
	}
	out := make([]*net.IPNet, 0, len(*raw))
	for i, item := range *raw {
		s := strings.TrimSpace(item)
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %q 不是合法 CIDR（如 100.64.0.0/10）", field, i, item)
		}
		out = append(out, n)
	}
	return out, nil
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("config: bad builtin cidr " + s)
	}
	return n
}

// AllowedHosts 返回规范化后的面板 Host 白名单（加载期已校验）。
func (c *Console) AllowedHosts() []string { return c.panelHosts }

// AgentCIDRs 返回解析后的 agent 来源网段（加载期已校验）。
func (c *Console) AgentCIDRs() []*net.IPNet { return c.agentCIDRs }

// CheckConfigFileSecurity 为公网模式的启动门槛之一（SPEC §4）：config 文件须
// 为普通文件，且平台专属的权限约束成立——unix 形态为 0600 + 属运行用户；
// Windows 形态为 ACL/属主校验（仅 当前用户/SYSTEM/Administrators 持有访问权、
// 属主为当前用户或管理员组，R33-#2 实装）。任一约束无法确认即按检查失败处理
// （公网形态拒绝启动并给 remediation），不允许静默放行。
func CheckConfigFileSecurity(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("公网模式启动被拒绝：%s 不是普通文件", path)
	}
	return checkConfigPlatformSecurity(path)
}

// aclVerdict 为 Windows ACL 检查的判定核心（R33-#2）：纯函数、平台无关，
// 单测在任意平台覆盖错误路径。规则＝属主合规（ownerSelfOK：属主为当前用户
// 或管理员组）且 DACL 中每条生效的允许 ACE 的 trustee 都在可信集
// {当前用户, SYSTEM, BUILTIN\Administrators} 内——即 config 仅这三类账户持有
// 访问权。path 仅用于错误信息中的 remediation 指引。
func aclVerdict(path string, ownerSelfOK bool, allowTrustees, trusted []string) error {
	if !ownerSelfOK {
		return fmt.Errorf("公网模式启动被拒绝：config 文件 %s 的属主既非运行用户也非管理员组；请将文件属主改为运行用户（或由 Administrators 持有）后重试", path)
	}
	trust := make(map[string]bool, len(trusted))
	for _, s := range trusted {
		trust[s] = true
	}
	for _, a := range allowTrustees {
		if !trust[a] {
			return fmt.Errorf("公网模式启动被拒绝：config 文件 %s 的 ACL 向受信账户之外授予权限（公网形态要求仅 属主/SYSTEM/Administrators 可访问）；请执行 `icacls %s /inheritance:r` 后仅授予运行用户与 SYSTEM、Administrators 权限", path, path)
		}
	}
	return nil
}

// validateHeadscale 校验 headscale 段（未配置 = 集成禁用，合法）。
// url/api_key 缺省与错误口径见 SPEC-M1b-b §3；interval_s 下限 30s（只读拉取保持低频）。
// R19-#10：url 的 scheme 限 http/https——其他 scheme（ftp/file/ssh 等）与无
// scheme 形式一律启动报错，不留到运行期拉取才失败。
func (c *Console) validateHeadscale() error {
	if c.Headscale == nil {
		return nil
	}
	h := c.Headscale
	h.URL = strings.TrimSpace(h.URL)
	if h.URL == "" {
		h.URL = "http://127.0.0.1:8080"
	}
	if u, err := url.Parse(h.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("console.headscale.url 须为合法 HTTP(S) 基地址（scheme 限 http/https，如 http://127.0.0.1:8080）")
	}
	h.APIKey = strings.TrimSpace(h.APIKey)
	if h.APIKey == "" {
		return fmt.Errorf("console.headscale.api_key 不得为空（headscale 预生成 API key；不需要集成时请删除 headscale 段）")
	}
	if h.IntervalS == 0 {
		h.IntervalS = 300
	}
	if h.IntervalS < 30 {
		return fmt.Errorf("console.headscale.interval_s 不得低于 30（只读拉取保持低频）")
	}
	return nil
}

// LoadConsole 载入控制台配置（服务模式，含注册 token 强制校验）。
// path 为空或文件不存在时返回纯缺省配置（不做 token 校验，与 M1a 行为一致）。
func LoadConsole(path string) (*Console, error) {
	cfg, loaded, err := parseConsole(path)
	if err != nil {
		return nil, err
	}
	if !loaded {
		return cfg, nil
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
	return cfg, nil
}

// LoadConsoleForPKI 载入控制台配置但不强制注册 token（`meshconsole pki` 生成证书
// 时使用：pki 子命令只关心 pki_dir/tailnet_ip 等字段，不应因 token 未配置而拒绝）。
// 共享字段的校验（listen/db_path/连接上限）与完整加载保持一致。
func LoadConsoleForPKI(path string) (*Console, error) {
	cfg, _, err := parseConsole(path)
	return cfg, err
}

// NormalizeFingerprint 归一化证书指纹：去冒号/空白、转小写；非 64 位 hex 报错。
func NormalizeFingerprint(fp string) (string, error) {
	var b strings.Builder
	for _, r := range fp {
		if r == ':' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	s := strings.ToLower(b.String())
	if len(s) != 64 {
		return "", fmt.Errorf("fingerprint 须为服务端证书 SHA-256 指纹的 64 位 hex（meshconsole pki 会输出）")
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			// 不回显原值：错误信息不留任何证书材料片段（R11-C）。
			return "", fmt.Errorf("fingerprint 含非 hex 字符（须为 64 位 hex，`meshconsole pki` 输出）")
		}
	}
	return s, nil
}

// ValidServiceTarget 报告 systemd/docker 型 target 是否在白名单字符集内
// （[a-zA-Z0-9_@.-]，SPEC-M1b-a §2：exec 固定 argv + 字符集白名单双保险）。
// 以 - 开头一律拒绝：exec 无 shell 仍可能被 systemctl/docker 当作选项解析。
func ValidServiceTarget(target string) bool {
	if target == "" || target[0] == '-' {
		return false
	}
	for _, r := range target {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '@' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

// validProcessTarget 校验 process 型 target（pgrep -f 模式）：可含空格但禁止
// 控制字符与开头的 -（防被解析为 pgrep 参数），长度有界。
func validProcessTarget(target string) bool {
	if target == "" || len(target) > 256 || target[0] == '-' {
		return false
	}
	for _, r := range target {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validLabelCfg(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// validateAgentServices 校验受管服务声明：type 合法、target 白名单、名称唯一、数量有界。
func validateAgentServices(services []ServiceDecl) error {
	if len(services) > 32 {
		return fmt.Errorf("agent.services: 条目数不得超过 32")
	}
	seen := map[string]bool{}
	for i, s := range services {
		if !validLabelCfg(s.Name, 128) {
			return fmt.Errorf("agent.services[%d]: name 非空、无空白且 ≤128 字节", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("agent.services[%d]: name %q 重复", i, s.Name)
		}
		seen[s.Name] = true
		switch s.Type {
		case "systemd", "docker":
			// R17-#2：长度上限与服务端入库截断口径（256 字节）一致——超长在
			// 启动即拒绝，避免运行期被静默截断后 agent 与 console 状态错位。
			if len(s.Target) > 256 {
				return fmt.Errorf("agent.services[%d] (%s): target 过长（≤256 字节），got %d", i, s.Type, len(s.Target))
			}
			if !ValidServiceTarget(s.Target) {
				return fmt.Errorf("agent.services[%d] (%s): target 须匹配 [a-zA-Z0-9_@.-] 且非空，got %q", i, s.Type, s.Target)
			}
		case "process":
			if !validProcessTarget(s.Target) {
				return fmt.Errorf("agent.services[%d] (process): target 须非空、无控制字符、不以 - 开头", i)
			}
		default:
			return fmt.Errorf("agent.services[%d]: type 须为 systemd|docker|process，got %q", i, s.Type)
		}
	}
	return nil
}

// validateAgentScan 校验 AI agent 扫描声明：custom 命令绝对路径、类型合法、
// 名称唯一（custom 与 services 之间亦不得重名），端口在值域内。
func validateAgentScan(scan *AgentScanCfg) error {
	if scan == nil {
		return nil
	}
	if scan.Known != nil {
		seen := map[string]bool{}
		for _, n := range *scan.Known {
			if !validLabelCfg(n, 64) {
				return fmt.Errorf("agent_scan.known: %q 须为非空无空白 ≤64 字节的 CLI 名", n)
			}
			if seen[n] {
				return fmt.Errorf("agent_scan.known: %q 重复", n)
			}
			seen[n] = true
		}
	}
	if len(scan.Custom) > 32 {
		return fmt.Errorf("agent_scan.custom: 条目数不得超过 32")
	}
	if len(scan.Services) > 32 {
		return fmt.Errorf("agent_scan.services: 条目数不得超过 32")
	}
	names := map[string]bool{}
	for i, c := range scan.Custom {
		if !validLabelCfg(c.Name, 128) {
			return fmt.Errorf("agent_scan.custom[%d]: name 非空、无空白且 ≤128 字节", i)
		}
		if names[c.Name] {
			return fmt.Errorf("agent_scan.custom[%d]: name %q 重复（custom 与 services 不得重名）", i, c.Name)
		}
		names[c.Name] = true
		typ := c.Type
		if typ == "" {
			typ = "cli"
		}
		if typ != "cli" {
			return fmt.Errorf("agent_scan.custom[%d]: type 须为 cli（服务型 agent 走 agent_scan.services），got %q", i, c.Type)
		}
		cmd := ExpandHome(strings.TrimSpace(c.Command))
		if !filepath.IsAbs(cmd) {
			return fmt.Errorf("agent_scan.custom[%d] (%s): command 必须为绝对路径，got %q", i, c.Name, c.Command)
		}
		vf := strings.TrimSpace(c.VersionFlag)
		if vf == "" {
			vf = "--version"
		}
		if len(vf) > 64 {
			return fmt.Errorf("agent_scan.custom[%d]: version_flag 过长（≤64）", i)
		}
		for _, r := range vf {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("agent_scan.custom[%d]: version_flag 含控制字符", i)
			}
		}
		scan.Custom[i].Command = cmd
		scan.Custom[i].VersionFlag = vf
		scan.Custom[i].Type = typ
		// 会话目录（M1b-c last_activity 辅证）：~ 展开即可，不做存在性校验——
		// 目录此刻缺失是合法状态（last_activity 如实报 null）。
		scan.Custom[i].SessionDir = ExpandHome(strings.TrimSpace(c.SessionDir))
	}
	for i, s := range scan.Services {
		if !validLabelCfg(s.Name, 128) {
			return fmt.Errorf("agent_scan.services[%d]: name 非空、无空白且 ≤128 字节", i)
		}
		if names[s.Name] {
			return fmt.Errorf("agent_scan.services[%d]: name %q 与 custom 重名", i, s.Name)
		}
		names[s.Name] = true
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("agent_scan.services[%d] (%s): port 须在 1-65535，got %d", i, s.Name, s.Port)
		}
	}
	// R10-#7：合并去重后的 agent 总数不得超过 64——与 console 心跳 agents 数组
	// 上限（registry maxAgents=64）一致，超限的扫描结果上报时会被 400 拒绝，
	// 必须在启动时即拒绝而非等心跳失败。同名时声明覆盖 PATH 探测，合并按名去重。
	merged := map[string]bool{}
	for _, n := range scan.KnownList() {
		merged[n] = true
	}
	for _, c := range scan.Custom {
		merged[c.Name] = true
	}
	for _, s := range scan.Services {
		merged[s.Name] = true
	}
	if len(merged) > maxAgentScanTotal {
		return fmt.Errorf("agent_scan: 合并去重后 agent 总数为 %d，不得超过 %d（与心跳 agents 数组上限一致）；请精简 known/custom/services", len(merged), maxAgentScanTotal)
	}
	return nil
}

// LoadAgent 载入代理配置。path 为空或文件不存在时返回纯缺省配置。
func LoadAgent(path string) (*Agent, error) {
	cfg := &Agent{
		Role:             "node",
		CollectIntervalS: 15,
		DiskMount:        DefaultDiskMount(),
		StateFile:        DefaultStatePath(),
		DockerBin:        "docker",
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
	// TLS 固定验证（DESIGN §7-3）：console_url 强制 https（R10-#1，明文 http
	// 一律拒绝），且 https 必须带 ca_cert 或 fingerprint 之一；不提供跳过验证
	// 的选项，配置缺失直接拒绝启动。
	cfg.CACert = ExpandHome(strings.TrimSpace(cfg.CACert))
	if cfg.Fingerprint != "" {
		fp, err := NormalizeFingerprint(cfg.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("agent.fingerprint: %w", err)
		}
		cfg.Fingerprint = fp
	}
	if cfg.ConsoleURL != "" {
		if u, err := url.Parse(strings.TrimSpace(cfg.ConsoleURL)); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("agent.console_url 必须为 https:// 地址（明文 http 已不再支持，DESIGN §7-3），got %q", cfg.ConsoleURL)
		}
		if cfg.CACert == "" && cfg.Fingerprint == "" {
			return nil, fmt.Errorf("agent.console_url 为 https 时必须配置 ca_cert 或 fingerprint 之一（不提供跳过验证的选项；指纹由控制台 `meshconsole pki` 输出）")
		}
	}
	// docker CLI 路径（R10-#4）：缺省 "docker"，支持 ~ 展开；空串归缺省。
	if cfg.DockerBin = ExpandHome(strings.TrimSpace(cfg.DockerBin)); cfg.DockerBin == "" {
		cfg.DockerBin = "docker"
	}
	if err := validateAgentServices(cfg.Services); err != nil {
		return nil, err
	}
	if err := validateAgentScan(cfg.AgentScan); err != nil {
		return nil, err
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
