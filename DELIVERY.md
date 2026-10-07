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
