# DELIVERY — M1a 骨架交付说明（R6 返工版）

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
