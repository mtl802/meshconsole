// Package pki 实现 MeshConsole 的 PKI 生命周期（DESIGN §7-3）：
// 首次部署生成持久化 CA + 服务端证书（私钥 0600），后续启动仅加载、不重新生成。
//
// 幂等口径（SPEC-M1b-a 验收 1）：
//   - CA（ca.key/ca.crt）已存在 → 永不重新生成，仅校验可解析；
//   - 服务端私钥（server.key）已存在 → 永不覆盖（验收：二次运行不覆盖已有私钥）；
//   - 服务端证书（server.crt）已存在且链可验证、未过期、SAN 覆盖当前配置 → 保持；
//     否则（首次生成 / SAN 变更如 tailnet_ip 调整 / 已过期）→ 用既有 CA + 既有私钥
//     重签证书——私钥仍不动。
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// CA 有效期 10 年、服务端证书 5 年：自托管内网 CA，不适用公共 CA 有效期基线；
	// 轮换流程（新旧 CA 双信任）按 DESIGN §7-3 在部署层执行。
	caValidity     = 10 * 365 * 24 * time.Hour
	serverValidity = 5 * 365 * 24 * time.Hour

	CAKeyFile      = "ca.key"
	CACertFile     = "ca.crt"
	ServerKeyFile  = "server.key"
	ServerCertFile = "server.crt"
)

// Config 为生成参数；Dir 为 pki 输出目录（console 配置 pki_dir）。
type Config struct {
	Dir string
	// TailnetIP 为控制台所在节点的 Tailnet IP（可选，进 SAN）。
	TailnetIP string
	// ExtraDNS / ExtraIPs 为追加 SAN（可选）。
	ExtraDNS []string
	ExtraIPs []string
}

// Result 汇报一次 Ensure 的动作与产物位置。
type Result struct {
	// CAGenerated 为本次新生成了 CA（false = 复用既有 CA）。
	CAGenerated bool
	// ServerCertRenewed 为本次（重）签发了服务端证书；false = 既有证书保持不动。
	ServerCertRenewed bool
	// Fingerprint 为服务端证书的 SHA-256 指纹（hex 小写，无冒号）——
	// agent 配置 fingerprint 项直接粘贴该值。
	Fingerprint    string
	CACertPath     string
	ServerCertPath string
	ServerKeyPath  string
}

// Ensure 按 Config 检查并按需生成 CA 与服务端证书，返回动作与指纹。
// 所有文件已就绪且证书仍匹配时为零改动（幂等）。
func Ensure(cfg Config) (*Result, error) {
	if cfg.Dir == "" {
		return nil, errors.New("pki 目录为空")
	}
	// R17-#1：tailnet_ip 非法不再静默忽略（旧实现 ParseIP 失败时跳过该 SAN 仍报
	// 成功——证书缺配置要求的 SAN 而部署方不知情）。配置了就必须合法。
	if cfg.TailnetIP != "" && net.ParseIP(cfg.TailnetIP) == nil {
		return nil, fmt.Errorf("tailnet_ip %q 不是合法 IP 地址（期望如 100.64.0.1）；修正配置后重试", cfg.TailnetIP)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create pki dir: %w", err)
	}
	caKeyPath := filepath.Join(cfg.Dir, CAKeyFile)
	caCertPath := filepath.Join(cfg.Dir, CACertFile)
	srvKeyPath := filepath.Join(cfg.Dir, ServerKeyFile)
	srvCertPath := filepath.Join(cfg.Dir, ServerCertFile)

	res := &Result{
		CACertPath:     caCertPath,
		ServerCertPath: srvCertPath,
		ServerKeyPath:  srvKeyPath,
	}

	// ---- CA：ca.crt 与 ca.key 必须同生同在（R10-#2）——
	//   - 两文件均缺失 → 新生成；
	//   - 任一单独存在 → 不完整（生成中途断电/人工误删半个 CA），拒绝生成并
	//     提示人工处置：此刻重新生成会覆盖残留文件，等于作废可能已分发的信任锚；
	//   - 两文件都在 → 解析校验后复用（存在但损坏同样报错，绝不静默覆盖），
	//     且私钥文件权限必须 0600。
	caCert, caKey, err := loadCA(caCertPath, caKeyPath)
	switch {
	case err == nil:
		// 复用前校验证书与私钥配对（R11-D）：错配的 CA 会让后续重签的证书
		// 无法被 ca.crt 验证，却仍报成功——公钥不一致直接拒绝。
		if err := requireCAPair(caCert, caKey); err != nil {
			return nil, err
		}
		// 复用前校验有效期（R13-#2）：过期/未生效的 CA 重签出的服务端证书
		// 无法被任何客户端验证，却会照常报成功——拒绝复用并提示重新生成。
		if err := requireCAValidity(caCert); err != nil {
			return nil, err
		}
		if err := requireKeyPerms(caKeyPath); err != nil {
			return nil, err
		}
	case errors.Is(err, errNoCA):
		caCert, caKey, err = generateCA()
		if err != nil {
			return nil, err
		}
		res.CAGenerated = true
		if err := writeKey(caKeyPath, caKey); err != nil {
			return nil, err
		}
		if err := writeCert(caCertPath, caCert); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	// ---- 服务端私钥：存在即复用，永不覆盖（SPEC 验收 1）；复用时权限必须 0600。
	srvKey, err := loadKey(srvKeyPath)
	switch {
	case err == nil:
		if err := requireKeyPerms(srvKeyPath); err != nil {
			return nil, err
		}
	case errors.Is(err, errNoKey):
		srvKey, err = generateKey()
		if err != nil {
			return nil, err
		}
		if err := writeKey(srvKeyPath, srvKey); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	// ---- 服务端证书：匹配当前配置则保持，否则重签（复用上面确定的 CA 与私钥）。
	renew, err := needsRenewal(srvCertPath, caCert, srvKey, cfg)
	if err != nil {
		return nil, err
	}
	if renew {
		leaf, err := issueServerCert(caCert, caKey, srvKey, cfg)
		if err != nil {
			return nil, err
		}
		if err := writeCert(srvCertPath, leaf); err != nil {
			return nil, err
		}
		res.ServerCertRenewed = true
	}

	certPEM, err := os.ReadFile(srvCertPath)
	if err != nil {
		return nil, err
	}
	fp, err := FingerprintPEM(certPEM)
	if err != nil {
		return nil, err
	}
	res.Fingerprint = fp
	return res, nil
}

// FingerprintPEM 计算第一个 PEM 证书块的 SHA-256 指纹（hex 小写，无冒号）。
func FingerprintPEM(certPEM []byte) (string, error) {
	der, _ := pem.Decode(certPEM)
	if der == nil {
		return "", errors.New("no PEM certificate block")
	}
	sum := sha256.Sum256(der.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// ServerTLSConfig 加载服务端证书构造 TLS 配置（console 启动仅加载，不生成；
// 证书缺失/损坏时启动失败，提示先跑 pki 生成）。caCertPath 非空且确为服务端
// 证书的签发方时，把 CA 追加进下发链——仅配置指纹的 agent 需从下发链取得信任
// 锚执行完整 x509 验证（R10-#1）。CA 文件缺失或与证书不匹配（外部证书管理、
// 证书文件已含全链）时保持原样：配 ca_cert 的 agent 凭本地信任库仍可验证。
func ServerTLSConfig(certPath, keyPath, caCertPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server cert (先运行 `meshconsole pki` 生成 %s/%s): %w", certPath, keyPath, err)
	}
	// 启动加载入口同样校验私钥权限 0600（R15-#3，与 R10-#2 生成侧同口径）：
	// Ensure 写入即 0600，此处拦人工放宽后的复用——放宽时启动报错并给 chmod 提示。
	if err := requireKeyPerms(keyPath); err != nil {
		return nil, err
	}
	if caCertPath != "" && len(cert.Certificate) == 1 {
		if leaf, perr := x509.ParseCertificate(cert.Certificate[0]); perr == nil && !leaf.IsCA {
			if caPEM, rerr := os.ReadFile(caCertPath); rerr == nil {
				if blk, _ := pem.Decode(caPEM); blk != nil {
					if caCert, perr := x509.ParseCertificate(blk.Bytes); perr == nil && leaf.CheckSignatureFrom(caCert) == nil {
						cert.Certificate = append(cert.Certificate, caCert.Raw)
					}
				}
			}
		}
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

var errNoCA = errors.New("ca not generated yet")
var errNoKey = errors.New("key not generated yet")
var errKeyPerms = errors.New("private key file permission must be 0600")

// requireCAPair 校验 CA 证书与私钥配对（R11-D）：ca.crt 的公钥必须就是
// ca.key 的公钥——两者来自不同 CA 时，用它重签的服务端证书无法被 ca.crt
// 验证（CheckSignatureFrom 失败）却仍会报成功，必须在此拒绝。
func requireCAPair(caCert *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	pub, ok := caCert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.X.Cmp(caKey.PublicKey.X) != 0 || pub.Y.Cmp(caKey.PublicKey.Y) != 0 {
		return fmt.Errorf("CA 证书与私钥不配对：%s 与 %s 来自不同密钥对，请人工处置（补齐正确配对或整体移除后重新生成）",
			CACertFile, CAKeyFile)
	}
	return nil
}

// requireCAValidity 复用既有 CA 前校验有效期（R13-#2）：已过期或尚未生效的
// CA 签出的服务端证书在客户端验证必然失败（有效期链不上），却会照常报成功——
// 拒绝复用，提示人工整体移除后重新 make pki（作废既有指纹固定，agent 侧需
// 同步更新信任材料）。
func requireCAValidity(caCert *x509.Certificate) error {
	now := time.Now()
	switch {
	case now.Before(caCert.NotBefore):
		return fmt.Errorf("CA 证书尚未生效（NotBefore %s，当前 %s），拒绝复用：请人工整体移除 %s/%s 后重新 make pki",
			caCert.NotBefore.Format(time.RFC3339), now.Format(time.RFC3339), CACertFile, CAKeyFile)
	case now.After(caCert.NotAfter):
		return fmt.Errorf("CA 证书已过期（NotAfter %s，当前 %s），拒绝复用：请人工整体移除 %s/%s 后重新 make pki 生成新 CA（各 agent 的 ca_cert/指纹需同步更新）",
			caCert.NotAfter.Format(time.RFC3339), now.Format(time.RFC3339), CACertFile, CAKeyFile)
	}
	return nil
}

// requireKeyPerms 复用既有私钥时校验权限 0600（R10-#2）：写入路径由 writeKey
// 保证，此处拦人为放松的权限——私钥全局可读等于把 CA/节点身份拱手让人。
// Windows 的 Mode().Perm() 不反映真实 ACL（恒 0666），跳过该项检查。
func requireKeyPerms(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if p := fi.Mode().Perm(); p != 0o600 {
		return fmt.Errorf("%w：%s 权限为 %o（chmod 600 %s 后重试）", errKeyPerms, path, p, path)
	}
	return nil
}

// loadCA 读取 CA 证书与私钥。errNoCA 仅表示两文件均不存在（可安全新生成）；
// 任一文件单独存在而另一个缺失返回不完整错误（拒绝生成，人工处置，R10-#2）；
// 其余读取/解析错误原样返回。
func loadCA(caCertPath, caKeyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, certErr := os.ReadFile(caCertPath)
	keyPEM, keyErr := os.ReadFile(caKeyPath)
	certMissing := os.IsNotExist(certErr)
	keyMissing := os.IsNotExist(keyErr)
	switch {
	case certMissing && keyMissing:
		return nil, nil, errNoCA
	case certMissing || keyMissing:
		missing, kept := CACertFile, CAKeyFile
		if keyMissing {
			missing, kept = CAKeyFile, CACertFile
		}
		return nil, nil, fmt.Errorf("CA 不完整：%s 缺失但 %s 存在，拒绝生成（重新生成会覆盖既有 CA，作废全部客户端固定）：%s 目录需人工处置（补齐文件或整体移除后重新生成）",
			missing, kept, filepath.Dir(caCertPath))
	case certErr != nil:
		return nil, nil, certErr
	case keyErr != nil:
		return nil, nil, keyErr
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return nil, nil, fmt.Errorf("parse %s: no PEM block", caCertPath)
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", caCertPath, err)
	}
	key, err := parseKey(keyPEM, caKeyPath)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("%s 不是 CA 证书（BasicConstraints 缺失）", caCertPath)
	}
	return cert, key, nil
}

func parseKey(keyPEM []byte, path string) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return nil, fmt.Errorf("parse %s: no PEM block", path)
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return key, nil
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	keyPEM, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, errNoKey
	}
	if err != nil {
		return nil, err
	}
	return parseKey(keyPEM, path)
}

func generateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := generateKey()
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "MeshConsole Root CA", Organization: []string{"meshconsole"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func generateKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return key, nil
}

// needsRenewal 判定既有 server.crt 是否仍可用：链可验证 + 有效期内且已生效 +
// EKU 可用于 TLS 服务端 + SAN 覆盖当前配置要求 + 公钥与既有私钥一致 → 保持；
// 任一不满足 → 重签（复用既有 CA 与私钥，R15-#2：未生效/缺 ServerAuth EKU 的
// 证书客户端验证必然失败，不得复用报成功）。
func needsRenewal(srvCertPath string, caCert *x509.Certificate, srvKey *ecdsa.PrivateKey, cfg Config) (bool, error) {
	certPEM, err := os.ReadFile(srvCertPath)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return true, nil
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return true, nil
	}
	now := time.Now()
	if now.After(cert.NotAfter) {
		return true, nil
	}
	// 尚未生效同样不可用（R15-#2）：NotBefore 在未来时客户端验证必然失败。
	if now.Before(cert.NotBefore) {
		return true, nil
	}
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		return true, nil
	}
	// EKU 检查（R15-#2）：既不含 ServerAuth 也不含 ExtKeyUsageAny 的证书
	// 无法用于 TLS 服务端，重签换上带 ServerAuth 的新证书。
	hasServerEKU := false
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth || eku == x509.ExtKeyUsageAny {
			hasServerEKU = true
			break
		}
	}
	if !hasServerEKU {
		return true, nil
	}
	// SAN 覆盖检查：配置要求的 DNS/IP 必须全部已在证书中。
	wantDNS, wantIPs := sans(cfg)
	haveDNS := map[string]bool{}
	for _, d := range cert.DNSNames {
		haveDNS[d] = true
	}
	for _, d := range wantDNS {
		if !haveDNS[d] {
			return true, nil
		}
	}
	haveIP := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		haveIP[ip.String()] = true
	}
	for _, ip := range wantIPs {
		if !haveIP[ip.String()] {
			return true, nil
		}
	}
	// 公钥必须仍是这份私钥对应的公钥（防历史上的私钥/证书错配组合）。
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.X.Cmp(srvKey.PublicKey.X) != 0 || pub.Y.Cmp(srvKey.PublicKey.Y) != 0 {
		return true, nil
	}
	return false, nil
}

// sans 汇总要求的 SAN：DNS 恒含 localhost 与本机主机名；IP 恒含 127.0.0.1 与 ::1，
// 配置了 tailnet_ip 再追加（SPEC：SAN 必须覆盖 localhost、127.0.0.1、tailnet IP、主机名）。
func sans(cfg Config) (dns []string, ips []net.IP) {
	dns = []string{"localhost"}
	if h, err := os.Hostname(); err == nil && h != "" {
		dns = append(dns, h)
	}
	dns = append(dns, cfg.ExtraDNS...)
	ips = append(ips, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	if cfg.TailnetIP != "" {
		if ip := net.ParseIP(cfg.TailnetIP); ip != nil {
			ips = append(ips, ip)
		}
	}
	for _, s := range cfg.ExtraIPs {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
		}
	}
	return dns, ips
}

func issueServerCert(caCert *x509.Certificate, caKey, srvKey *ecdsa.PrivateKey, cfg Config) (*x509.Certificate, error) {
	dnsNames, ipAddrs := sans(cfg)
	tpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "meshconsole", Organization: []string{"meshconsole"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign server cert: %w", err)
	}
	return x509.ParseCertificate(der)
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		// crypto/rand 失败属系统级故障，直接 panic 与库内其他 rand 用法一致。
		panic(fmt.Sprintf("crypto/rand serial: %v", err))
	}
	return n
}

// writeKey 原子写私钥：唯一临时文件创建即 0600 + rename（与 state 持久化同口径，
// 防止临时窗口出现全局可读私钥，也防固定名符号链接注入）。
func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return atomicWrite(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func writeCert(path string, cert *x509.Certificate) error {
	return atomicWrite(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("create tmp %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}
