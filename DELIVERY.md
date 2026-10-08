# DELIVERY — M1b-c2 交付说明（账号体系 + 公网 MCP + 全面中文化）

> 交付人：zcode · 2026-10-08
> 依据：SPEC-M1b-c2.md v4 定稿（唯一需求源，逐条执行）
> 基线：M1b-c（fix2 后）。状态：**本机可验证项全部自测通过（含二进制级 E2E 验收矩阵 16 项实过）；公网绑定门槛上机实跑、浏览器人眼、云机真 agent 连测列部署上机项**（证据见 REVIEW.md R32 节）。**R33 首审 7 条已修完（m1b-c2-fix，REVIEW.md R34 节）；R35 复审残余 3 条（1 阻塞 + 2 建议）已修完（m1b-c2-fix2，REVIEW.md R36 节），未执行 git commit。**

## 一、交付物清单

| 域 | 内容 |
|---|---|
| 账号体系 | migration **v6**（users/sessions/api_tokens，SPEC 原文 v5 与既占序号冲突顺延，表结构以 SPEC 为准）+ **v7**（`users.password_version`，R33-#1 签发复核）；`internal/auth`（登录/会话/登录防护/API token 认证；**会话签发以单条条件插入复核密码版本与启用态**——改密/禁用在凭据校验后的间隙完成也拒绝签发，旧凭据换不来活会话）；CLI `user add/passwd/disable/enable` + `token create/list/revoke`（交互口令，bootstrap 先迁移；**flag 解析失败与多余位置参数一律报错退出 2 不产生任何变更，R33-#5/R35-#3**） |
| 会话安全 | Cookie `mc_session`（HttpOnly/Secure/SameSite=Lax/host-only）；滑动续期 + 30 天绝对期限；改密/禁用单事务撤销全部 session+api_tokens；最后启用用户保护 |
| 登录防护 | per-IP 5 次/分 429（滑动窗 cap 4096）；全局 bcrypt 并发 ≤2（排队上限 32/等待 5s）；未知用户 dummy bcrypt；失败统一文案防枚举 |
| 公网 MCP | `POST /mcp` 官方 go-sdk v1.8.0（go.mod 锁定未变）`Stateless + JSONResponse` = JSON 同步响应模式（无 SSE）；Bearer api_token 认证（401 中文化不泄露工具）；独立 semaphore 4；body 1MB；**SDK 层英文错误经 HTTP 层响应拦截映射为中文错误体——JSON-RPC 与 text/plain 两条路径全闭合（含空 POST、Last-Event-ID POST、未收录文案兜底通用中文），JSON-RPC code/HTTP 状态码原样保留，不泄露工具清单与内部细节，R33-#3/R35-#2** |
| 公网门槛 | 实际绑定地址判型；≥1 启用用户 + config 权限 + Host 白名单三项检查拒绝启动；**config 检查 unix=0600+属主，Windows=x/sys/windows 实装 ACL/属主校验（仅 当前用户/SYSTEM/Administrators 持有访问权——允许型 ACE 含普通/callback/object/callback-object 四型全解析，拒绝型不参与放行判定，无法判定/解析失败即按失败拒绝启动；属主为当前用户或管理员组；R33-#2 实装、R35-#1 收紧）**；数据文件 data 0700 / DB/WAL/SHM 0600 |
| 来源收敛 | `/api/agent/*` RemoteAddr CIDR 过滤（缺省回环+私网+100.64/10；显式空=仅回环）；公网 403；token 校验独立；不读转发头 |
| 安全头 | 全链 no-store/nosniff：root handler `secureHeaderWriter` 全局包装（含 mux 默认 404/405 与 agent API 全部拒绝路径，R33-#4） |
| SAN | `tls_extra_sans` 统一校验（IP→IP SAN / DNS→DNS SAN / URL·端口·CIDR·空值报错 / 去重），修 ExtraIPs 静默跳过；变更自动重签 |
| 并发三闸 | agent API 64 / overview 8 / MCP 4，相互独立 |
| 中文化 | 面板 UI 全量 + 登录页新增 + API 错误体 + MCP HTTP 错误（含 SDK 层错误映射；渲染层翻译，机器契约保持英文）；代码注释与日志键英文不变 |
| DESIGN | §0 同步：v0.8（§7-4 重写「公网面板例外」三层认证/限流/三闸/门槛/pin 更新/回滚；§8 补账号条款；删除「公网无暴露端口」绝对表述） |

新依赖：`golang.org/x/crypto v0.57.0`（bcrypt）、`golang.org/x/term v0.46.0`（交互口令）；go-sdk v1.8.0 锁定不变（**兼容客户端声明：实现 MCP Streamable HTTP（2025-06-18+）的标准客户端，含 HanaAgent connector**）。

## 二、公网部署 runbook（伦哥操作，按序执行）

0. **前置**：服务器拉取本批产物（`bin/meshconsole-linux-amd64`）替换 /opt/meshconsole/meshconsole；`make pki` 幂等不覆盖私钥。
1. **建账号**：`meshconsole user add lunge`（交互口令两遍）→ `meshconsole token create lunge -desc "HanaAgent Mac"`，**明文只显示一次，当场保存**。
2. **改 config**（/opt/meshconsole/console.yaml，改完 `chmod 600`）：
   - `panel_allowed_hosts: ["localhost", "127.0.0.1", "::1", "100.64.0.3", "1.13.158.180"]`
   - `tls_extra_sans: ["1.13.158.180"]`
   - `listen: "0.0.0.0:7700"`
3. **重签证书**（备份旧对）：`cp pki/server.crt{,.bak} && cp pki/server.key{,.bak}` → `meshconsole pki -config console.yaml`（CA/私钥不动，仅 server.crt 重签带公网 IP SAN）→ 输出新 fingerprint 记录在案。
4. **重启 console**（systemd restart meshconsole）。启动日志核对：`listening addr=0.0.0.0:7700 mode=public`；门槛失败会拒绝启动并指明原因（无启用用户/权限不对/白名单非法）。
5. **客户端更新顺序**：ca_cert 模式 agent **零动作**（当前部署均为 ca_cert，核对即可）；fingerprint 模式 agent 先拿第 3 步新指纹更新配置再切流量；浏览器首次访问经带外核对指纹/导入 CA（**验收一律禁用 -k**）。
6. **防火墙**：腾讯云放行 TCP 7700（伦哥在云控制台操作）。
7. **验收矩阵**（逐项打勾）：
   - [ ] 公网匿名 `https://1.13.158.180:7700/` → 302 /login；`/api/panel/overview` 匿名 → 302（无业务数据）
   - [ ] 登录页中文 → 登录成功 → 面板中文（导航/状态/空态）
   - [ ] 错密码 ×5 → 401；第 6 次 → 429
   - [ ] 登出 → 会话即失效（后退 302 /login）
   - [ ] `/mcp` 无 token → 401（中文，不泄露工具名）；带 token → initialize/tools/list 正常、description 中文
   - [ ] mesh 心跳不回退（cloud-agent 心跳 200，在线状态保持）
   - [ ] 公网匿名仅得 /login + 静态 + /healthz
   - [ ] 公网来源 `POST /api/agent/*` → 403「来源地址不在允许网段」（mesh 内 agent 不受影响）
8. **回滚**：撤防火墙 7700 放行 → `cp pki/server.crt.bak pki/server.crt`（如已重签）→ config `listen: "100.64.0.3:7700"` → 重启。账号体系与来源收敛回环/Tailnet 形态原样生效，回滚不降级安全性。

### 部署上机项（本机沙盒边界，commit 后由 Hana 主会话执行）

- 公网绑定门槛实跑：0.0.0.0:7700 + 无用户 → 拒绝启动；建用户后正常（逻辑已由 `TestPublicModeGate` 四态钉住，上机核验日志 `mode=public`）。
- Windows 上机核验：config ACL/属主检查已代码实装（`config_owner_windows.go`，x/sys/windows；判定核心 `aclVerdict` 与 ACE 分类器 `classifyDACLACE`/`daclAllowTrustees` 单测覆盖全部判定分支——含 callback/object 允许型与无法判定类型拒绝，R35-#1，交叉编译过）——上机验证 `icacls` 裁剪继承后启动放行、残留 Everyone 授权（含藏身 callback/object 允许型）时拒绝启动且报错含 remediation。
- 浏览器人眼：登录页/面板中文化 + 登出按钮（固定右上角）+ Liquid Glass 亮色回归。
- HanaAgent 实配 `https://1.13.158.180:7700/mcp` + Bearer token 连测六工具。

## 〇、修复轮 m1b-c2-fix（R33 全 7 条，2026-10-08）

R33 裁决 2 阻塞 + 3 建议 + 2 条 R31 候选核销全部修完（逐条文件:行号见 REVIEW.md R34）。行为变化：① **会话签发竞态关闭**——migration v7 增 `users.password_version`，改密/禁用递增版本，签发以单条条件插入复核「版本+启用态」，改密/禁用在登录凭据校验后的间隙完成也拒绝签发（并发交错单测钉死）；② **Windows config ACL/属主检查实装**——公网形态下校验仅 当前用户/SYSTEM/Administrators 持有访问权且属主合规，取不到 ACL 即拒绝启动（此前直接放行）；③ MCP SDK 层英文错误（unknown tool 等）映射为中文错误体，JSON-RPC code/id/data 保留；④ 安全头全局包装，mux 404/405 与 agent API 拒绝路径全覆盖 no-store/nosniff；⑤ `token create` flag 解析失败 exit 2 不签发；⑥ 面板轮询：tick 起始独立新鲜度校验 + fetch 10s AbortController 超时 + 防重入（挂起不再冻结失效、不再叠发）；⑦ `list_agent_tasks` 任务清单与截断标记改单只读事务取得。验证：vet/gofmt 零输出、211 用例全绿（+9）、make cross 三平台、node --check + DOM 桩冒烟 + 二进制 E2E 冒烟（登录/改密撤销/重登、MCP 中文错误与六工具、404/401 安全头、agent 兼容）全过。

## 〇′、修复轮 m1b-c2-fix2（R35 全 3 条，2026-10-08）

R35 复审裁决 1 阻塞 + 2 建议全部修完（逐条文件:行号见 REVIEW.md R36）。行为变化：① **Windows ACL「跳过」语义彻底清除**——callback/object/callback-object 允许型 ACE 的 trustee 按 winnt.h 布局解析并纳入可信集判定（此前一概跳过即不可信账户藏身其上可绕过启动检查）；拒绝型只收权不授权、不参与放行判定；无法判定/解析失败的 ACE 类型一律按检查失败拒绝启动（判定核心 `config_acl.go` 平台无关纯函数，5 新用例任意平台覆盖）；② MCP 空 POST、带 Last-Event-ID 的 POST 返回中文错误体（状态码不变），text/plain 未收录英文文案兜底「请求处理失败」不再透出；③ `token create`/`user` 各子命令/`token list`/`token revoke` 收到多余位置参数即退出码 2、零变更（`token create <用户名> extra -expires <未来>` 此前会把 -expires 静默忽略后照签长期 token，已堵死）。验证：vet/gofmt 零输出、218 用例全绿（+7）、make cross 三平台、node --check 过；二进制冒烟（CLI 多余参数 exit 2 零签发、TLS 实例两条 MCP 路径实测中文、R34 已验收面回归不回退）全过。

## 三、验证汇总（2026-10-08，m1b-c2-fix2 后）

- `go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` **218 用例全绿**（13 包：cmd/console 9、agent 14、collect 35、agentdisc 9、auth 10、config 31、headscale 12、mcpserver 11、meshview 7、panel 9、pki 13、registry 27、store 31；两轮修复轮共新增 16：R34 9——签发竞态交错 2、Windows ACL 判定 3、安全头包装 1、token flag 解析 1、MCP SDK 错误中文化 1、任务快照一致性 1；R35→fix2 7——ACE 分类/整链 5、MCP 错误路径 1、多余位置参数 1）；`make cross` 三平台通过；`node --check` 过 + 面板轮询 DOM 桩冒烟过。
- 二进制级 E2E 16 项实过（REVIEW.md R32「验证记录」）+ 修复轮 E2E 冒烟（REVIEW.md R34/R36「验证记录」）：登录/改密撤销/重登、MCP 中文错误与六工具与 list_agent_tasks 契约、404/401 安全头实测、agent 注册心跳兼容；fix2 另实测空 POST/Last-Event-ID POST 中文、CLI 多余位置参数拒绝。
- M1b-c 及更早已验收项无回退（既有用例全部保持通过；SPEC 点名用例原样绿）。

---

# DELIVERY — M1b-c 交付说明（亮色主题 + Agent 任务监测 / agent 中台）

> 交付人：zcode · 2026-10-08
> 依据：SPEC-M1b-c.md（唯一需求源，逐条执行）+ `docs/skills/liquid-glass-frontend` 技能（伦哥点名，theming/liquid-glass 两篇先行）
> 基线：M1b-b（commit da1e1ad）。状态：**本机可验证项全部自测通过（含真机伪装进程 E2E）；cloud-agent 上机上报、真 headscale 连测、浏览器人眼验收列部署上机项**（逐条证据见 REVIEW.md「自测报告 · zcode M1b-c」节）。**R27 首审 5 条已修完（m1b-c-fix，REVIEW.md R28 节）；R29 复审 3 条已修完（m1b-c-fix2，REVIEW.md R30 节），未执行 git commit。**

## 〇、修复轮 m1b-c-fix（R27 全 5 条，2026-10-08）

R27 裁决 2 阻塞 + 2 建议 + 1 可选全部修完（逐条文件:行号见 REVIEW.md R28）。行为变化：① 服务端 `agent_tasks.cpu_pct` 值域放宽为「有限且 ≥0」不设上限（多核 ps %CPU 超 100 是 ps 语义，此前 8 核打满会整条心跳 400 丢库）；mem_pct 维持 [0,100]；② 会话目录遍历全程挂 8s 预算 ctx、8192 上限改文件+目录合计、触顶 `Truncated` 如实进 collect_errors（海量空目录不再无限遍历）；③ 离线/快照过期（>90s 未刷新，`taskStaleAfter`）节点的任务打 `stale` 标记——面板任务大卡计时冻结+「snapshot stale」标注、仪表与 meta 不冒充运行中，MCP list_agent_tasks 同源带标记；④ 任务清单触顶 64 触发 `agent_tasks_truncated` 上报，落 nodes.tasks_truncated（**migration v5**），面板任务卡/meta 标注「list truncated」；⑤ headscale.go:41 注释与节流窗口实际行为对齐（纯注释）。验证：vet/gofmt 零输出、171 用例全绿（+9）、make cross 三平台、node --check + DOM 桩冒烟过。

## 〇′、修复轮 m1b-c-fix2（R29 全 3 条，2026-10-08）

R29 裁决 1 阻塞（R27-#2 第 2 次修复）+ 2 建议全部修完（逐条文件:行号见 REVIEW.md R30）。行为变化：① 会话目录遍历**弃 `filepath.WalkDir` 改显式栈式 DFS + 分批 readdir**（`os.Open` + `ReadDir(256)` 分批，批间查 ctx、批内逐条查条目配额）——超大单目录（数十万条目）的读取量不再发生在检查之前，被「8192 配额 + 一批」与 8s 预算双重封顶，触顶/超时如实报不完整（Truncated→collect_errors / 缺席，上报口径不变）；② 面板 overview **拉取持续失败超 90s**（与服务端 `taskStaleAfter` 同窗口径）时旧任务卡按 stale 冻结（复用 updateTaskCard 同一套语义：计时停走、呼吸点停转、「snapshot stale」标注、仪表只计非 stale），窗口内失败不冻结，恢复刷新自动解冻；③ MCP `list_agent_tasks` 返回结构新增 `tasks_truncated` 键（恒输出，涉及节点截断标记并集），工具描述同步——MCP 用户可知清单仅为前 64 条。验证：vet/gofmt 零输出、**174 用例全绿（+3）**、make cross 三平台、node --check + DOM 桩冒烟（冻结/解冻四拍）过。

## 一、交付物清单

| 项 | 说明 |
|----|------|
| **agent 任务采集**（`internal/agent/collect/agenttasks.go`，SPEC §2.1） | 每 15s 随心跳：①进程扫描——unix `ps -eo pid,etime,pcpu,pmem,command`，按 agentdisc 已知清单（known 默认五项 + config custom 并集，同名 custom 优先）匹配**命令行首 token 基名**（宁漏不误：`vim ~/.codex/…` 不误报；解释器包装漏报如实）；Windows `tasklist /FO CSV /NH` 兜底（cpu/mem/etime/started_at 报 null 不填 0）。每命中进程抓 pid/agent_name/cmd（截断 200 字节）/elapsed_s/cpu_pct/mem_pct/started_at（etime 反推）。②会话目录 stat——known 内置映射（zcode `~/.zcode/cli/rollout`、codex `~/.codex/sessions`、claude `~/.claude/projects`、gemini `~/.gemini/tmp`；aider 无公认目录不统计），custom 走配置新字段 `agent_scan.custom[].session_dir`（~ 展开，同名声明优先）；**只 stat 不读内容**（WalkDir d.Info()=lstat），取树内文件最近 mtime+文件数（**条目上限 8192 为文件+目录合计，R27-#2 后触顶置 Truncated 如实进 collect_errors，目录遍历全程挂整轮 8s 预算 ctx**），目录缺失置 null。整轮预算 8s；ps/tasklist **失败=缺席字段+collect_errors（失败≠没有任务，绝不空数组清行）**；ps 输出触 1MB 上限同样显式报错（真机回归：64KB 级上限在 558 进程的 macOS 上静默丢高 pid 行，自测抓出修复）。心跳新增顶层可选字段 `agent_tasks`（三态沿用 R11-A：扫描成功即上报数组、空数组=清空该节点、缺席=无变化）+ `agent_tasks_truncated`（单拍匹配 >64 时置 true，R27-#4）+ `agents[].last_activity/session_files`（runner 每拍合并活动度进发现清单，15s 刷新） |
| **console 侧落库与聚合**（SPEC §2.2） | migration **v4**：`agent_tasks` 表（node_id/pid/agent_name/cmd/elapsed_s/cpu_pct/mem_pct/started_at/updated_at，节点删除级联）+ `ai_agents` 增列 `last_activity`/`session_files`。心跳 `agent_tasks` 经 `optionalArray` 三态校验（显式 null 400）后**按节点先清后插全量替换**（进程消失=任务结束不留历史行；任务历史档案属 M1c+ 不做）；值域校验（pid>0 去重、elapsed≥0、**cpu_pct 有限且≥0 不设上限（多核 ps %CPU 超 100 是 ps 语义，R27-#1）**、mem_pct∈[0,100] 拒 NaN/Inf、started_at 不晚于 now+1d、cmd 服务端再截断净化、≤64 条）；`agent_tasks_truncated` 随显式快照落 nodes.tasks_truncated（**migration v5**）；`HeartbeatFull` 单事务扩为 metrics+services+agents+tasks+截断标志写入（任一失败整体回滚）。`/api/panel/overview` 聚合扩展：`running_tasks`（全网任务快照带节点名与 **stale 过期标记（R27-#3）**）+ agents 带 `last_activity/session_files` + 节点带 `tasks_truncated` |
| **MCP**（SPEC §2.2） | 新增只读工具 `list_agent_tasks(node?)`（结构化输出 `{tasks:[…], tasks_truncated:bool}`——R29-#3 后截断标记恒输出（涉及节点 `nodes.tasks_truncated` 并集，true=清单仅为前 64 条），未知节点空清单）——**六只读工具**：list_nodes/get_node/list_services/list_agents/**list_agent_tasks**/get_mesh_status。E2E 实测返回真数据（本机 3 个真实 codex 进程 + 伪装 zcode 进程） |
| **亮色主题面板**（SPEC §1/§2.3，`internal/panel/web/`） | **亮色为默认**（`data-theme="light"`）：token 三元组换亮色（emerald 700/amber 700/red 600，白底对比度 4.8/4.6/4.5:1——等宽字数据、状态点、offline 红全部白底可读）；语义层换冷调 porcelain/ink；frost 按技能「light-mode tuning」**更薄（panel mix 42/46%→30/34%）更饱和（saturate 190/200%→210/215%）**+ 亮 specular；atmosphere 换亮色 mesh（blob `screen`→`multiply`，`--aura` 0.30→0.24）；glass-inset 亮色改 ink 轻染。**暗色完整保留在 `[data-theme="dark"]` 变量块（M1b-b 原值），TODO 标 M1c+ 切换，翻属性整体回暗组件零改动**。中台信息架构：顶栏四仪表（在线终端/运行中任务总数（>0 亮绿；**只计未过期快照，R27-#3**）/服务异常/tailnet 在线）→ **运行中 agent 任务大卡区**（核心，跨终端卡流：终端/agent/cmd 摘要/活动计时本地每秒走动/cpu/呼吸绿点；**快照过期（stale）时计时冻结+「snapshot stale」标注+呼吸停转、截断节点的卡标「list truncated」（R27-#3/#4）**；空态「全部安静」受管可往返）→ 终端区每终端一卡（状态/角色/sparkline/disk/agent 清单带 `act <rel>` last_activity）→ 服务+tailnet 区块保留。Makefile 版本批次号 m1b-b→m1b-c |
| **顺手修：tailnet ips=null**（SPEC §4） | 根因：headscale 版本演进中节点地址字段改名（新版 `addresses` / 旧版 proto `ip_addresses`→JSON `ipAddresses`），旧解析只认 `addresses` → 旧版部署 ips 空串→视图 null。修复：wireNode 双字段声明取非空者（同现时优 addresses）；`availableRoutes` 为子网路由（非节点 100.x 地址）刻意不采。单测钉住旧字段解析入库全链路 + 双字段同现优先级 |
| config 扩展 | `agent_scan.custom[].session_dir`（可选，~ 展开，不做存在性校验——目录此刻缺失是合法状态） |
| 测试 | 净增 30 用例（132→162）：collector 10（解析/etime/截断 rune 安全/扫描失败语义/触顶报错/tasklist/目录 stat/上限/匹配口径/空清单）、registry 4（三态/值域/活跃度透传/cmd 净化）、store 4（全量替换+清空+隔离/NULL 维度+级联/活跃度往返/migration v4）、meshview 2（overview 扩展/节点过滤）、mcpserver 2（六工具清单/list_agent_tasks）、headscale 2（旧字段/优先级）+ 既有用例随协议扩展补齐。**R27 修复轮再净增 9（162→171）**：collect 3（空目录条目上限收敛/目录遍历 ctx/预算耗尽缺席）、registry 3（cpu 750 放行落库/NaN-Inf 拒绝/截断标志链路）、store 2（migration v5/截断标志往返）、meshview 1（stale 标记三态）。**R29 修复轮再净增 3（171→174）**：collect 2（超大单目录小配额×小批量收敛+截断如实/flipCtx 中途超时 ok=false）、mcpserver 1（tasks_truncated 透出） |

## 二、Liquid Glass 亮色适配说明（SPEC §2.3 交付自证五要素）

- **frost**：亮色下白面板藏 frost——按技能 liquid-glass.md「反直觉」节收薄 panel mix（surface 42%→30%、soft 46%→34%）+ 拉高 saturate（190%→210%、200%→215%），pastel aura 才读得出来；specular 顶边换亮 lip（0.55/0.65）；glass-inset 在亮色下改用 ink 3.5% 轻染 + 白顶缘反光（白 elev 叠白板不可见），hover 染深一档「lift in」。
- **atmosphere**：亮色 mesh——blob 混合模式 `screen`（暗色提亮）在白底不可见，改 `multiply` 叠出 pastel；`--aura` 0.30→0.24（技能 theming.md：暗色 aura 复用到亮色会发闷）；网格线换深色 3.2% 墨线；弧线/边框吃 token 自动适配。
- **token**：三 RGB 三元组纪律不变，亮色换 Radix 深阶（emerald 700 `4 120 87`/amber 700 `180 83 9`/red 600 `220 38 38`）——状态色即品牌色延续，且白底对比度全部 ≥4.5:1；全站色板照旧从三元组派生，组件无一处写死 hex。暗色三元组+语义层原值保留在 `[data-theme="dark"]` 变量块。
- **排版**：等宽系统栈不变（离线取舍延续 M1b-b）；标题超大 mono + 衬线斜体 accent（`Agent *ops*`，呼应 agent 中台定位）；数据全部 `tabular-nums`。
- **动效**：唯一 easing 签名延续；新增「活动计时」每秒走动（数据更新非动画，reduced-motion 下照常走）；运行中任务呼吸绿点（与 offline 呼吸红点同款曲线）；空态「全部安静」给足存在感（SPEC：这本身是信息）。

## 三、部署上机项与已知限制

### 部署上机项（commit 后由 Hana 主会话执行）

1. **cloud-agent 升级 m1b-c 二进制**（`make cross` 产物 linux/amd64）：真机上报 agent_tasks/last_activity，面板与 MCP list_agent_tasks 核对真数据（本机 E2E 已全链路验证：伪装进程匹配→消失清行→MCP 真数据）。
2. **真 headscale 连测**：确认现网版本节点地址字段形态（`addresses` vs `ipAddresses`），面板 tailnet ips 列显示（本机 mock 双形态已覆盖，真机字段形态待核）。
3. **浏览器人眼过亮色面板**：frost 薄饱和/pastel atmosphere/任务大卡/活动计时动效的实际观感（本机 Edge headless 受沙箱限制无法截图，同 M1b-b 口径；Node+最小 DOM 桩冒烟以真数据跑通 render 全路径并抓修 2 个运行时缺陷，见 REVIEW「四个真实缺陷」节）。

### 已知限制（本机验证边界内如实说明）

4. known 会话目录内置四家映射；aider 无公认固定目录不统计（last_activity 如实 null）；可用 `known: []` + custom + `session_dir` 整体替换。目录映射不校验存在性（此刻缺失=合法 null）。
5. 进程匹配只看命令行首 token 基名：解释器包装（`python -m aider`）与改名进程（如本机真实 `zcode-host-local-1`）漏报如实（宁漏不误；本机实测真实 zcode CLI 进程形态即属此类，已记录）。
6. agent_tasks 只存当前快照（进程消失即清行），历史任务档案/统计为 M1c+（SPEC §3 非目标）。
7. Windows 任务采集为 tasklist 兜底（cpu/mem/etime/started_at 报 null），精细化属 M1c+（SPEC §3）。
8. 升级路径：v3 库原地跑 v4 migration（`ALTER TABLE ai_agents ADD COLUMN` 两列，旧行 NULL 语义正确）→ v5（nodes 增 `tasks_truncated` 列，NOT NULL DEFAULT 0，R27-#4）；MCP 只读句柄不执行 migration，console 服务模式需先启动一次。

## 四、验证汇总（2026-10-08）

`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` **162 用例全绿**（12 包全 ok，M1b-b 收口 132 → 净增 30）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check` 面板 JS 过；Node+最小 DOM 桩冒烟（真 overview 数据全路径 render：任务大卡/终端卡/agent 清单/空态往返/计时 tick）通过并抓修 2 个面板运行时缺陷。本机 E2E（/tmp/m1bc-e2e，心跳 5s）：console+agent 全链路——伪装进程（`exec -a zcode /bin/sleep 300`）匹配上报（pid/elapsed/started_at etime 反推全对）→ 进程退出 1-2 拍内快照清行；3 个真实 codex 进程全被正确匹配；agents last_activity/session_files 上报；`/api/panel/overview` 含 `running_tasks`；MCP 六工具 + `list_agent_tasks` 真数据；`data-theme="light"` 默认输出。对用户可见的行为变化：① agent 心跳新增 `agent_tasks`/`agents[].last_activity` 字段（旧 console 收到新 agent 心跳会因未知字段忽略而兼容，新 console 收旧 agent 心跳字段缺席=无变化，双向兼容）；② migration v4 自动执行（console 服务模式首次启动）；③ MCP 工具数 5→6；④ 面板默认亮色 + 中台三区块（顶栏 tailnet 仪表口径从 tracked 改为 online）；⑤ `--version` 批次号变 `m1b-c+git_<hash>`；⑥ tailnet ips 在旧版 headscale 下不再为 null。

### 修复轮 m1b-c-fix 验证（2026-10-08，R27 后）

`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` **171 用例全绿**（162 + 修复轮净增 9，12 包全 ok）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check` + DOM 桩冒烟扩展 stale/truncated 形态（任务卡标注/冻结计时/仪表只计非 stale/meta `N running · N stale · list truncated`/恢复/空态往返）全过。增量行为变化：① cpu_pct>100 的心跳不再整条 400 丢库；② migration v5 随 console 服务模式首启自动执行；③ 旧 console + 新 agent 组合下 `agent_tasks_truncated` 未知字段被忽略（双向兼容不变）；④ 心跳 `collect_errors` 可能新增 `agent_activity` 键（会话目录触顶截断说明，值 ≤256B 净化入库）。

### 修复轮 m1b-c-fix2 验证（2026-10-08，R29 后）

`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` **174 用例全绿**（171 + 修复轮净增 3，12 包全 ok）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check` + DOM 桩冒烟（可控时钟四拍：成功渲染计时走动 → 窗口内失败不冻结 → 超 90s 失败冻结（标注/停转/仪表 0/meta/计时停走）→ 恢复自动解冻）通过。增量行为变化：① 单个超大会话目录（数十万条目）读取量被「8192 配额 + 一批（256）」与 8s 预算双重封顶，不再无界读盘；触顶/超时如实报不完整（collect_errors/缺席口径不变）；② console 不可达超 90s 后面板任务卡冻结、仪表归零，恢复自动回活；③ MCP `list_agent_tasks` 返回新增恒在键 `tasks_truncated`（旧客户端按未知字段忽略，兼容）。

---

# DELIVERY — M1b-b 交付说明（MCP Server + Headscale 集成 + Web 只读面板）

> 交付人：zcode · 2026-10-08
> 依据：SPEC-M1b-b.md（唯一需求源，逐条执行）+ DESIGN.md v0.7 §6/§2/§3/§8
> 基线：M1b-a（commit 19f3a33）。状态：**本机可验证项全部自测通过；真 headscale 连测与浏览器人眼验收未做，列入「部署上机项清单」待上机执行**（见 §四；逐条证据见 REVIEW.md「自测报告 · zcode M1b-b」节；R19 审查 12 条意见修复见 REVIEW.md R20 节）。未执行 git commit（提交权在流水线）。
> 前端设计按伦哥要求采用 `docs/skills/liquid-glass-frontend` 技能：只取其设计系统与原则，技术基线仍为 SPEC 硬约束的纯静态 HTML/CSS/原生 JS + Go embed（无 Node/npm/CDN）。

## 一、交付物清单

| 项 | 说明 |
|----|------|
| **MCP Server**（`meshconsole mcp` 子命令） | 官方 `github.com/modelcontextprotocol/go-sdk v1.8.0`（版本锁定 go.mod；GOPROXY=goproxy.cn 实拉成功，未走手写 JSON-RPC 备选）。**stdio 传输**（MCP 2025-06-18 语义由 SDK 保证），零网络暴露；五个只读工具：`list_nodes`（nodes 全行）/`get_node`（详情 + 24h metrics 摘要 cpu/mem 均值峰值、磁盘占比 + 该节点 services/agents）/`list_services(node?)`/`list_agents(node?)`/`get_mesh_status`（节点/在线/离线名单、服务异常名单、tailnet 概况、数据新鲜度）。配置复用 `-config console.yaml`（`LoadConsoleForPKI` 读 db_path，不强制注册 token）；**库只读打开**（`store.OpenReadOnly`：`?mode=ro` + `query_only=1` 双保险，不执行 migration、库不存在报错不创建；store 9 个写路径方法加 ErrReadOnly 守卫）。结构化输出顶层一律对象（structuredContent 稳妥形态）；get_node 未知节点回 IsError 工具错误并提示 list_nodes。**日志全走 stderr**（stdout 是 JSON-RPC 通道，`cmd/console/main.go:100` cmdMCP） |
| **Headscale 集成**（`internal/headscale/`，console 内嵌 goroutine） | 数据源 REST `GET /api/v1/node`（Bearer api_key）；配置 `console.yaml` 新增 `headscale:` 段（`url` 缺省 `http://127.0.0.1:8080`、**scheme 限 http/https 其他启动报错（R19-#10）**、`api_key` 必填、`interval_s` 缺省 300 下限 30；**无该段=禁用**，启动 INFO 一条不报错）。HTTP client 超时 10s；解析只取 id/name/addresses/online/lastSeen（**id 兼容数字/字符串两形态**，未知字段忽略——headscale 版本演进不炸；body ≤4MiB 封顶，**超限/读满上限未见结尾/中途截断一律显式报 malformed（R21-#1）**；非 200 报错含状态码+前 200B 说明；**HTTP 200 但 body 非法（{}/null/缺 nodes 键/尾随垃圾（含 `]`/`}` 起始与拼接值）/超限/截断）同样按拉取失败处理，保留旧数据不清库，只有严格合法的 nodes 数组（含空数组；二次 Decode 必须 io.EOF，registry decodeJSONStrict 同口径）才全量替换（R19-#1/R21-#1）**）。存储：migration **v3** `tailnet_nodes`（id/machine_name/ips/online/last_seen/updated_at），**每次拉取单事务全量替换**（先清后插，中途失败整体回滚保留旧数据（R19-#12 断言））；拉取失败不触库（旧数据保留）+ 连续失败计数（首个失败立即 WARN，其后节流 1/h **且节流窗口独立于失败计数/streak——恢复归零后再失败仍受窗口约束，窗口内降级 Debug（R21-#2）**；**计数在连续成功 3 次后才归零并 INFO 恢复——闪断不再反复突破节流（R19-#9）**）。E2E 实测：401 → `WARN consecutive_failures:1` + 数据原样；恢复 → `sync recovered after_failures:1` + 续替 |
| **Web 只读面板**（`internal/panel/` + `internal/panel/web/`） | 纯静态 HTML/CSS/原生 JS 三件套 **embed 进二进制**（`//go:embed all:web`，离线/内网可用，无 Node/npm/CDN/外部字体）。与 agent API 同端口：`GET /` 面板页、`GET /api/panel/overview` 聚合 JSON（nodes（含 id/24h metrics 摘要）/services/agents/**service_issues 异常名单（R19-#4）**/tailnet 一次取齐，15s 轮询）；数据出口与 MCP 同源（`internal/meshview`，不复制 SQL）。**安全（DESIGN §8 全做 + R19-#7 严格化）**：Host/Origin 校验中间件（允许名单 localhost/127.0.0.1/[::1] 带或不带端口、大小写不敏感；url.Parse 严格解析——`attacker.com:127.0.0.1` rebinding 变体、非数字/越界/空端口、`[localhost]` 方括号非 IPv6、裸 `::1`、userinfo、携带 path 等畸形形式一律 403；Origin scheme 限 http(s)、拒绝 userinfo/path）；全路径 `X-Content-Type-Options: nosniff` + `Cache-Control: no-store`（403 也不例外）；无 Cookie 无状态只读（无 CSRF 面）；面板访问日志 DEBUG 不刷屏；未知路径 404、方法限定 GET（POST 405）；JS 动态数据全部 `textContent` 写入（DB 内容视为不可信，DESIGN §7-8） |
| meshview 服务层（`internal/meshview/`） | MCP 五工具与面板 overview 的**同源只读数据出口**（SPEC §4.1「service 层复用，别复制 SQL」）：视图类型即 JSON 契约；节点视图含 `id`（R19-#2）；服务异常口径唯一谓词 `serviceIsIssue`：`status != active` 且 updated_at 未超 300s 宽限（`serviceIssueGrace`；`stale` 簿记态不计，陈旧行=离线节点遗留数据不当现行故障，单测双向钉住）——get_mesh_status 与面板 overview 的 `service_issues` 共用，前端只消费名单不做业务计算（R19-#4）；overview 节点卡带 24h metrics 摘要（复用 store `MetricsStatsSince`，R19-#3）。节点行对外视图 `NodeRecord` 不含 token_hash（只读消费方无需凭据材料） |
| store 扩展 | migration v3（tailnet_nodes 表）；`OpenReadOnly`；`ReplaceTailnetNodes`（单事务全量替换，中途失败整体回滚保留旧数据（R19-#12））；`ListTailnetNodes`/`ListAllServices`/`ListAllAgents`/`ListNodes`（无 token_hash）/`NodeNameIDMap`/`MetricsStatsSince`（24h 均值峰值，NULL 语义保持；**total=0 的行不再整行剔除——占比分指标 NULLIF 求值，CPU 等可用指标照常参与（R19-#8）**）/`MetricsSeriesSince`（sparkline 序列，升序限条）/`LatestMetricsTime`（新鲜度）；`Close` 兼容只读句柄 |
| config 扩展 | console `headscale:` 段（校验：url 合法性 + **scheme 限 http/https（R19-#10）**、api_key 非空、interval_s ≥30）；R17-#2 systemd/docker target ≤256 字节启动校验（与服务端入库截断口径对齐） |
| 观察点修复 | ① `meshconsole --version` / `meshagent --version`（**语义版本 `m1b-b+git_<short_hash>` + commit，R19-#5**）+ 两二进制统一 usage/`-h` 风格；② Makefile 补 `-X main.commit` 注入（版本+commit 双注入）+ `bin/meshconsole` 重建依赖纳入 `internal/panel/web/*`；③ process 型自进程判定加固（详见 §三.3） |
| R17 三条转来候选闭环 | ① `pki.go:74` tailnet_ip 非法即拒绝（旧实现静默忽略该 SAN 仍报成功）+ 单测；② `config.go:462` target 长度上限 + 单测；③ `runner.go:135` 注释勘误（null=400 违规，缺席才是无变化，与 R11-A 实现对齐） |
| deploy/ | `console.example.yaml` 新增 headscale 段注释样例与 Web 面板说明 |
| 依赖 | `github.com/modelcontextprotocol/go-sdk v1.8.0`（唯一新增直接依赖；间接：google/jsonschema-go、segmentio/encoding、golang-jwt、oauth2、x/time 等随 SDK 带入，go.sum 锁定） |

## 二、Liquid Glass 面板设计说明（技能落地）

**Aesthetic direction（写定）**：「控制室 / mission-control on glass」——深冷墨底 + 磷光 mint 品牌色（兼作 online/ok 状态色，品牌与语义统一）、amber=warn、red=crit；数据全部等宽字体的仪表盘气质。拒绝「三张等宽卡片居中 hero」模板脸。

**签名时刻**：首页「仪表条」（glass-surface-soft 四联超大等宽数字 online/offline/svc issues/tailnet，与左侧超大 mono 标题呈不对称沉底对齐）+ offline/failed 状态的呼吸红点。

- **五要素自证**（frost/atmosphere/token/排版/动效，逐条落到 glass.css 行为）见 REVIEW.md 自测报告「Liquid Glass 五要素自证」节。
- **布局**：非对称 bento——7/5 与 5/7 交替；节点卡不等宽（主节点大卡：状态/心跳/uptime/cpu+mem sparkline（内联 SVG 手绘，NULL 断线）/disk meter；次节点小卡更小更密）；服务/agent/tailnet 清单用 `.glass-inset` 内嵌行（玻璃上玻璃，无嵌套 backdrop-filter）。
- **取舍声明**：等宽字体用系统栈（JetBrains Mono/IBM Plex Mono 命中优先，回落 SF Mono/Menlo/Consolas）——离线硬约束下不引 webfont，代价是无网关机器上字形不可控；面板文案用英文控制室惯例（kicker/标签/大数字），设计性格优先。浅色主题按技能 theming.md 分层留好接缝（`[data-theme]` 语义层已就绪），M1b-b 仅交付深色（SPEC 口径），代码内 TODO 注释标明。
- **动效纪律**：全站唯一 easing 签名 `cubic-bezier(0.22,1,0.36,1)`；载入一次性 stagger 编排（标题 clip 揭示→面板按序浮起，非 uniform fade-in-up）；刷新时数字 tween/状态点过渡；`prefers-reduced-motion` 全关；blob 漂移 26/31/37s 错频。

## 三、观察点闭环（部署验证转来，SPEC §5）

1. **`--version` 子命令**：两二进制新增 `--version/-version/version` → `<bin> <version> (commit <commit>)`；`-h/--help/help` 统一 usage 出口，`meshconsole pki -h`、`meshconsole mcp -h` 同风格（用法行 + flag 默认值）。实测（R19-#5 语义化后）：`meshconsole m1b-b+git_19f3a339 (commit 19f3a339)`。
2. **ldflags 注入**：Makefile LDFLAGS 补 `-X main.commit=$(COMMIT)`；部署验证时显示旧号 `31eafac-dirty` 的根因是服务器上跑的是 M1a 期构建产物——**部署流程口径：拉新代码后必须重跑 `make build`/`make cross` 并重启 systemd**（`--version` 现可直接核对版本+commit）。另修 `bin/meshconsole` 目标依赖缺 `internal/panel/web/*`（改面板不重编会打进旧静态资源）。
3. **process 型自进程判定**：核查结论=**代码不存在 exclude-self 设计**（裸 `pgrep -f`，pgrep 只排除自身不排除祖先；本机实测祖先可命中）。部署观察到的 inactive 最可能是该节点 target 与实际 cmdline 不符（配置层）或平台边缘致 pgrep 漏检。按 SPEC「等效方案」加固：pgrep exit 1 时以本进程命令行（os.Args，`/proc/self/cmdline` 跨平台等效物）按 pgrep 同口径 ERE 做确定性自核对，命中报 `active`+`count=1 (self…)` 如实标注来源，不命中维持 inactive。4 例单测 + 真机自探活/反例用例 + E2E（自探 target → active count=1）。

## 四、部署上机项清单与已知限制（R19-#6 口径：本机未验证的明确列出，不含糊）

### 部署上机项（本机无法验证，commit 后由 Hana 主会话上机执行）

1. **真 headscale 连测**（云机现成实例）：配真 API key 拉到真实节点并入库展示（SPEC §7 第 4 项的前半）。本机仅有 mock E2E 兜底：节点解析入库、id 数字/字符串两形态、401 → WARN+旧数据保留、恢复 → INFO 续替、非法 200 body（{}/null/缺 nodes/尾随垃圾/超限/截断）→ 拉取失败不清库（R19-#1/R21-#1）均已覆盖；真机版本演进（字段增删）行为待上机核对。
2. **浏览器人眼过面板**：布局/动效/深色玻璃质感/呼吸红点/数字 tween 的实际观感（SPEC §4.4 后半）。本机渲染管线已由 Node + 最小 DOM 桩冒烟实证（节点卡/仪表/清单行/sparkline/页脚全构建、15s 轮询钉住、并实抓修复过一处真实渲染缺陷），但玻璃质感只能人眼最终验收；需经隧道打开面板过一遍。
3. **Windows 真机**（M1a 起遗留）：process 型在裸 Windows 的上报语义（pgrep 缺失 → unavailable+说明）。三平台编译通过，真机行为未验。

### 已知限制（本机验证边界内的如实说明）

4. MCP stdio：管道一次性写入全部帧后立即关 stdin 会随 EOF 快速退出（帧可能未及处理）；真实 MCP 客户端为常驻会话不受影响。E2E 采用分帧+间歇写入。
5. 面板 poll 15s、sparkline 48 点、异常宽限 300s、headscale interval 缺省 300s 为固定常量（SPEC 口径），未入配置。
6. tailnet_nodes.online 直接采信 headscale 报告；旧版 API 无该字段时按 offline 存（不推导不编造），last_seen 仍可见。
7. MCP/面板共享 24h 摘要窗口与 48 点 sparkline 常量（metricsWindow/sparkPoints），超窗数据依 retention（7 天）仍在库，后续如需更长历史再扩展参数。
8. `meshconsole mcp` 打开库要求 schema 已就绪（先跑过服务模式）；只读句柄不执行 migration，库不存在报错退出（提示先启动 console）。

## 五、验证汇总（2026-10-08，R22 修复轮 2 后全量重测）

`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` **132 用例全绿**（R20 收口基线 130 + R22 修复轮 2 新增 2：headscale 截断/超限、节流独立性；12 包全 ok）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check` 面板 JS 语法过；Makefile 依赖行为实测（touch go.mod → bin 产物立即重编）。本机 E2E（R20 轮）：console+agent 全链路（心跳/服务清单/AI agent 发现/tailnet 并入）、面板 4.4 本机可验项（`curl | grep glass`、overview 聚合 JSON、非法 Host 403）、MCP 三方法握手+真数据、headscale 降级/恢复、`--version` 双二进制（`m1b-b+git_19f3a339`）。R19 修复轮真机冒烟补充复核：overview 输出节点 `id`/每节点 24h `metrics` 摘要/`service_issues` 名单；Host 畸形变体全 403；headscale 不可达 WARN 恰一条+旧数据保留。**真 headscale 连测与浏览器人眼验收未做**（见 §四 部署上机项清单）。对用户可见的行为变化：console 启动后多出面板入口（同端口 `/`）与可选 headscale 拉取循环；新增 `meshconsole mcp` 子命令与两二进制 `--version`（语义版本 `m1b-b+git_<hash>`）；pki 对非法 tailnet_ip 从静默忽略改为拒绝；R19/R21 两轮修复引入的行为收紧见 REVIEW.md R20/R22 节末「对用户可见的行为变化」（R21→R22 轮要点：headscale 200 响应的尾随垃圾/超限/截断一律按拉取失败保留旧数据；WARN 节流窗口独立于失败计数严格 1/h；面板 tailnet 空态转有数据不再提示文案与节点行同显；bin 产物随 go.mod/go.sum/Makefile 变更自动重建）。

---

# DELIVERY — M1b-a 交付说明（TLS 全覆盖 + 受管服务清单 + AI agent 发现）

> 交付人：zcode · 2026-10-07（R10 审查 5 阻塞 + 2 建议、R11 复审新增 7 条裁决、R13 复审 4 条残留，逐条修复说明见 REVIEW.md「修复轮记录 · zcode m1b-a-fix」与 R14 节）
> 依据：SPEC-M1b-a.md（任务书）+ DESIGN.md v0.7（§4.2-A/B、§4.3、§5、§7-3/4/5/9）
> 基线：M1a（commit 31eafac）。状态：**全部 8 条验收项自测通过**。未执行 git commit（提交权在流水线）。

## 一、交付物清单

| 项 | 说明 |
|----|------|
| `internal/pki/` | PKI 生命周期（新包）：`Ensure` 幂等生成/校验 CA + 服务端证书（ECDSA P-256，CA 10 年/服务端 5 年）；**CA 已存在永不重新生成、服务端私钥永不覆盖**（R10-#2 收紧：ca.crt 与 ca.key **任一单独存在视为不完整，拒绝生成并提示人工处置**——旧实现在缺 ca.crt 时会重新生成覆盖 ca.key；复用前校验证书/私钥**配对**（R11-D）与 **有效期**（R13-#2：已过期/尚未生效拒绝复用并提示重新 make pki，作废产物无法被客户端验证却报成功）；复用既有私钥时**权限必须 0600**，非 Windows 校验）；SAN 恒含 localhost/主机名/127.0.0.1/::1，配置 tailnet_ip 追加；SAN 变更（如 tailnet_ip 调整）、证书过期/**尚未生效/缺 ServerAuth EKU**（R15-#2）时**仅用既有 CA+私钥重签证书**；私钥 0600（唯一临时文件+原子 rename）、证书 0644；`FingerprintPEM` 输出服务端证书 SHA-256 指纹（hex 无冒号）供 agent 配置；`ServerTLSConfig(cert, key, caPath)` 供启动加载（**证书缺失或私钥权限非 0600 即启动失败**，提示先跑 pki / chmod 600，R15-#3），ca 确为签发方时**追加进下发链**——仅配指纹的 agent 需从下发链取得信任锚做完整 x509 验证（R10-#1） |
| Makefile | 新增 `pki` 目标：`make pki [CONFIG=console.yaml]`（依赖 build，调 `meshconsole pki`） |
| `cmd/console/` | 新增 `meshconsole pki` 子命令（读 pki_dir/tailnet_ip，**不强制注册 token**——生成证书是部署前置动作，经 `LoadConsoleForPKI`）；服务模式改 **HTTPS-only**：`TCP → netutil.LimitListener(max_connections) → TLS` 监听链，启动仅加载 pki 不生成；net/http 服务端无 TLS 握手超时字段（ReadHeaderTimeout 在握手后才生效），listener 层对未握手连接设 10s deadline 防 slowloris 占满连接配额；drainHTTP 三段式不变，对 TLS listener 实测正常；退出链补端到端回归单测（真实 pki TLS 证书 + LimitListener 链 + 在途连接跑同款 drainHTTP 序列，R15-#4） |
| 连接限额（R5 裁决落地） | `golang.org/x/net v0.59.0` 的 `netutil.LimitListener`（**已读源码确认**：带 done-channel 修复，listener Close 时阻塞中的 acquire 立即返回错误，Accept 不会被信号量卡死——与 Shutdown 无 R5 式互锁）。配置 `max_connections` 默认 256 可配，非正数拒绝启动。超额连接 TCP 建立后在配额队列排队（非拒绝），有连接释放即被服务 |
| `internal/agent/`（client.go） | `NewHTTPClient(consoleURL, caCert, fingerprint)`：**console_url 强制 https**（R10-#1，明文 http/其他 scheme 一律拒绝，register 与 run 共用此入口，无明文路径）；https 必须配 ca_cert 或 fingerprint 之一（两者皆空拒绝构造，**无跳过验证选项**）；`CheckRedirect` 拒绝向 http 降级的重定向（上限 3 跳）；验证语义（R10-#1 收紧）：ca_cert 模式 = RootCAs 标准链+有效期+主机名验证，配指纹则叠加复核；**仅指纹模式 = 下发链重建完整 x509 验证（证书链+有效期+主机名全执行）+ 叶子 SHA-256 指纹比对**——`InsecureSkipVerify` 仅为绕过系统信任库，x509 语义一项不豁免；下发链末端必须是自签锚（Go 对「叶子自身在信任池」有直通捷径，已显式拦截非自签锚；**R13-#1 收紧：self-issued 名单≠self-signed——入池前用锚自身公钥验证其 TBS 自签名，同名单异密钥的伪造锚直接拒绝**；实测 leaf-only 下发即拒绝）；MinVersion TLS1.2；指纹归一化兼容冒号/大小写，**格式错误只提示 hex 要求、不回显原值**（R13-#3，与 config 侧同口径）；错误信息不含任何一方指纹值/证书材料；`Register` 改收外部 client |
| `internal/agent/`（runner.go） | 心跳扩展装配：`Services` 每 15s 随心跳查询上报（配置声明过即恒报，**显式空列表发 `[]` 驱动 console 全量替换**，不发 null）；`Discover` 扫描循环（启动即扫 + 每 5 分钟，原子换入结果，首轮完成前心跳不带 agents 字段） |
| `internal/agent/collect/`（services.go） | 受管服务状态查询：systemd=`systemctl is-active <target>`（**退出码+文本双口径，R10-#3**：0=active、3=inactive（failed 文本精化）、其他非零按文本映射、只有执行器错误（非 ExitError，如命令不存在）才 unavailable——旧实现把 inactive 的退出码 3 一律归 unavailable）、docker=`<docker_bin> ps --filter name=<target> --format '{{.State}}'`（**只读 CLI，不经 SDK 直连 socket**，`docker_bin` 可配置默认 "docker"（R10-#4），生产必须只读 helper/socket-proxy（M1b-b 落地）；daemon 不可达/二进制缺失→unavailable+stderr 说明）、process=`pgrep -f <target>` 计数（exit 1=inactive、exit ≥2=unknown、二进制缺失=unavailable）。一律固定 argv exec（无 shell），单条 5s ctx 超时；**exec 输出 capped buffer 64KB 封顶**（R10-#5，写入阶段限额，触顶截断并在该条 detail 标注 `output truncated at 64KB`）；`cmd.WaitDelay=500ms` 防 CommandContext 只杀直接子进程时孙进程持有输出管道令 Run() 悬挂（单测实抓过该缺陷）；`cmdRunner` 接口抽象，单测注入 canned 输出 |
| `internal/agentdisc/` | AI agent 发现（新包，DESIGN §8 规划位置）：双轨——内置已知清单（默认 zcode/codex/claude/gemini/aider；`LookPath` 命中才上报，不存在的不出现；`--version` 5s 超时取首行）+ 配置显式声明（custom CLI 绝对路径直探；service 型仅本地 TCP 端口探测存活，**不可 invoke**）；端口探测改 `Dialer.DialContext`（R10-#6，响应取消：探测被打断按 unavailable+说明上报，不编造 inactive）；版本探测输出同口径 64KB capped buffer（R10-#5；**R13-#4 收紧：截断判定针对「实际选中的行/缓冲」——版本值取自哪个缓冲就按哪个缓冲判断，来自截断缓冲且无换行终止的行不得作版本值（报 unavailable+truncated），stdout 空行不再掩盖半行、stderr 截断同样命中；截断致「无输出」结论不可信时亦报 unavailable 不再静默 active**）；**配置层校验合并去重后总数 ≤64**（R10-#7，与心跳 agents 上限一致，超限拒绝启动）；**同名声明覆盖 PATH 结果**；结果按名称排序稳定上报；版本探测失败/超时→unavailable+说明（发现路径不冒充可调用路径） |
| `internal/config/` | console：`pki_dir`（默认 ./pki）/`tls_cert`/`tls_key`/`tailnet_ip`/`max_connections`；agent：`ca_cert`/`fingerprint`（归一化 64hex 校验）、**`docker_bin`（默认 "docker"，R10-#4）**、`services`（type 枚举 systemd/docker/process，systemd/docker target 白名单字符集 [a-zA-Z0-9_@.-] 且不以 - 开头，process 禁控制字符与开头 -，≤32 条、重名拒绝）、`agent_scan`（`known` 未配置键=默认清单、显式给出=整体替换；custom `command` 非绝对路径拒绝启动；port 1-65535；custom/services 重名拒绝；**合并去重总数 ≤64**（R10-#7））；**console_url 强制 https**（R10-#1，明文 http 配置加载即拒绝）且 https 无 pinning 配置直接拒绝启动 |
| `internal/store/` | migration **v2**：`services`、`ai_agents` 表（UNIQUE(node_id,name)、ON DELETE CASCADE、节点+时间索引；`detail` 列为 SPEC 表结构外新增——unavailable/unknown 的说明必须落库可查，见 §三）；`ReplaceNodeServices`/`ReplaceNodeAgents`：单事务全量替换（UPSERT + 本次未出现行置 **stale** 不删行；空列表全转 stale；节点行已消失返回 false 不复活幽灵行）；**`HeartbeatFull`（R11-F）：metrics+services+agents 单事务原子写入，任一失败整体回滚**；`invokable` 在 store 层强制 false（客户端无任何途径写 1）；附 `ListServices`/`ListAgents` 查询 |
| `internal/registry/` | heartbeat 扩展 `services`/`agents` 数组：`json.RawMessage` 承载三态语义（R11-A 收紧）——**字段缺席 = 无变化不覆盖；显式 `null` = 协议违规 400；显式数组（含 `[]`）= 全量替换**；校验（type/status 枚举、重名、≤64 条、长度上限、控制字符净化）**先于任何写库**——违规整条 400，不落 metrics 也不落服务行（与 M1a 值域校验同口径）；一次心跳的 metrics 与 services/agents 写入经 `store.HeartbeatFull` **单事务**完成，任一失败整体回滚（R11-F） |
| `cmd/agent/` | `register` 新增 `-ca-cert`/`-fingerprint`（缺省取配置值）；`run` 构建固定验证客户端（启动即失败于非 https 地址/缺失 pinning/坏 CA 文件）；接线 ServiceChecker/Scanner |
| deploy/ | 两份样例更新：console（TLS 段 + max_connections + make pki 提示 + **生产 docker 只读 helper/socket-proxy 注意（R10-#4，M1b-b 落地）**）、agent（**https-only 说明（R10-#1）** + ca_cert/fingerprint + services + **docker_bin 及生产 socket-proxy 说明（R10-#4）** + agent_scan 全注释样例及 64 上限） |
| 依赖 | golang.org/x/net v0.59.0（go.sum 已锁定；唯一新增依赖） |

## 二、验收标准逐条自测结果（全部 ✅）

### 1. `make pki` 幂等生成；二次运行不覆盖私钥 ✅

- 首次：`CA generated` + `server cert issued`，输出 ca_cert/server_cert/server_key 路径与 `fingerprint(sha256): d6ed28…5d0e`。
- 二次：日志 `CA kept (idempotent)` + `server cert kept (idempotent)`；`shasum -a 256 server.key ca.key` 两次**字节级一致**（diff 为空）。
- 权限实测：`ca.key`/`server.key` = `-rw-------` (600)，`ca.crt`/`server.crt` = `-rw-r--r--` (644)。
- SAN 实测（openssl s_client 解析）：`DNS:localhost, DNS:MengTianlundeMac-Mini.local, IP Address:127.0.0.1, IP Address:0:0:0:0:0:0:0:1`（E2E 配置未设 tailnet_ip，故无第三 IP；单测覆盖含 tailnet_ip 场景与 SAN 变更重签场景）。

### 2. console HTTPS 启动；curl 分向验证 ✅

- `curl --cacert pki/ca.crt https://127.0.0.1:7799/healthz` → `ok`。
- 不带 cacert：`curl: (60) SSL certificate problem: unable to get local issuer certificate`（自签不被默认信任）。
- `openssl s_client -CAfile ca.crt` → `Verify return code: 0 (ok)`。
- 明文 http 打 https 端口 → Go TLS 栈拒绝：`Client sent an HTTP request to an HTTPS server.`（无明文入口）。

### 3. agent 固定验证上报；篡改指纹 → TLS 错误且日志无密钥材料 ✅

- `meshagent register -console https://… -ca-cert ca.crt -fingerprint <pki 输出>` → 注册成功（state 0600）；`meshagent run` 心跳持续 200（`report failed` 计数 0）。
- 篡改指纹（末两位改 `00`）后运行：持续退避，日志实录 `report failed, backing off err="heartbeat request: Post "https://…": server certificate fingerprint mismatch"`。
- 日志卫生（agent 与 console 双侧 grep）：节点 token 明文、注册 token、`BEGIN.*PRIVATE`、指纹原文**均未出现**。

### 4. LimitListener 生效：max_connections: 3 第 4 条排队 ✅（单测 + 实测双证）

- **单测**（cmd/console）：3 配额占满后第 4 条 TCP 建立成功但不被 Accept（300ms 无服务）；释放一条后立即被服务。另测 Close listener 时阻塞中的 Accept 立即带错返回（R5 回归：无互锁）。
- **E2E**：独立 console 实例 `max_connections: 3`，3 条空闲连接占满配额后 `curl --max-time 2` → rc=28 超时（排队）；关闭一条空闲连接 → 同一 curl 立即返回 `ok`。

### 5. 服务清单落库：active / inactive / unavailable+说明 ✅

macOS 本机声明 2 个 process 型 + 1 个不存在 systemd 型，随心跳落库实查：

```
sqlite> SELECT name,type,target,status,detail FROM services ORDER BY name;
e2e-alpha         |process|m1b-e2e-alpha      |active      |count=2
e2e-beta          |process|m1b-e2e-beta       |inactive    |
fake-systemd-unit |systemd|fake-unit.service  |unavailable |systemctl: exec: "systemctl": executable file not found in $PATH
```

- active 进程以 `bash -c 'exec -a m1b-e2e-alpha sleep 600'` 造真进程，pgrep 计数=2。
- 不存在的 systemd unit 在 macOS（无 systemctl）→ **unavailable + 如实说明**，不编造。
- 全替换/stale 语义 E2E：agent 配置**去掉 services 段**重启 → 三行原样保持（字段缺席≠变化）；配置改 `services: []` 重启 → 三行全部转 `stale`（行保留）。期间还实测了 ai_agents 的同型语义：某轮 agent_scan.services 为空时 `hana-agent` 行转 stale。
- 单测覆盖状态全集：systemd active/inactive/failed/中间态 unknown/无输出/exec 错；docker running/空（inactive）/异常态 unknown/daemon 不可达 unavailable；pgrep exit 0/1/≥2/二进制缺失；固定 argv 防注入断言（`systemctl |is-active|<target>`、docker 恰 5 个 argv）。

### 6. zcode/codex 自动发现且版本正确；不存在的 CLI 不出现 ✅

本机 PATH 实测（`agent_scan.known: [zcode, codex, aider]` + custom 声明）：

```
sqlite> SELECT name,type,version,path,status,invokable FROM ai_agents ORDER BY name;
codex      |cli|codex-cli 0.160.0   |/opt/homebrew/bin/codex      |active  |0
hana-agent |service|              |                             |inactive|0   ← 端口拒绝，detail 如实
my-agent   |cli|my-agent 9.9.9-e2e |/tmp/m1b-a-e2e/bin/my-agent  |active  |0   ← custom 声明
zcode      |cli|zcode-app-cli 3.14.4-31 |/opt/homebrew/bin/zcode |active  |0
```

- 版本与真机 `zcode --version` / `codex --version` 输出逐字一致。
- `aider` 未安装 → **不出现**（不在表里）；不可执行文件同样不出现（单测）。
- invokable 恒 0（store 层强制，客户端无途径写 1）。
- 单测另覆盖：版本超时→unavailable+说明（并实抓修复了孙进程持管道悬挂缺陷）、声明覆盖 PATH、声明文件缺失→unavailable、ctx 取消不编造 active。

### 7. M1a 验收不回退 ✅

- `go test ./... -count=1` 全绿：**97 用例**（M1a 43 + M1b-a 新增 47 + R13 修复新增 4 + R15 修复新增 3）：store 13、registry 17、agent 14、collect 15、agentdisc 9、config 16、pki 10、cmd/console 3。
- `go vet ./...` 零输出；`gofmt -l` 无文件；`make cross` 三平台通过（linux/amd64、darwin/arm64、windows/amd64）。
- 协议回归 over HTTPS 实测：错 token 401、已消费注册 token 401、跨节点 403、坏 JSON 400、services 值域违规 400（且 metrics 不落库）、2MB body 413。
- SIGTERM → `shutting down: draining http connections` → `stopped` → 端口拒绝（rc=7），LimitListener 链上无悬挂（配额占满时 Close 解阻塞由单测钉住）。

### 8. DELIVERY.md 更新 ✅（本文件）

## 三、schema 与协议差异（如实说明）

- `services`/`ai_agents` 在 DESIGN §5 字段外**各增加 `detail` 列**：SPEC 要求 unavailable/unknown 必须带说明，说明不落库就无法在面板/MCP 查证（同 M1a 给 metrics 加 collect_errors 的先例）。
- `ai_agents.invokable` 恒 0 且由 store 层强制：发现 ≠ 可调用（DESIGN §4.2-B）。
- heartbeat 的 `services`/`agents` 为可选字段，语义三分（R11-A 收紧）：**字段缺席 = 无变化**；**显式 `[]` = 全量替换为空（全转 stale）**；**显式 `null` = 协议违规 400 拒绝**（null 不是合法上报，不得与缺席混同）；**非空数组 = 以本次为准 UPSERT（消失者转 stale）**。agent 侧保证配置了 services 就发数组（发现并修复过一处缺陷：空清单曾发 null 被当作缺席，单测与 E2E 均已钉住正确行为）。
- 服务状态枚举：active/inactive/failed/unavailable/unknown（+console 侧 stale）；agent 状态枚举：active/inactive/unavailable/unknown（+stale）。failed 仅 systemd 文本可产生。
- `agent_scan.known` 语义：配置里**未出现该键** = 用内置默认清单；**显式给出（含空列表）= 整体替换**——增删都通过改列表完成（yaml 无法区分「空列表」与「缺省」，故用指针字段显式区分）。
- SAN 变更（如 tailnet_ip 调整）时证书自动重签但 **CA 与私钥不动**：配了 `ca_cert` 的 agent 不受影响；**仅配 fingerprint 的 agent 会因证书更换而失联**，需更新指纹（见 §四.3 部署建议）。

## 四、已知限制与取舍

1. **Windows 真机未测**（本机 macOS）：process 型在裸 Windows 上 pgrep 不存在 → 按语义报 unavailable+说明（诚实上报，不编造）；systemd/docker 型同理属平台缺失。三平台编译通过，真机行为列 M1b-b 上机项。
2. **mTLS 本批不做**（SPEC 约定，M3 再议）：当前为单向 TLS + agent 侧固定验证。
3. **指纹轮换**：服务端证书重签（SAN 变更/到期）后指纹变化。部署建议：agent 以 `ca_cert` 为主（CA 不变则链验证持续有效），`fingerprint` 为辅（双因素）；仅配指纹的节点在证书重签后需带外更新指纹。
4. **服务查询串行执行**（每条 5s 超时，≤32 条）：正常环境毫秒级、远低于 15s 周期；整轮查询有 **10s 总预算**（scanBudget，R11-G）兜底——预算耗尽后剩余条目不再执行，如实报 unknown+`budget exhausted` 说明（不编造状态），单条 5s 超时语义不变；病态环境（全部卡超时）整轮最坏 ~10s（预算到期即在途条目一并截断），远低于 offline_after 60s，M1 规模（≤5 服务/节点）可接受，超规模再并行化。
5. `--version` 输出取首个非空行、截断 128 字节；退出 0 但无输出时版本留空（状态仍 active——探测成功但版本未知，如实呈现）。**例外（R13-#4）**：输出被 64KB 封顶截断时，「无输出/半行」结论不可信——来自截断缓冲且无换行终止的行不作版本值、截断致无输出也不得按 active 放行，一律 unavailable+truncated 说明。
6. 扫描周期 5 分钟、版本超时 5s、端口探测 2s 为固定常量未入配置（SPEC 口径）；已通过 Scanner 字段可注入（测试用）。
7. LimitListener 语义为「超额排队」而非「超额拒绝」：极端连接洪泛下超额连接仍占用内核 accept 队列（容量受 somaxconn 约束），配额内连接不受影响；主动拒绝语义与 handler 层 503（limitConcurrent）互补。
8. agent 心跳 401 不自动重注册（M1a 语义保持）；本批新增的 TLS 失败（指纹不匹配/CA 不信任/验证失败）同样走退避重试并显式报错，等待人工处置——固定验证失败绝不静默降级为明文。
9. **docker 服务查询的生产边界（R10-#4 裁决）**：本批仅将 docker CLI 路径配置化（`agent.docker_bin`，默认 "docker"），普通 docker CLI 等价宿主 root；**生产部署必须以只读 helper 或 socket-proxy 提供受限 docker 访问并把 docker_bin 指向该包装入口，完整接线在 M1b-b 部署批次落地**（当前三节点无 docker daemon，该路径实际未激活）。
10. **明文 http 上报路径已移除**（R10-#1）：agent 与 console 之间只存在 https；本机调试用 `make pki` 产物（SAN 恒含 localhost/127.0.0.1）即可，无需公网证书。
11. systemd 状态判定按退出码+文本双口径（R10-#3）：0=active、3=inactive（failed 文本精化）、其他非零按文本映射；跨 systemd 版本的退出码差异（如旧版 unit 不存在报 3/4）以文本精化兜底，中间态（activating 等）一律 unknown+原文，不编造。

## 五、复现步骤（验收 1-6 可重放）

```bash
make build
# 1) pki 幂等
CONFIG=/tmp/e2e/console.yaml make pki && CONFIG=/tmp/e2e/console.yaml make pki   # 二次 kept，私钥不变
# 2) console HTTPS
cp deploy/console.example.yaml /tmp/e2e/console.yaml   # 改 listen/db_path/pki_dir/registration_tokens
./bin/meshconsole -config /tmp/e2e/console.yaml
curl --cacert /tmp/e2e/pki/ca.crt https://127.0.0.1:7799/healthz          # → ok
curl https://127.0.0.1:7799/healthz                                       # → curl (60)
# 3) agent 固定验证
./bin/meshagent register -console https://127.0.0.1:7799 -token <reg-token> \
  -name <expected_node> -ca-cert /tmp/e2e/pki/ca.crt -fingerprint <pki 输出指纹>
./bin/meshagent run -config agent.yaml    # agent.yaml 样例见 deploy/，含 services/agent_scan
# 5/6) 观察
sqlite3 /tmp/e2e/data/console.db "SELECT * FROM services;" "SELECT * FROM ai_agents;"
# 4) 连接限额：起一个 max_connections: 3 的实例，3 条空闲连接占满后
#    curl --max-time 2 https://… → 超时（排队）；关一条 → 立即 ok
# 3) 篡改指纹：改 agent.yaml fingerprint 末两位 → run → 日志出现 fingerprint mismatch 退避
```

## 六、审查处置记录（R10 + R11 + R13 + R15，2026-10-08）

M1b-a 审查处置：R10（5 阻塞 + 2 建议）+ R11 复审新增裁决（A/C/D/F/G 代码项 + B/E 文档项，其余 7 条与 R10 重合）共 **14 条**全部裁决采纳并修完，逐条修复明细（文件:行号）见 REVIEW.md「修复轮记录 · zcode m1b-a-fix」节；R13 复审确认关闭并新增 **4 条残留**（1/2 阻塞、3/4 缺陷），全部裁决采纳修完，见 REVIEW.md R14 节；R15 复审（修改后通过）5 条采纳意见全部修完，见 REVIEW.md R16 节。修复后验证：`go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **97 用例全绿**（94 基线 + R15 修复新增 3）、`make cross` 三平台通过。对用户可见的行为变化：

- agent 不再支持任何明文 http 上报（含本机调试路径与 state 文件里的旧 http 地址）。
- 仅配置 fingerprint 的 agent 要求服务端下发完整证书链（console 端已自动把 ca.crt 追加进下发链，`meshconsole pki` 生成的部署无需额外操作）；下发链末端信任锚必须**真自签**（R13-#1：同名单异密钥的伪造锚拒绝）。
- pki 目录里 ca.crt/ca.key 任一单独存在、或证书与私钥不配对、或私钥权限非 0600 时，`meshconsole pki` 拒绝执行，需人工处置；**CA 已过期或尚未生效时同样拒绝复用并提示重新 make pki（R13-#2）**；console **启动加载**阶段对服务端私钥同口径校验 0600，权限放宽即拒绝启动并提示 chmod（R15-#3）。
- 服务端证书**尚未生效或既不含 ServerAuth 也不含 ExtKeyUsageAny EKU** 时，`meshconsole pki` 视为不可用并自动重签（同 SAN 变更/到期路径，CA 与私钥不动，R15-#2）。
- 注册 token 纯字符串写法的启动报错**不再回显原值**，只保留拒绝原因与映射写法示例（R15-#1，与指纹错误同口径：凭据材料不进错误信息/日志）。
- 指纹格式错误（配置侧与 register `-fingerprint` 命令行）只提示 hex 格式要求，不回显原值（R13-#3）。
- agent 配置新增 `docker_bin`（默认 "docker"，生产须指向只读 helper/socket-proxy，M1b-b 落地）与 `agent_scan` 合并总数 ≤64 约束。
- 心跳协议：`services`/`agents` 显式 `null` 从「无变化」改为 **400 拒绝**（缺席才是无变化，R11-A）；一次心跳的 metrics 与清单写入合并为单事务（R11-F）。
- 服务查询整轮 10s 总预算：超预算未查的条目如实报 unknown+说明（R11-G）。
- 版本探测：来自截断缓冲且无换行终止的行不作版本值，截断致无输出亦报 unavailable（R13-#4，见 §四.5）。

---

# DELIVERY — M1a 骨架交付说明（历史批次，下文完整保留）

> 交付人：zcode · 2026-10-07（首版同日，R1 审查 13 条处置后重交 R2 版；R3 复审裁决 7 条处置后为 R4 版；R5 终审裁决 3 条处置后为本 R6 版，逐条修复说明见 REVIEW.md R2/R4/R6 节）
> 依据：SPEC-M1a.md（任务书，含 R1 裁决后的 Go 版本修订）+ DESIGN.md v0.7（§3 技术选型、§4.1-A/B、§4.3、§5 数据模型、§7 安全 2/4/5/9 条）
> 状态：**全部验收项自测通过**。未执行任何 git commit（提交权在伦哥）。
> 更正声明：首版自测报告有两处不实（Windows load 结论、constant-time 泛指），R2 版已按实际实现更正，详见 §二.3/§二.5；R4 版按 R3 裁决口径重测并更新（注册 token 强制映射写法、退出时序、空指标 400、连接上限 256、日志 key-only）；R6 版按 R5 裁决处置（**连接总数上限裁剪移除**、退出超时路径语义收紧为「归零才关库 + 极端 ERROR 留痕退出」、到期判定时钟移入事务内），相关小节已重测更新。

## 一、交付物清单

| 项 | 说明 |
|----|------|
| Go module | `github.com/mtl802/meshconsole`，go.mod 声明 **`go 1.26.0`**（本机工具链 go1.26.0）。更正：首版写「go 1.24 / 工具链 go1.24.13」与事实不符；1.26 系 R1 裁决接受值（gopsutil v4.26.9 依赖要求 go≥1.26，SPEC 已同步修订） |
| `cmd/console/` | meshconsole 入口：`-config` 加载 YAML、`/healthz`、agent API、离线巡检与保留清理后台循环、优雅退出（R6 按 R5 裁决收紧：Shutdown 排空 35s → 超时强断 → 等全部 handler **归零**上限 30s，归零才停后台循环/关库；极端 30s 不归零记 ERROR「handler 未排空，存在截断残余风险」留痕退出，明示接受的最坏情形）、请求头 8KB 上限；标准 listener（R5 裁决：连接总数上限 256 裁剪移除，连接限额挪 M1b 与 TLS 一并实现；请求并发防线保留 handler 层 64） |
| `cmd/agent/` | meshagent 入口：`register` / `run` 子命令（签名与 SPEC 一致），注册请求绑 15s 超时 ctx |
| `internal/config/` | console/agent YAML 配置 + 缺省值 + 启动校验（无注册 token 直接拒绝启动；R4 强制化：注册 token 只接受映射写法，缺 `expires_at`/`expected_node` 或已过期均拒绝加载，纯字符串写法解析即报错并提示新写法）；路径 `~` 展开 |
| `internal/registry/` | 认证中间件（注册 token 到期/绑定校验、单轮全量累加比对、节点 token 常量时间复核）+ register/heartbeat 端点（body 严格 EOF 校验、413/400 分级、指标值域校验、空指标且无错误说明 400（R4）、collect_errors 净化；日志只记 key 列表与条数不记 value（R4））+ 并发上限 + 离线巡检循环 + 指标保留清理循环 |
| `internal/store/` | SQLite 访问层（modernc.org/sqlite 纯 Go）：WAL、busy_timeout=5000、单写连接+读池分离、内嵌 migration、nodes/metrics/一次性注册 token 表；注册事务**先验到期与 token 消费再查重名**（R4：到期二次校验在事务内烧 token 前执行；R6：到期判定时钟 now 移入 BeginTx 之后事务内取，单写连接排队跨过期时刻也正确拒绝） |
| `internal/agent/` | 本地 state 持久化（JSON，唯一临时文件创建即 0600 + 原子 rename）、注册/心跳 HTTP 客户端（全部绑 ctx）、心跳循环（指数退避 1s→60s 封顶，抖动 [0.5d, d) 不超顶） |
| `internal/agent/collect/` | gopsutil v4 采集：cpu（异步窗口采样，失败/过期报 null 不回旧值）/mem/disk/net（累计 rx/tx）/uptime/load（Windows 平台显式 null）；错误说明有界化 |
| Makefile | `build` / `cross` / `test` / `vet` / `fmt` / `clean` |
| deploy/ | `console.example.yaml`（R4：注册 token 全映射示例，token/expires_at/expected_node 三字段必填）、`agent.example.yaml`（state 路径与默认一致，注明 ~ 展开） |
| 依赖 | modernc.org/sqlite v1.60.1、shirou/gopsutil/v4 v4.26.9、gopkg.in/yaml.v3（go.sum 已锁定） |

## 二、验收标准逐条自测结果（R4 返工后全量重测）

### 1. `make cross` 三平台编译通过、无 vet 警告 ✅

```
built bin/meshconsole-linux-amd64   bin/meshagent-linux-amd64
built bin/meshconsole-darwin-arm64  bin/meshagent-darwin-arm64
built bin/meshconsole-windows-amd64.exe  bin/meshagent-windows-amd64.exe
```
- `go vet ./...` 零输出；`gofmt -l` 无文件；CGO_ENABLED=0 纯静态交叉编译（windows 产物 `file` 确认 PE32+ x86-64）。
- 体积：meshagent darwin-arm64 **6.8MB**（<10MB 设计目标）；meshconsole 10.5MB（含 modernc sqlite）。

### 2. 本机（macOS）console + agent 全链路 ✅（R4 重测）

- console 启动后 `/healthz` 返回 `ok`（200，无认证，唯一免认证端点）。
- **注册 token 强制映射写法（R4，R3-#4 阻塞项）**——四类坏配置全部拒绝启动（exit 1）：
  - 纯字符串 token：解析即报错 `注册 token 不再接受纯字符串写法（无到期、不绑定节点，等于永久凭据）: "…"\n请改为映射写法，例如: …`（含映射样例）；
  - 映射缺 `expected_node`：`缺少 expected_node（token 必须绑定预期节点名）`；
  - 映射缺 `expires_at` / 空值：`缺少 expires_at（所有 token 必须为映射写法且含到期时刻）`；
  - 已过期：`token 已过期（expires_at=2020-01-01T00:00:00Z），请移除或换新`。
- 绑定 token（`expected_node: e2e-node`）：错名注册 **401**、正名注册 **200**（`node_id=1`）；同 token 二次使用 **401**；console 重启后同 token 再注册仍 **401**（消耗表持久化生效）。`meshagent register` CLI 注册成功，state 落盘权限实测 `-rw-------` (600)。
- agent run（测试间隔 3s）后 SQLite 实查：节点 `online`、首条心跳（cpu 采样窗口等待）`last_success` 保持 NULL，正常心跳后 `last_success` 刷新，metrics 持续落库、`journal_mode=wal`。

### 3. 认证与协议边界（401/403/400/413）✅

| 用例 | 结果 |
|------|------|
| heartbeat 无 Authorization / 错误 token | **401** `{"error":"unauthorized"}`，响应不含任何原因细节 |
| register 无认证 / 错误 token | **401** |
| 注册 token 二次使用（换名） | **401**（用后即废） |
| 已消费 token 撞**已占用**节点名 | **401**（非 409——消费校验先于重名检查，无法借冲突差异探测节点名，R1-#3） |
| 绑定 token 注册**非绑定名** / **绑定名** | **401** / **200**（R1-#4） |
| 含已过期 token 的配置启动 | **拒绝启动**（R1-#4） |
| 纯字符串 token / 缺 `expires_at` / 缺 `expected_node` 的配置启动 | **拒绝启动并提示映射新写法**（R4，R3-#4 强制化） |
| heartbeat 全空指标且无 collect_errors | **400**，不落库（R4）；全空但带错误说明 **200** 且 `last_success` 不刷新 |
| A 节点 token 上报 B 节点名 | **403** `{"error":"forbidden"}` |
| heartbeat `cpu_pct=150` 等越界值 | **400**，不落库（R1-#9） |
| 请求体尾部多余数据（未到 EOF） | **400**（R1-#8） |
| 请求体 2MB（>1MB 上限） | **413**（R1-#8） |
| token 存储 | 库中仅 `token_hash`（64 位 SHA-256 hex），无明文；比对=DB 哈希等值查询定位 + **应用层 `subtle.ConstantTimeCompare` 复核**（注册 token 比对无提前退出）。更正：首版「比对用 constant-time」表述泛指了 DB 查询路径，实际 DB 等值查询非常量时间，应用层复核为 R1-#7 补齐 |

### 4. 杀 agent 60s 后 offline；重启复活且不重复建行 ✅

- 20:25:34 kill agent → 巡检周期（offline_after 60s，巡检间隔 15s）后实查：
  `1|mac-mini|offline`，console 日志 `nodes marked offline count=1`（同时注册后从未心跳的 bound-node 亦如期离线）。
- 用**原 state 文件**重启 agent（不重新注册）→ 数秒内恢复：
  `1|mac-mini|online`，`SELECT COUNT(*) FROM nodes` 仍为 **2**（无重复节点行），node_id 不变，metrics 续写。

### 5. 采集异常字段 null 非 0 ✅（本节含更正声明）

- 实测：agent 启动后**第一条**心跳 CPU 采样窗口未就绪，落库 `cpu_pct` 为 **NULL**，`collect_errors={"cpu_pct":"waiting for first sampling window"}`；第二条起为真实值（6.93…），全程无 0 冒充。agent 重启后首条同样为 NULL+说明（采样窗口重新等待）。
- **更正声明（R1-#1）**：Windows load 首版写「`load.Avg()` 失败 → load1=null」不实——gopsutil v4.26.9 在 Windows 返回模拟 load 值而非 error，该错误路径在 Windows 上根本不会触发。现实现为**平台显式拦截**：`collect.go` 中 `runtime.GOOS=="windows"` 时不调用 gopsutil load，直接 `load1=null` + `collect_errors.load1="load average not available on windows"`；linux/macOS 照常采集。语义由 `TestLoadPlatformGate` 单测钉住；**Windows 真机验证仍未做（本机 macOS），列 M1b 上机项**。
- 采样时效（R1-#2）：CPU 采样失败或超过 staleAfter（2×采样窗口+5s）未更新时，`cpu_pct=null`+失败/过期说明，不回填旧值（单测覆盖三态）。
- 注册层面同等语义：heartbeat 带 `collect_errors` 时入库 NULL 且 `nodes.last_success` 不刷新（区分 last_seen/last_success 双时间戳，DESIGN §4.1）。
- 客户端上报的 `collect_errors` 入库/入日志前经净化：截断 ≤256B/条、去控制字符、≤16 条，超限 400（R1-附，E2E 实测 10KB 消息被截断清洗）；**日志只记 key 列表与条数，value 原文一律不进日志**（R4，E2E 实测 value 中的标记串不出现在日志）。

### 6. 单元测试覆盖核心路径 ✅

`go test -count=1 ./...` 全绿（**43 个用例**，R6 新增 1）：
- **store 层（10）**：migration 版本表与 WAL 生效；注册/按哈希认证；重名拒绝（且不误烧新 token）；注册 token 单次有效（含「已消费+重名仍报已消费」防探测语义）；**注册事务内到期二次校验（已过期/恰在到期时刻拒绝且不误耗 token，R4）**；**到期判定时钟取在事务内——占住唯一写连接令 BeginTx 排队、排队期间过期，释放后按事务实际执行时刻拒绝且不误耗 token（R6）**；心跳刷新 last_seen/last_success 双时间戳语义；null 字段落库为 NULL；离线巡检（含幂等二巡）；保留清理只删过期行。
- **registry 层（13）**：注册认证与 401 不泄露细节；token 用后即废（含撞占用名仍 401）；重名 409 且不烧 token；过期 token 401；绑定 token 错名 401/正名 200；坏 body 400 与尾部数据 400；心跳全流程（含跨节点 403、collect_errors 不推进 last_success、null 入库）；值域校验 7 越界+0 边界；**全空指标且无错误说明 400/带说明 200（R4）**；collect_errors 净化（截断/控制字符/条目超限/空 kv）；离线后老 token 心跳复活不重复建行；节点删除后旧 token 401。
- **collect（6）**：null JSON 序列化（禁 0）；采集失败⟺错误说明不变量；异步 CPU 采样就绪与 0-100 值域；**CPU 采样失败/过期/新鲜三态**；**load 平台门**；错误说明截断与控制字符。
- **config（9）**：缺省/覆盖/校验；**结构化注册 token 解析（引号/裸时间戳/绑定名去空白/空 token 字段拒绝）与过期拒绝**；**纯字符串写法拒绝并提示新写法（R4）**；**缺 expires_at/expected_node 拒绝（R4）**；**~ 展开**。
- **agent（5）**：state 0600 权限、原子写、覆盖写无临时残留、缺 state 报错；**抖动值域 [d/2, d)**。

### 7. console 常驻内存实测 ✅

| 进程 | 实测 RSS | 目标 |
|------|----------|------|
| **meshconsole** | **23.0 MB**（23,568 KB，ps 实测，持续心跳负载下） | <80MB ✅ |
| meshagent | 13.1 MB（13,408 KB） | <30MB（DESIGN §4.3）✅ |

*测量环境：macOS (Apple Silicon) 本机，3s 高频心跳（生产 15s 负载更低）。云机（2核2G）为同构 Go runtime + modernc sqlite，预计相近。*

### 8. 优雅退出实测（R1-#5 → R3-#5 → R5 裁决验证）✅（R6 重测）

SIGTERM console（**在途心跳场景**：3.4KB 心跳体 @1KB/s 慢速传输中收到信号）→ 日志时序实测：`shutting down: draining http connections`（21:18:55.3，请求在途）→ 在途心跳被等完（21:18:57.1 handler 返回 200，`heartbeat with collect errors` 日志齐全）→ `stopped`（21:18:57.4）后进程退出、端口拒连；在途写库实查落盘（metrics 1 行）。时序（R6 按 R5 裁决收紧）：`srv.Shutdown` 排空在途请求（宽限 35s = 读超时 30s+余量）→ 超时则 `srv.Close()` 强断 → WaitGroup 等 handler **全部归零**（上限 30s，归零才继续关库）→ 极端 30s 仍不归零记 ERROR「handler 未排空，存在截断残余风险」留痕后退出（明示接受的最坏情形）→ 之后才停后台循环并关 SQLite。

## 三、与 DESIGN §5 的 schema 差异（如实说明）

- `metrics` 表在 DESIGN §5 字段外**增加** `uptime_s`、`load1`、`collect_errors` 三列：SPEC §3 要求采集上报 uptime/load 及失败说明，落库以便验收核验与 M1b 面板直接使用；全部可空。
- 新增 `consumed_registration_tokens` 表：一次性注册 token「用后即废」需要跨 console 重启持久化（内存态会在重启后复活 token，不可接受）。
- `nodes` 增加 `token_hash`（SHA-256，唯一索引）——认证所需，库中无明文。
- 时间列统一 INTEGER unix 秒（`last_seen`/`last_success`/`created_at`/`ts`），已建索引：`idx_metrics_ts`、`idx_metrics_node_ts`、`idx_nodes_last_seen`。

## 四、已知限制与取舍

1. **重复注册防护=409 硬拒**：agent 丢失本地 state 文件后无法自助恢复，需管理员删 `nodes` 行后重新注册（换取「凭 reg token 不能劫持已有节点身份」的安全属性）。注册 token 用尽需在 console.yaml 增补后重启；token 强制映射写法且必须含未来 `expires_at` 与非空 `expected_node`（R4，DESIGN §4.1-A 落地为配置强约束），到期前需换新 token 并重启 console。
2. **无 TLS**（M1a 约定）：明文 HTTP，console 必须只绑定 Tailnet IP 或 localhost；TLS/证书统一在 M1b。
3. **心跳 401 不自动重注册**：token 失效/吊销后 agent 按 60s 封顶退避持续重试并显式报错，等待人工重注册——遵守「不重复注册」原则。
4. **Windows 平台**：三平台编译通过；load=null 语义为平台显式拦截（不依赖 gopsutil 错误路径）并经单测覆盖，但**未在真机 Windows 实测**（本机 macOS），列 M1b 上机项；`C:\` 默认挂载点为约定路径。
5. **SQLITE_BUSY 规避策略**：单写连接（`SetMaxOpenConns(1)`）+ 读池分离 + WAL + busy_timeout=5000ms 兜底；未做并发压测（M1 规模 3 节点余量充足，压测列入后续）。
6. 采集仅 gopsutil 本机指标；Docker/服务清单/命令通道等均未越界（M1b）。
7. 并发上限 64 为 `/api/agent/*` 全局值（固定常量，未入配置）；M1 规模 3 节点远低于此，M1b 若接 Web 面板再评估是否配置化。连接总数上限按 R5 裁决**裁剪移除**（R4 曾加 256，实际规模 3 节点+1 客户端无现实意义，防御过度是设计债）；连接限额与 TLS 一并挪 M1b 实现（DESIGN §7-4/§7-9 连接部分）。

## 五、复现步骤（验收 2/3/4/8 可一键重放）

```bash
make build
# console（样例配置拷贝后必改：每个 token 为映射写法，token/expires_at/expected_node 三字段必填）
cp deploy/console.example.yaml /tmp/console.yaml   # 修改 registration_tokens 与 db_path
./bin/meshconsole -config /tmp/console.yaml
# agent
./bin/meshagent register -console http://127.0.0.1:7700 -token <一次性token> -name <expected_node 绑定的节点名>
./bin/meshagent run -config deploy/agent.example.yaml   # state 默认 ~/.meshagent/state.json，~ 会展开
# 观察
sqlite3 <db_path> "SELECT * FROM nodes; SELECT COUNT(*) FROM metrics;"
curl -s http://127.0.0.1:7700/healthz
# 401/403/400/413 与离线复活：kill agent 后等 ≥75s 查 nodes.status，再用原 state 重启 agent
```

## 五、部署验证（2026-10-08，腾讯云 1.13.158.180 + Mac mini 跨机）

**结论：M1b-a 全部核心功能在真实环境验证通过。** 部署形态：console systemd 常驻（/opt/meshconsole，MemoryMax 256M，ProtectSystem=strict），agent systemd 常驻（cloud-agent）+ Mac mini 经 SSH 隧道注册（mac-mini，node_id=3）。

| # | 验证项 | 结果 |
|---|--------|------|
| 1 | CA 生成持久化 + SAN（127.0.0.1/公网 IP/主机名） | ✓（tailnet_ip 配置生效） |
| 2 | console HTTPS systemd 部署 + 幂等重启 | ✓ |
| 3 | agent 注册：one-shot token/到期/expected_node 绑定 | ✓（重放 401，消费即废） |
| 4 | 心跳 + metrics 落库（15s 间隔，cpu/mem/disk/net/uptime/load） | ✓（首轮 cpu 如实标 waiting，不编数） |
| 5 | 服务采集 systemd 型（headscale/meshconsole/dsh-tunnel） | ✓ 全 active |
| 6 | 服务采集 docker 型（nextcloud-app） | ✓ active（docker ps 只读路径） |
| 7 | 服务采集 process 型（mac meshagent） | ✓ 落库（inactive 判定见观察点③） |
| 8 | AI agent 发现（custom zcode/codex，版本探测，invokable 恒 false） | ✓（zcode 3.14.4-31 / codex 0.160.0，invokable=0） |
| 9 | 离线判定（无心跳 60s → offline） | ✓（node1 复现） |
| 10 | 401（假注册 token/假 node token）| ✓ |
| 11 | 403（跨节点心跳：node2 token 报 node1 名） | ✓ |
| 12 | 400（services 显式 null/全空 metrics/严格 JSON） | ✓ |
| 13 | 多节点并存（3 节点在线状态独立） | ✓ |
| 14 | agent SIGTERM 重启恢复（state 续接不重注册） | ✓ |

**观察点（非阻塞，转 M1b-b 候选）**：① `--version` flag 两二进制均缺；② agent 启动日志 version 显示 `31eafac-dirty`（M1a 旧 commit 号，ldflags 注入未随 M1b-a 更新）；③ process 型采集对 agent 自身进程判 inactive（疑似 exclude-self 设计，需核对源码确认意图）。

**部署遗留**：console listen 127.0.0.1:7700（公网未放行，Mac 经隧道接入）；公网直连需腾讯云防火墙放行 TCP 7700 并将 listen 改绑（M1b-b Web 面板批次一并定稿）。
