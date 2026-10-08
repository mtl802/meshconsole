# SPEC-M1b-c2 · 账号体系 + 公网 MCP + 全面中文化

版本：v4 定稿（2026-10-08；v3 经 codex 评审修订 7 项后定稿） · 前置：M1b-c 交付后执行

## 0. DESIGN 同步

§7-4 重写「公网面板例外」：三层认证（面板=账号会话 / MCP=Bearer token / agent=来源收敛+token）、限流与并发配额、证书信任与 pin 更新流程、回滚路径。§8 补账号/会话/token 条款。删除"公网无暴露端口"绝对表述，改为"公网暴露须满足本节全部条件"。

## 1. 账号体系（migration v5）

- `users`（id/username/password_hash bcrypt cost 10/**enabled**/created_at）+ `sessions`（token_hash/user_id/expires_at）+ `api_tokens`（token_hash/user_id/description/expires_at/created_at，token_hash 建索引）。
- CLI：`user add/passwd/disable/enable`（disable 最后一个启用用户时拒绝并提示）；`token create/list/revoke`。CLI 先跑 migration 再操作（bootstrap 不依赖既有数据）；stdio MCP 只读打开、不迁移。
- **认证在所有网络形态始终开启**（含回环 listen）——不设"内网免登录"路径；非回环绑定是额外启动门槛（§4），不是认证开关。
- 会话：登录发 256bit token，Cookie `mc_session`（Path=/、host-only 不设 Domain、HttpOnly、Secure、SameSite=Lax）；每次认证请求滑动续期；**绝对期限 30 天**（续期不越过）；改密/disable 用户 → 撤销该用户全部 session 与 api_tokens。
- 登录防護：per-IP 5 次失败/分 → 429（滑动窗 cap 4096）；**全局并发 bcrypt ≤2**（超出排队或 429）；未知用户名也执行 dummy bcrypt（防时序枚举）。
- 面板全部资源挂会话守卫（未登录 → 302 /login）；豁免清单显式：`GET /login`、静态资源（css/js/字体）、`GET /healthz`。**匿名可及仅此三项**，其余（含 /api/panel/*）未登录一律 302，不得泄露业务数据与工具元数据。

## 2. MCP 公网 HTTP（internal/mcpserver）

- `POST /mcp` Streamable HTTP。实现选型：优先官方 go-sdk（锁定 go.mod 版本，README/REVIEW 记录版本与兼容客户端声明）；SDK 不可用 → **完整手写 Streamable HTTP（JSON 同步响应模式，不用 SSE 流）**——tools/call 均为短查询，JSON 模式规避 SSE 与 WriteTimeout 30s 的冲突。
- 认证：`Authorization: Bearer <api_token>`，哈希查表比对；失败 401（响应体中文化，不泄露工具列表）。
- **MCP 独立并发闸**：semaphore 上限 4，超出 429——与 overview 的 8 并行，防挤占 agent 心跳。
- 工具集：五只读 + list_agent_tasks（M1b-c）；**description 全中文**；JSON-RPC error message 中文化（协议 code 不变）。stdio 模式保留。

## 3. 全面中文化

面板 UI（导航/状态/空态/表单/登录页）+ 用户可见 API 错误 + MCP description/error。代码注释与结构化日志键保持英文。

## 4. 公网模式启动门槛（internal/config + main）

- 判定：**实际绑定地址**（listener addr 解析）非 {127/8, ::1（含 IPv4-mapped 规范化）} → 公网模式。0.0.0.0/::/空 host 属非回环。
- 公网模式启动检查（任一失败拒绝启动，错误指明原因）：存在 ≥1 个 enabled 用户；config 文件普通文件且 0600 且属运行用户（Windows 用 ACL 等价约束）；panel_allowed_hosts 合法（显式空数组=仅接受回环 Host；非法项报错）。
- **数据文件权限**：data 目录 0700、DB/WAL/SHM 0600（store.Open 现建 0755 需修）。

## 5. agent API 来源收敛（internal/registry）

- `/api/agent/*`：RemoteAddr ∈ {127/8, ::1, 10/8, 172.16/12, 192.168/16, 100.64/10}（IPv4-mapped 先规范化）放行，公网 403；`agent_allowed_cidrs` 覆写（缺省=上述默认；显式空=仅回环）。**网络过滤非身份认证**，token 校验独立保留；不读任何转发头。

## 6. SAN 与证书（internal/pki）

- `tls_extra_sans: []`：任何 PKI 写入前**统一校验**（IP literal → IP SAN；合法 DNS → DNS SAN；拒绝 URL/端口/CIDR/空值；规范化去重；非法显式报错——修复现有 ExtraIPs 静默跳过）。部署值 ["1.13.158.180"]。

## 7. listen 与资源

`listen: "0.0.0.0:7700"`；overview semaphore 8 + MCP semaphore 4；全链 no-store/nosniff。

## 8. 部署 runbook（DELIVERY）

1. `user add lunge`（交互密码）→ `token create` 记录明文一次
2. config：panel_allowed_hosts 加 100.64.0.3/1.13.158.180；tls_extra_sans 加公网 IP；listen 改绑
3. 重签证书（备份旧对）→ 重启 console
4. **客户端更新顺序**：ca_cert 模式 agent 无需动作；fingerprint 模式 agent 先拿新指纹更新配置再切流量（当前部署均为 ca_cert，核对即可）；浏览器首次访问核对指纹/导入 CA（带外），验收禁用 -k
5. 验收矩阵：未登录 302 /login→登录→面板中文/错密码 429/登出失效/MCP 无 token 401/有 token 中文 tools/mesh 心跳不回退/公网匿名仅得 login+静态/公网 agent API 403
6. 防火墙放行 TCP 7700（伦哥）；回滚：撤防火墙规则 + 恢复 .bak 证书 + listen 改回 100.64.0.3:7700
