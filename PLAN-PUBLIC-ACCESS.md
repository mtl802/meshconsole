# PLAN-PUBLIC-ACCESS · 面板公网访问改造（m1b-c2 预研）

需求（伦哥 2026-10-08 13:53）：通过 `https://1.13.158.180:7700` 从公网访问面板，不依赖内网。
背景：当前 console 绑 100.64.0.3:7700（mesh），面板 Host 白名单仅 localhost 系，无认证。

## 改动清单（草案 v1）

### A. Basic Auth 认证（internal/panel）

- config 新增段：
  ```yaml
  panel_auth:
    username: "lunge"          # 必填（启用认证时）
    password: "<openssl rand>" # 明文存 config（600），不入库不入 git；v1 不做 hash
  ```
- guard 链插入 Basic Auth middleware：解析 `Authorization: Basic`，constant-time 比对（subtle.ConstantTimeCompare 双字段）；失败 401 + `WWW-Authenticate: Basic realm="meshconsole"`；未配置 panel_auth 段 → 认证关闭（内网形态向后兼容）。
- 只挂面板路由（/ 与 /api/panel/*）；agent API 不动（已有 token 体系）。

### B. Host 白名单 config 化（internal/panel + config）

- config 新增 `panel_allowed_hosts: ["localhost", "127.0.0.1", "::1"]`（默认值 = 现状，向后兼容）。
- hostAllowed 的硬编码 switch 改为查 config 名单（解析严格性逻辑不变：port 值域/方括号 IPv6/userinfo/path 全保留）。
- 本次部署值：追加 `100.64.0.3`、`1.13.158.180`。

### C. 证书 SAN 多值（internal/pki + config）

- 现状：`tailnet_ip` 单值 → SAN 单 mesh IP。
- 改：config 新增 `tls_extra_sans: []`（数组），pki 生成时并入 SAN（tailnet_ip 保留向后兼容）。
- 本次部署值：`tls_extra_sans: ["1.13.158.180"]`，重签 server cert（CA 不动，agent 链验证不受影响）。

### D. listen 改绑（config + 部署）

- console.yaml `listen: "0.0.0.0:7700"`（mesh 直连 + 公网同端口共存）。
- 安全边界：公网可达性完全依赖腾讯云防火墙（仅放行 TCP 7700）；文档注明"绑定 0.0.0.0 的前提是主机防火墙收敛"。
- agent 配置不改（cloud-agent 继续走 100.64.0.3）。

### E. 交付物

- SPEC-M1b-c2.md + 代码 + 单测（auth 通过/失败/未配置、allowed_hosts 变体、SAN 多值）+ 部署（服务器配置更新 + 防火墙放行由伦哥操作）+ 验收（公网 curl 认证通过/失败、mesh 直连不回退、人眼）。

## 待讨论问题（请 codex 重点评估）

1. Basic Auth 密码 config 明文（600）vs bcrypt hash 权衡——v1 明文是否可接受？
2. 认证失败是否需要节流/延迟（公网 brute force 面）？v1 范围？
3. listen 0.0.0.0 依赖外部防火墙 vs 多地址分别 net.Listen——哪个更符合 DESIGN §7 精神？
4. 公网暴露后 agent register/heartbeat 端点同端口可达——现有 token 体系是否足够，是否需要对公网来源额外收敛？
5. SAN 多值的 config 形态（tls_extra_sans 数组 vs 重构 tailnet_ips）哪个迁移成本更低？
6. Basic Auth 与既有 guard（Host/Origin 校验）的顺序——先 Host 后 Auth 还是先 Auth？
7. 遗漏风险清单。
