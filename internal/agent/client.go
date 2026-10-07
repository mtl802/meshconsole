package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mtl802/meshconsole/internal/agent/collect"
)

const httpTimeout = 10 * time.Second

// maxRedirects 为单次请求允许的最大重定向次数（防重定向环）。
const maxRedirects = 3

// NewHTTPClient 构造上报客户端。console_url 强制 https://（R10-#1：明文 http
// 一律拒绝——启动（run）与注册（register）共用本入口，不存在明文路径）：
//   - 配置 ca_cert：RootCAs 指向该 CA，标准证书链 + 有效期 + 主机名验证；
//   - 配置 fingerprint：在完整 x509 验证之上叠加叶子证书 SHA-256 指纹比对；
//   - 两者皆空返回错误（不提供跳过验证的选项，DESIGN §7-3）。
func NewHTTPClient(consoleURL, caCertPath, fingerprint string) (*http.Client, error) {
	u, err := url.Parse(strings.TrimSpace(consoleURL))
	if err != nil {
		return nil, fmt.Errorf("console_url 必须为 https:// 地址（解析失败: %v），got %q", err, consoleURL)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("console_url 必须为 https:// 地址（明文 http 已不再支持，DESIGN §7-3；本机调试用 pki 证书的 localhost SAN 即可），got %q", consoleURL)
	}
	if caCertPath == "" && fingerprint == "" {
		return nil, errors.New("https 上报必须配置 ca_cert 或 fingerprint 之一（不提供跳过验证的选项；指纹由控制台 `meshconsole pki` 输出）")
	}
	tlsCfg, err := pinnedTLSConfig(u.Hostname(), caCertPath, fingerprint)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:   httpTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 重定向一律拒绝向 http 降级（R10-#1）：即便 console 被劫持返回
			// 302，凭据也不会发往明文通道。
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect downgrade to %s://", req.URL.Scheme)
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			return nil
		},
	}, nil
}

// pinnedTLSConfig 构造固定验证的 TLS 配置（host 为验证用主机名/IP）：
//   - 配置 ca_cert：RootCAs 指向该 CA，crypto/tls 原生执行证书链 + 有效期 +
//     主机名验证；如同时配置指纹则再加指纹复核；
//   - 仅配置 fingerprint：InsecureSkipVerify 仅为绕过系统信任库（自签 CA 不在
//     其中），x509 语义一项不豁免——VerifyPeerCertificate 以服务端握手下发的
//     证书链重建完整验证（证书链 + 有效期 + 主机名全部执行，R10-#1），信任锚
//     取下发链末端证书，信任关系由指纹比对闭环；再叠加叶子指纹比对。
func pinnedTLSConfig(host, caCertPath, fingerprint string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if caCertPath != "" {
		pemBytes, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("read ca_cert %s: %w", caCertPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("ca_cert %s 中没有可用的 PEM 证书", caCertPath)
		}
		cfg.RootCAs = pool
	}
	if fingerprint != "" {
		// 兼容 register 命令行直接传入的冒号/大小写写法（配置路径已在 LoadAgent 归一化）。
		fp, err := normalizeFingerprint(fingerprint)
		if err != nil {
			return nil, err
		}
		if caCertPath == "" {
			cfg.InsecureSkipVerify = true
			cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				leaf, err := verifyServedChain(rawCerts, host)
				if err != nil {
					return err
				}
				return matchFingerprint(leaf, fp)
			}
			return cfg, nil
		}
		// ca_cert + 指纹双因素：标准链验证之外叠加指纹复核（任一失败即拒绝）。
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server sent no certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse server certificate: %w", err)
			}
			return matchFingerprint(leaf, fp)
		}
	}
	return cfg, nil
}

// verifyServedChain 以服务端握手时下发的证书链执行完整 x509 验证（R10-#1）：
// 下发链末端证书作信任锚（必须自签——Go 对「叶子自身在信任池」有直通捷径，
// 非自签锚会让链验证形同虚设），中间证书进 Intermediates，验证签名链、有效期
// （NotBefore/NotAfter）、ExtKeyUsage 与主机名——与标准验证同一套 x509 语义，
// 唯一差异是信任锚取自下发链而非本地信任库。锚定闭环靠调用方叠加的指纹比对：
// 冒充者即使伪造自签链也无法让叶子指纹与固定值一致，更无法持有对应私钥完成握手。
func verifyServedChain(rawCerts [][]byte, host string) (*x509.Certificate, error) {
	if len(rawCerts) == 0 {
		return nil, errors.New("server sent no certificate")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, fmt.Errorf("parse server certificate: %w", err)
		}
		certs = append(certs, c)
	}
	anchor := certs[len(certs)-1]
	if !bytes.Equal(anchor.RawIssuer, anchor.RawSubject) {
		return nil, errors.New("incomplete server certificate chain: verification requires the full chain ending at a self-signed root")
	}
	// self-issued（Issuer 名单相同）≠ self-signed：同名单异密钥的伪造锚一旦
	// 入信任池，Go 对「叶子即在信任池」的直通捷径会架空签名链验证（R13-#1）。
	// 入池前用锚自身公钥验证其 TBS 签名，签名不符即拒绝。
	if err := anchor.CheckSignature(anchor.SignatureAlgorithm, anchor.RawTBSCertificate, anchor.Signature); err != nil {
		return nil, fmt.Errorf("server certificate chain ends at an anchor that is not genuinely self-signed: %w", err)
	}
	leaf := certs[0]
	roots := x509.NewCertPool()
	roots.AddCert(anchor)
	opts := x509.VerifyOptions{
		DNSName:   host,
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if len(certs) > 2 {
		inters := x509.NewCertPool()
		for _, c := range certs[1 : len(certs)-1] {
			inters.AddCert(c)
		}
		opts.Intermediates = inters
	}
	if _, err := leaf.Verify(opts); err != nil {
		return nil, fmt.Errorf("server certificate verification (chain/validity/hostname): %w", err)
	}
	return leaf, nil
}

// matchFingerprint 常量时间比对叶子证书 SHA-256 指纹；不输出任何一方指纹值
// （日志不留证书材料，SPEC 验收 3）。
func matchFingerprint(leaf *x509.Certificate, fp string) error {
	sum := sha256.Sum256(leaf.Raw)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(fp)) != 1 {
		return errors.New("server certificate fingerprint mismatch")
	}
	return nil
}

func normalizeFingerprint(fp string) (string, error) {
	s := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(fp))
	if len(s) != 64 {
		return "", errors.New("fingerprint 须为服务端证书 SHA-256 指纹的 64 位 hex（meshconsole pki 会输出）")
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			// 不回显原值：错误信息不留任何输入材料片段（R13-#3，与 config 侧同口径）。
			return "", errors.New("fingerprint 含非 hex 字符（须为 64 位 hex，`meshconsole pki` 输出）")
		}
	}
	return s, nil
}

// Register 向控制台申请节点凭据并返回待持久化的 state。
// client 由调用方经 NewHTTPClient 构造（https 时已带固定验证）。
func Register(ctx context.Context, client *http.Client, consoleURL, regToken, name, role, osName, arch string) (*State, error) {
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
// M1b-a 扩展：心跳体按需携带 services（配置声明了服务清单即每次上报，
// 含显式空列表以驱动 console 侧 stale）与 agents（首轮扫描完成后上报）。
func reportOnce(ctx context.Context, client *http.Client, consoleURL, nodeToken, name, version string, c *collect.Collector, extra *heartbeatExtras) error {
	snap := c.Collect()
	body := map[string]any{
		"node":           name,
		"agent_version":  version,
		"metrics":        snap,
		"collect_errors": snap.Errors,
	}
	if extra != nil {
		extra.apply(ctx, body)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode heartbeat: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, consoleURL+"/api/agent/heartbeat", bytes.NewReader(payload))
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
