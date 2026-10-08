# REVIEW — M1a 审查记录

## R1 · codex 审查（2026-10-07 19:58）

**结论：修改后通过。** 6 阻塞 + 6 建议。审查人核对以代码为准，指出 DELIVERY 自测报告两处不实（Windows load 结论、constant-time 泛指）。

### 裁决（Hana）

| # | 意见 | 级别 | 裁决 | 理由 |
|---|------|------|------|------|
| 1 | Windows load 违反 null 约定（gopsutil v4.26.9 返回模拟 load 非 error） | 阻塞 | **采纳** | null 语义是 SPEC 验收项，按平台显式返回 null+说明 |
| 2 | CPU 采样失败仍上报旧值、不记错误、不校验时效 | 阻塞 | **采纳** | 违反"失败字段报 null"与 last_success 语义 |
| 3 | 已消费 token 先查重名后查消费 → 可探测节点名 | 阻塞 | **采纳** | 信息泄露，统一先验 token 消费状态 |
| 4 | 注册凭据无到期时间/预期身份绑定 | 阻塞 | **采纳** | DESIGN §4.1-A 明确要求"短期、一次性、绑定预期节点" |
| 5 | 优雅退出不等待 HTTP 排空即关库 | 阻塞 | **采纳** | 数据完整性 |
| 6 | go.mod 1.26.0 vs SPEC 声明 1.24 | 阻塞 | **裁决接受 1.26** | SPEC 的 1.24 是示例值（DESIGN §3 原文"固定受支持的版本，如 1.24.x"）；gopsutil v4.26.9 依赖要求 go≥1.26；1.26 为当前受支持版本。修改 SPEC 表述，DELIVERY 相应更正 |
| 7 | constant-time 声明不完整 | 建议 | **采纳** | DB 哈希等值查询补应用层 constant-time，成本低 |
| 8 | 请求体未校验 EOF、缺连接/并发上限 | 建议 | **采纳** | 对应 DESIGN §7-4/9 限额条款 |
| 9 | 心跳指标值域未校验（CPU 越界/负字节） | 建议 | **采纳** | null⟺错误说明一致性 + 值域校验 |
| 10 | 退避抖动超 60s 封顶、请求未绑 context | 建议 | **采纳** | 与 DESIGN 声明一致化 |
| 11 | ~ 不展开、样例配置与默认注册路径不一致 | 建议 | **采纳** | 可用性 |
| 12 | state 临时文件固定名竞争+先写后 chmod | 建议 | **采纳** | 凭据安全 |
| 附 | registry.go:315 日志完整记录客户端错误说明 | 建议 | **采纳** | 限制长度+脱敏 |

**返工上限规则**：同一条意见修两次不过 → 停止循环回伦哥对齐。

## R2 · zcode 修复（2026-10-07 完成）

**13 条全部处置完毕**（6 阻塞修完、6 建议修完、1 附带修完；#6 按裁决 go.mod 保持 1.26.0，仅更正文档）。修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test ./...` 38 用例全绿、`make cross` 三平台通过，并重做了 SPEC 验收自测（见 DELIVERY.md §二，两处不实声明已更正）。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | `internal/agent/collect/collect.go:136-141,181`：新增 `loadNotAvailable(goos)` 平台门，`Collect()` 里 `runtime.GOOS=="windows"` 时**不调用** gopsutil `load.Avg()`，直接 `Load1=nil` + `Errors["load1"]="load average not available on windows"`——v4.26.9 在 Windows 返回模拟值而非 error，不能依赖其错误路径。null+说明语义由 `TestLoadPlatformGate` 钉住；Windows 真机验证仍列 M1b 上机项 |
| 2 | `collect.go:66-70,97,118-131`：`cpuSample` 增加 `err` 字段与 `staleAfter`（采样窗口 2 倍+5s）。采样循环失败时 `Store` 失败状态（`:97`）；`collectCPU()`（`:118-131`）三种情形——无样本「waiting」/最近采样失败/样本过期——一律 `CPUPct=nil`+说明，**不再沿用旧值**。`TestCollectCPUFailureAndStaleness` 覆盖三态 |
| 3 | `internal/store/store.go:222-247`：`RegisterNode` 事务内**先**执行一次性 token 消费校验（`:234` `INSERT OR IGNORE` 判 `ErrTokenUsed`），**再**查重名（`:245`）。已消费 token 无论撞不撞名一律 401，无法借「409/401 差异」探测节点名；未消费 token 撞名仍 409 且不烧 token。单测/HTTP 层均补了「已消费 token + 已占用名 → 401」用例 |
| 4 | `internal/config/config.go:18-66`：`RegistrationToken` 增加 `expires_at`（RFC3339，`FlexTime` 兼容带引号/裸时间戳）与 `expected_node`，YAML 纯字符串写法兼容保留；`config.go:163` 启动时已过期 token 拒绝加载。`internal/registry/registry.go:127-131` 中间件到期即 401；`registry.go:269-274` 预期节点绑定——token 只对绑定名有效，其他节点名一律 401 不泄露 token 有效性。E2E 实测：过期配置拒绝启动、绑定 token 错名 401/正名注册成功 |
| 5 | `cmd/console/main.go:84-104`：退出路径加 `shutdownDone` 通道——信号触发 `srv.Shutdown` 排空在途请求，`ListenAndServe` 返回后主流程 `<-shutdownDone` **等排空完成再停后台循环、关 SQLite**；错误退出路径同样先 `stop()`+关库再退出。E2E 实测 SIGTERM：日志 `draining http connections` → `stopped` 后进程才退出 |
| 6 | go.mod/go.sum **不动**（保持 `go 1.26.0`，裁决接受）。`SPEC-M1a.md:14-15` 表述由「声明 1.24」改为「声明 1.26」并注明理由（gopsutil v4.26.9 要求 go≥1.26，1.26 为当前受支持版本，原 1.24 为示例值）；DELIVERY.md §一 更正错误声明（原「go.mod 声明 go 1.24、工具链 go1.24.13」与事实不符，实为 go.mod `go 1.26.0`、工具链 go1.26.0） |
| 7 | `registry.go:108-116`：注册 token 比对改为全量累加 `ok |= ConstantTimeCompare`（无提前退出）；`registry.go:155`：节点 token 在 DB 哈希等值查询命中后**再经应用层 `subtle.ConstantTimeCompare` 复核**，接受路径常量时间。DELIVERY 措辞同步改为精确描述 |
| 8 | `registry.go:186-204`：`decodeJSONStrict`——解码后再 `Decode(&struct{}{})` 必须 `io.EOF`，尾部多余数据 400；`*http.MaxBytesError` 单列 **413**（`registry.go:484-489`）。`registry.go:36,173-185,214-216`：新增 `limitConcurrent(64)` 并发上限（超限 503），`/api/agent/*` 两路由套用；`cmd/console/main.go:79` `MaxHeaderBytes: 8KB`。对应 DESIGN §7-4/§7-9 |
| 9 | `registry.go:355-381`：`validateMetrics`——`cpu_pct ∈ [0,100]`（含 NaN/Inf 拒绝）、`load1 ≥ 0`、mem/disk/net/uptime 各整型非负，越界整条 400 不落库。`TestHeartbeatValueRange` 覆盖 7 种越界 + 0 值合法边界 |
| 10 | `internal/agent/runner.go:59-61,73-76`：jitter 定义改为 `[d/2, d)`，并在 `Run` 里对 `next` 再封顶 `backoffMax`——重试间隔永不超 60s（原 `[0.5d,1.5d)` 峰值 90s）；`internal/agent/client.go:24,78` register/heartbeat 改 `http.NewRequestWithContext`，`cmd/agent/main.go:81` 注册请求绑 15s 超时 ctx。`TestJitterRange` 区间更新 |
| 11 | `internal/config/config.go:114-129,150,207`：新增 `ExpandHome`（`~` 与 `~/`、Windows `~\`，`~user` 不支持原样返回），`agent.state_file` 与 `console.db_path` 加载时展开；`deploy/agent.example.yaml` 注明 ~ 展开且与默认 `$HOME/.meshagent/state.json` 一致（`DefaultStatePath` 改 `filepath.Join` 同口径）。`TestExpandHome`/`TestAgentStateFileTilde` 钉住 |
| 12 | `internal/agent/state.go:56-63`：`SaveState` 临时文件改 `os.CreateTemp`（唯一随机名，消除固定名竞争/符号链接注入），创建即 0600 + `tmp.Chmod(0o600)` 防御 umask，再写再 rename；失败路径清理。`TestStateRoundTripAndPerms`/`TestSaveStateOverwrite` 用 glob 验证无 `.tmp*` 残留 |
| 附 | `registry.go:384-419,478`：新增 `sanitizeCollectErrors`/`sanitizeErrStr`——入库与日志前截断（key/value 各 ≤256B）、去控制字符（rune 边界安全）、条目数 ≤16，超限/空 key/空 value 整条 400；`agent_version` 同样净化（≤64B）。`:478` 日志只输出净化后的 map。E2E 实测 10KB+控制字符消息被截断清洗入库 |

**验证记录**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（38 用例：store 8、registry 12、collect 6、config 7、agent 5）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。全链路自测数据见 DELIVERY.md §二。

## R3 · codex 复审（2026-10-07 20:40）

**结论：修改后通过。** R1 十三条中 7 条确认关闭、1 条按裁决关闭、5 条部分关闭/未关闭。

### 裁决（Hana）

| 项 | 级别 | 裁决 | 修复口径 |
|----|------|------|----------|
| #4 纯字符串 token 永久凭据 | 阻塞 | **采纳，强制化** | 配置加载即拒绝：所有注册 token 必须映射形式且含有效 expires_at + 非空 expected_node；纯字符串写法启动报错提示新写法（M1 单管理员，无兼容负担） |
| #5 Shutdown 5s 截断慢请求 | 阻塞 | **采纳** | Shutdown ctx 35s（读超时 30s+余量）→ 超时 srv.Close() 强断 → WaitGroup 等 handler 全部返回（上限 60s）→ 才停后台/关库 |
| 到期检查在 body 读取前，跨到期时刻窗口 | 建议 | **采纳** | 消费前（事务内）二次校验 expires_at |
| 空指标+无错误说明计为成功 | 建议 | **采纳** | metrics 全空且 collect_errors 为空 → 400（正常 agent 心跳必带指标或错误说明） |
| #7 第二轮 token 查找提前 break | 建议 | **采纳** | 与第一轮一致全量累加 |
| #8 未限连接总数 | 建议 | **采纳** | net.Listener 包 semaphore（上限 256） |
| 附：日志仍记错误原文片段 | 建议 | **采纳** | 只记 key 列表+条数，不记 value 原文 |

**返工上限规则**：#4、#5 已是第二轮修复，若本轮再不过 → 停止循环回伦哥对齐。

## R4 · zcode 修复（2026-10-07 完成）

**R3 裁决 7 条全部按口径修完**（2 阻塞 + 5 建议/残余）。修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **42 用例全绿**（原 38 + 新增 4：config 纯字符串拒绝、config 缺字段拒绝、store 事务内过期、registry 空指标）、`make cross` 三平台通过；E2E 重测数据见 DELIVERY.md §二（R4 版）。

| 项 | 修复说明（文件:行号 · 怎么修） |
|----|------|
| #4 阻塞：纯字符串 token 强制化 | `internal/config/config.go:65-79`：`RegistrationToken.UnmarshalYAML` 删除纯字符串分支——标量写法解析层即报错并附映射写法样例（`config.go:44-48` `tokenExampleYAML`）；`config.go:168-186` 启动校验三连：缺 `expires_at`（`:173`）/ 已过期（`:177`）/ 缺 `expected_node`（`:183`，加载时 TrimSpace 归一 `:181`）一律拒绝加载。`deploy/console.example.yaml:11-26` 同步改为全映射示例并注明三字段必填。测试：`config_test.go:69`（纯字符串拒绝+提示文案四关键词）、`:87`（缺 expires_at/空白 expires_at/缺 expected_node 三态）、`:116`（映射解析+裸/引号时间戳+绑定名去空白+空 token 字段拒绝） |
| #5 阻塞：优雅退出时序 | `cmd/console/main.go:86-107` 新增 `drainHTTP` 三段式：① `srv.Shutdown(ctx 35s)`（`:28` `shutdownGrace`=读超时 30s+余量）→ ② 超时 `srv.Close()` 强断（`:96-98`）→ ③ WaitGroup 等全部 handler 返回、上限 60s（`:28-30` `handlerWaitCap`，`:100-106` select 兜底告警）。在途计数：`main.go:144-149` 根 handler 中间件 `inFlight.Add/Done` 包住全部路由。主流程 `main.go:157-166,185`：信号 goroutine 走 `drainHTTP` 后 `<-shutdownDone` 才 `stop()` 停后台循环、`defer st.Close()` 关库；`Serve` 失败路径（`:177-182`）同样先 drain 再关库。E2E 实测 SIGTERM：`draining http connections` → `stopped` 后进程才退（agent 心跳在途场景复测通过） |
| 到期检查：事务内二次校验 | `internal/store/store.go:28-29` 新增 `ErrTokenExpired`；`store.go:231-241` `RegisterNode` 增加 `regTokenExpiresAt` 参数，事务内**烧 token 前**校验 `now >= expires` 即回滚拒绝（过期不误耗，测试 `store_test.go:107` 覆盖已过期/恰在到期时刻/过期拒绝后同 token 仍可用）。`internal/registry/registry.go:286-291` 把 `entry.expiresAt` 换算 unix 秒随事务带入，`:299` `ErrTokenUsed|ErrTokenExpired` 统一 401 不泄细节——关闭「中间件检查在读 body 前、body 传输跨到期时刻」的窗口 |
| 空指标：全空且无 collect_errors → 400 | `internal/registry/registry.go:357-361` `metricsAllEmpty`（九个指标指针全 nil）；`registry.go:450-455` `metricsAllEmpty && len(CollectErrors)==0 → 400` 不落库（metrics 字段整个缺省同样命中）。全空但带错误说明仍 200（全体采集失败是合法心跳），`last_success` 不刷新。测试 `registry_test.go:349` 覆盖 400×2 + 不落库 + 带说明 200 + last_success 保持 NULL |
| #7 残余：第二轮查找提前 break | `internal/registry/registry.go:109-124`：第一、二轮合并为**单轮全量累加**——`ok |= ConstantTimeCompare` 之外，命中条目随累加一并记取（`eq==1` 时记 entry），循环无任何提前退出，比对次数与条目序号无关 |
| #8 残余：连接总数上限 256 | `cmd/console/main.go:26` `maxConns=256`；`main.go:52-80` `limitListener`/`limitConn`——Accept 成功即占信号量槽位，连接关闭释放（`sync.Once` 防 net/http 与底层断开双 Close 重复释放，不吞底层错误）；打满后新连接在内核 backlog 排队。`main.go:176-177` `srv.Serve(newLimitListener(...))` 取代 `ListenAndServe` |
| 附残余：日志只记 key 与条数 | `internal/registry/registry.go:495-504`：collect_errors 告警行改为 `"count", N, "keys", [排序 key 列表]`，value 原文一律不进日志（入库副本仍在 `metrics.collect_errors` 按需排查）。E2E 实测：value 含 `TOPSECRET-value` 标记，日志仅出现 `count=1 keys=["cpu_pct"]` |

**验证记录**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（**42 用例**：store 9、registry 13、collect 6、config 9、agent 5）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。E2E：纯字符串/缺绑定/缺到期/已过期四类配置全部拒绝启动（报错含新写法提示）；绑定 token 错名 401/正名注册成功/复用 401（跨 console 重启仍 401，消耗表持久化生效）；空指标无说明 400、带说明 200；SIGTERM 排空后关库。

## R5 · codex 终审（2026-10-07 21:03）

**结论：不通过。** #4/空指标/#7/日志脱敏确认关闭；3 项缺口。

### 裁决（Hana，按伦哥授权自主裁决）

| 项 | 级别 | 裁决 |
|----|------|------|
| #5 Shutdown 超时路径仅告警即关库 | 阻塞 | **采纳**：Close() 强断后 handler 因网络断开快速返回，WaitGroup 等待（30s 上限）归零才关库；极端不归零则记 ERROR 留痕退出（如实写明残余风险） |
| 连接上限与 Shutdown 互锁（Accept 卡信号量） | 阻塞 | **裁剪**：M1 移除连接总数上限（limitListener/limitConn 删除），保留 handler 并发 64。理由：实际规模 3 节点+1 客户端，256 上限无现实意义，防御过度是设计债；DESIGN §7-9 连接限额挪至 M1b 与 TLS 一并实现 |
| 事务内到期校验取时间在 BeginTx 前 | 建议 | **采纳**：now 移入事务内取 |

触发返工上限规则（#5 两轮未过），已按伦哥授权（2026-10-07 晚：流程性决策自主裁决）选择方案 A 继续，不再打断伦哥。

## R6 · zcode 修复（2026-10-07 完成）

**R5 裁决 3 条全部按口径处置完毕**（1 阻塞 + 1 裁剪 + 1 建议）。修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **43 用例全绿**（原 42 + 新增 1：事务内取时钟钉住用例）、`make cross` 三平台通过；E2E 复测（SIGTERM 在途排空）见 DELIVERY.md §二.8。

| 项 | 处置说明（文件:行号 · 怎么修） |
|----|------|
| #5 阻塞：Shutdown 超时路径语义 | `cmd/console/main.go:29` `handlerWaitCap` 60s → **30s**（裁决口径）。`main.go:47-73` `drainHTTP` 按裁决时序收紧：① `srv.Shutdown(ctx 35s)`（`:54-56`）→ ② 超时 `srv.Close()` 强断（`:57-61`，原样保留）→ ③ `inFlight.Wait()` 等 handler **全部归零**，上限 30s（`:62-72`）——归零才返回，主流程 `:155` `<-shutdownDone` 之后才停后台循环/关库（`Serve` 失败路径 `:147-154` 同口径）；极端 30s 仍不归零：`:71` `log.Error("handler 未排空，存在截断残余风险", …)` 留痕后返回退出，明示接受的最坏情形——修复前该分支仅 Warn 即放行关库，为 R5 判定的缺口本体。E2E 实测：SIGTERM 时心跳请求在途（~3.4s 慢速传输），console 等其在途处理完（handler 返回 200、落库 1 行 metrics）才输出 `stopped` 退出，退出后端口拒连 |
| 裁剪：连接总数上限移除 | `cmd/console/main.go`：删除 `maxConns` 常量与 `limitListener`/`newLimitListener`/`limitConn` 全部代码（原 `:25-26,48-80`）；`main.go:138` 恢复标准 `net.Listen` 裸 listener，`:147` `srv.Serve(ln)` 直接挂裸 listener（原 `newLimitListener` 包装）。handler 层 `limitConcurrent(64)` 不动（`internal/registry/registry.go:171-172`，`:213`/`:215` 两路由套用）。cmd/console 无测试文件，无对应测试需删改。依据 R5 裁决：实际规模 3 节点+1 客户端，256 上限无现实意义，防御过度是设计债；连接限额挪 M1b 与 TLS 一并实现（DESIGN §7-4/§7-9 连接部分） |
| 建议：到期校验时钟入事务 | `internal/store/store.go`：`RegisterNode` 的 `now := time.Now().Unix()` 由 `BeginTx` 之前移到**之后**事务内（`store.go:234` BeginTx，`:240` now，`:241-243` 到期判定）——单写连接（`SetMaxOpenConns(1)`）下 BeginTx 可能排队等锁，now 按事务实际执行时刻取值，排队跨过到期时刻也正确拒绝且不误耗 token；文档注释同步（`store.go:228-230`）。新增钉住测试 `internal/store/store_test.go:126-157` `TestRegisterNodeNowInsideTx`：`BeginTx` 占住唯一写连接（`:135`）令 RegisterNode 的 BeginTx 排队，到期时刻定在排队期间（`:139`），700ms 后释放锁（`:145-148`），断言 `ErrTokenExpired`（`:149-151`）——修复前写法 now 在排队前取值则该用例必失败（漏拒），钉死修复；`:152-156` 复验排队期过期拒绝不误耗 token（同 token 换有效到期注册成功） |

**验证记录**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（**43 用例**：store 10、registry 13、collect 6、config 9、agent 5）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。E2E：标准 listener 下 register 200 正常；SIGTERM 在途心跳场景排空完整（在途写库不丢）。未执行任何 git commit。

## R7 · codex 复审（2026-10-07 21:25）

**结论：修改后通过。** R5 三项缺口全部关闭，两项一致性修正：

| 项 | 裁决 |
|----|------|
| 两路由独立 semaphore 合计 128 | 采纳：共享单个 64 配额 |
| DESIGN.md §7-9 裁剪未同步 | 采纳：注明 M1a 仅请求并发限额，连接总数限额随 TLS 延至 M1b |

## R8 · zcode 修复（2026-10-07 完成）

**R7 两项一致性修正全部按裁决修完。** 修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **43 用例全绿**（用例集与 R6 相同，纯接线/文档修改无新增测试）。未执行任何 git commit。

| 项 | 修复说明（文件:行号 · 怎么修） |
|----|------|
| 两路由独立 semaphore 合计 128 → 共享单个 64 | `internal/registry/registry.go:212-217`：`RegisterRoutes` 入口创建**单个** `sem := make(chan struct{}, maxInFlight)`（`:213`），register 与 heartbeat 两路由均传入同一 `sem`（`:215`/`:217`）——注册+心跳在处理请求合计不超 64 配额；`limitConcurrent` 签名由 `(n int, …)` 改为 `(sem chan struct{}, …)` 并删除函数内各自 `make`（`:171-183`），超限 503 语义不变 |
| DESIGN.md §7-9 裁剪未同步 → 补范围注记 | `DESIGN.md:464-465` §7-9 资源限额补注：M1a 实现范围仅请求体 ≤1MB 与 handler 并发 ≤64（注册+心跳共享配额），连接总数限额随 TLS 于 M1b 实现；`DESIGN.md:447-448` §7-4「请求体/连接数限额」处同步同口径注记——两处与 R5 裁剪（连接总数上限移除）不再矛盾 |

## R10 · codex 审查 M1b-a（2026-10-08 00:20）

**结论：修改后通过。** 5 阻塞 + 2 建议。正向：netutil.LimitListener 用了官方实现（R5 教训吸收）、target 白名单/绝对路径/固定 argv 落实、M1a 语义无回退、无越界。

### 裁决（Hana）

| # | 意见 | 级别 | 裁决 |
|---|------|------|------|
| 1 | TLS 可绕过：HTTP 地址放行 + fingerprint-only 跳过链验证 | 阻塞 | **采纳**：agent 强制 HTTPS（HTTP 地址启动报错）、CheckRedirect 拒绝降级、指纹验证叠加证书链+有效期+主机名校验 |
| 2 | CA 幂等覆盖路径：ca.crt 缺失但 ca.key 在时重生成覆盖 | 阻塞 | **采纳**：任一文件单独存在视为不完整拒绝生成；复用文件校验 0600 |
| 3 | systemd 状态误报：非零退出全归 unavailable | 阻塞 | **采纳**：区分 is-active 状态退出码（0=active/3=inactive 等）与执行环境错误 |
| 4 | Docker 权限边界：直接用普通 docker CLI | 阻塞 | **采纳+范围裁决**：本批改为可配置 docker bin 路径 + deploy 文档明确生产部署必须接 socket-proxy/受限 helper（M1b-b 部署落地）；理由：当前三节点无 docker daemon，该路径实际未激活，完整接线随部署做，配置化先行不留裸接口 |
| 5 | 探测输出无内存上限（无限 Builder） | 阻塞 | **采纳**：写入阶段限额（capped buffer）+ 截断标记，符合 §7-9 |
| 6 | 端口探测不响应 ctx 取消 | 建议 | **采纳**：DialTimeout → DialContext |
| 7 | 发现配置容量与心跳 64 项限制不一致 | 建议 | **采纳**：合并去重后总数 >64 拒绝启动 |

## R11 · codex 复审 M1b-a（2026-10-08 凌晨，值班员执行）

**背景**：R10 裁决后上一会话中断，STATE.md 未更新；值班员按流程重跑 codex 审查（read-only 沙盒），14 条意见（7 阻塞 + 7 建议），tokens 95,972。与 R10 交叉比对：7 条与 R10 重合（不重复裁决，按 R10 口径执行），7 条为新增，逐条抽验源码后裁决如下。

### 新增意见裁决（值班员，按伦哥既有授权自主裁决流程性问题）

| # | 意见 | 级别 | 裁决 |
|---|------|------|------|
| A | registry.go:347 `*[]serviceIn` 指针字段：JSON `null` 与字段缺席在服务端合并为「无变化」，违反任务书审查重点「服务端不得把 null 当作字段缺席」；DELIVERY 自述「null=无变化」与任务书冲突 | 阻塞 | **采纳，以任务书为准**：JSON 层区分缺席/null/数组三态——缺席=无变化、显式 `[]`=全量替换、显式 `null`=400 拒绝（协议违规）；补 services/agents 三态回归测试 |
| B | runner.go:151 services 配置段整个缺席时不发清单，「删光配置后历史服务永不转 stale」 | 建议 | **不采纳代码改动**（字段缺席=无变化是协议语义，显式 `[]` 全转 stale 已实现并 E2E 钉住）；**采纳文档补充**：deploy/agent.example.yaml 注明「移除全部受管服务时须显式写 `services: []`」 |
| C | config.go:339 指纹错误 `%q` 回显原值，与 DELIVERY「错误信息不含指纹值」自述矛盾 | 建议 | **采纳**：错误信息不回显原值（含 NormalizeFingerprint 全路径） |
| D | pki.go:208 复用既有 CA 时未校验证书/私钥配对与有效期，可能报告成功却产出不可验证证书 | 建议 | **采纳，与 R10 #2 合并执行**：复用前校验 ca_cert/ca_key、server_cert/server_key 配对（公钥比对）+ 私钥权限 0600，不匹配拒绝 |
| E | pki.go:406 并发 Ensure 无跨进程锁，可覆盖私钥或 CA 配对不一致 | 建议 | **不采纳代码改动**（单管理员 make pki 串行执行，为极低概率场景加锁是设计债）；**采纳文档注明**「勿并行运行 make pki」 |
| F | registry.go:640+ heartbeat/services/agents 三次独立事务，中途失败留半轮数据 | 建议 | **采纳**：同一心跳全部写入合并为单一事务（或等效原子路径），任一失败整体回滚 |
| G | collect/services.go:74 服务查询串行，最多阻塞心跳 160s，超过 offline_after 60s | 建议 | **采纳轻量版**：整轮查询设总预算（10s ctx），超预算未完成的服务报 unknown+说明（如实），不引入并发复杂度 |

**修复轮范围**：R10 全部 7 条 + 本表 A/C/D/F/G 代码项 + B/E 文档项 → 派 m1b-a-fix。验证口径不变：go vet / gofmt / go test -count=1 全绿 + make cross 三平台 + M1a 验收不回退。

## 修复轮记录 · zcode m1b-a-fix（2026-10-08 完成）

**R10 全部 7 条 + R11 新增裁决 A/C/D/F/G（代码）与 B/E（文档）共 14 条全部按口径修完。** 修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **90 用例全绿**（修复前基线 75 + 本轮新增 15）、`make cross` 三平台通过。未执行任何 git commit。

### R10 七条修复（文件:行号 · 怎么修）

| # | 修复说明 |
|---|---|
| 1 | 阻塞：TLS 可绕过。`internal/agent/client.go:34-66` `NewHTTPClient` 重写——`url.Parse` 后 scheme 非 https（含 http/其他 scheme/无 scheme）一律报错（`:42-44`，明文路径彻底移除；register/run/state 旧地址共用此入口全被拦）；`:52-63` `CheckRedirect` 拒绝向非 https 降级（报错含 `downgrade`），上限 3 跳防重定向环。指纹验证重做（`:73-122`）：ca_cert 模式不变（RootCAs 链+有效期+主机名，配指纹叠加复核 `:139-150`）；**仅指纹模式 `:95-105`——`InsecureSkipVerify` 仅为绕过系统信任库（自签 CA 不在其中），`verifyServedChain`（`:124-161`）以服务端握手下发的证书链重建完整 x509 验证（自签锚+签名链+有效期+ExtKeyUsage+主机名全执行），再叠加叶子 SHA-256 常量时间指纹比对**——不再是叶子字节单比对。两处关键防御：① `ServerName` 显式取自 console_url 主机名（`:74`），主机名校验不被跳过；② `:137-139` 下发链末端必须自签——Go x509 对「叶子自身在信任池」有直通捷径（`opts.Roots.contains(leaf)→{leaf}` 即过，签名链验证被架空；已用独立探针程序实测钉住），非自签锚直接拒绝。服务端配套：`internal/pki/pki.go:178-198` `ServerTLSConfig(cert,key,caPath)` 新增第三参，ca 确为签发方（CheckSignatureFrom）时把 CA 追加进下发链，仅指纹的 agent 才有信任锚；`cmd/console/main.go:220` 传 `pki_dir/ca.crt`（CA 缺失/不匹配保持原样不阻塞启动）。配置层：`internal/config/config.go:566-570` console_url 非 https 拒绝加载。测试 `internal/agent/client_test.go`：`:35` http/ftp/无 scheme 全拒；`:188` CA 签发四态——完整链过、leaf-only 拒（指纹匹配也不行，证明非字节比对）、过期拒、主机名不匹配拒；`:251` http 重定向拒；原自签 httptest 场景（自签锚=平凡链）全保留通过 |
| 2 | 阻塞：CA 幂等覆盖路径。`internal/pki/pki.go:224-249` `loadCA` 按两文件存在性三分：均缺失→`errNoCA`（可安全新生成）；**任一单独存在→报「CA 不完整」拒绝生成并提示人工处置**（`:233-238`；修复前缺 ca.crt 而 ca.key 在会走 errNoCA 重新生成并覆盖 ca.key——审查指出的缺陷本体）；均在→解析校验复用。`Ensure`（`:70-133`）复用路径校验私钥权限：ca.key（`:103`）/server.key（`:126`）非 0600 报错（`requireKeyPerms :213-225`，含 chmod 提示；Windows 的 `Mode().Perm()` 不反映真实 ACL，平台豁免并注释）。测试 `internal/pki/pki_test.go:134`（cert-only/key-only 两场景：报错含「不完整/人工」、存量文件字节不变、不产生缺失侧新文件）、`:175`（ca.key/server.key 分别放宽 0644 拒绝，恢复 0600 幂等通过） |
| 3 | 阻塞：systemd 状态误报。`internal/agent/collect/services.go:155-197` `checkSystemd` 按退出码分类（裁决口径）：0=active（`:195` 文本核对防编造）、**3=inactive（`:174-184`，"failed" 文本精化为 failed——现代 systemd failed 单元也退出 3，纯按码归 inactive 会丢 failed；""/"inactive"→inactive；其他文本 unknown+原文）**、其他非零按文本映射（`:186-188`）、**非 ExitError（命令不存在等执行器错误）才 unavailable（`:160-165`）**、超时 unknown（`:156-159`）。`:199-214` `systemdTextStatus` 文本映射抽出共用（active/inactive/failed 直映，空/中间态 unknown+原文）。修测试 mock：`internal/agent/collect/services_test.go:71-78` inactive 改注入 `exitErr(3)+inactive` 文本（原 mock 以 err=nil 返回，掩盖「非零退出全归 unavailable」缺陷本体）；`:73-75` failed=`exitErr(3)+"failed"`；`:82-84` unit 不存在=`exitErr(4)+inactive`→inactive；`:59-130` 全表 8 态断言 |
| 4 | 阻塞（采纳+范围裁决）：Docker 边界。`internal/config/config.go:199-202` 新增 `Agent.DockerBin`（yaml `docker_bin`），`:581-583` 加载时 ExpandHome+TrimSpace、空串归缺省 `"docker"`（缺省结构体 `:532` 同）；`internal/agent/collect/services.go:105-115` `NewServiceChecker(decls, dockerBin)` 收路径、`:231` `checkDocker` 以 `c.dockerBin` 执行（原硬编码 `"docker"`）；`cmd/agent/main.go:174` 接线 `cfg.DockerBin`。文档（生产必须以只读 helper/socket-proxy 提供，M1b-b 落地）：`deploy/agent.example.yaml` services 段 + `deploy/console.example.yaml` 生产部署安全注意段 + DELIVERY §一/§四.9/§六。测试：`internal/config/config_test.go:462`（缺省/覆盖/空白归缺省）、`services_test.go:127-149`（断言 exec 的正是配置的 bin 路径且 argv 不变） |
| 5 | 阻塞：探测输出无内存上限。`internal/agent/collect/services.go:37-39` `maxExecOutput=64KB`；`:60-84` `cappedBuffer` 写入阶段封顶（触顶丢弃+置位 truncated+报告全量写入不中断 io.Copy 语义）；`:88-98` `execRunner.Run` stdout/stderr 各挂 cappedBuffer、`cmdResult.truncated` 汇总；`:217-225` `markTruncated` 三型查询全部路径 detail 统一标注 `output truncated at 64KB`。`internal/agentdisc/agentdisc.go:37/186-206` 同口径 64KB cappedBuffer；`runVersion`（`:209-249`）：失败路径 detail 追加截断说明；**成功路径换行在封顶之内→首行版本可信照报；封顶吞掉换行→首行不完整不冒充版本，报 unavailable+truncated 说明**。测试：`services_test.go:255`（cappedBuffer 边界：未触顶/恰满/触顶丢弃+标记）、`:275`（execRunner 真实执行 640KB 输出→截断 64KB+标记、小输出不受影响）、`:302`（三型 detail 标注+inactive 空 detail 也标注）；`agentdisc_test.go:213`（版本行先出+200KB 噪声→版本正常；128KB 无换行→unavailable+truncated 且 Version 为空） |
| 6 | 建议：端口探测响应取消。`internal/agentdisc/agentdisc.go:165-183` `portProbe(ctx, svc)` 签名加入 ctx，`net.DialTimeout`→`net.Dialer{Timeout}.DialContext(ctx,"tcp",addr)`；ctx 取消的探测错误单独分流——**报 unavailable+cancelled 说明，不编造 inactive**（`:171-175`），普通不可达仍 inactive+err 说明。`Scan`（`:92`）传 ctx。测试 `internal/agentdisc/agentdisc_test.go:191`（预取消 ctx→unavailable+detail）；`:169-186` `TestScanRespectsCtx` 断言收紧为不得编造 active/inactive |
| 7 | 建议：发现配置容量对齐心跳 64 项。`internal/config/config.go:177` `maxAgentScanTotal=64`；`validateAgentScan`（`:436`）末尾 `:506-521` 计算 known+custom+services **合并去重**后总数（custom 同名覆盖 PATH 探测不重复计数，`KnownList()` 承载「未配置取默认 5 项」语义）>64 拒绝启动，报错注明与 console 心跳 agents 数组上限（registry `maxAgents=64`）一致。测试 `internal/config/config_test.go:497`（60 known+5 services=65 拒；60 known+1 同名 service 去重后 60 通过） |

### R11 新增七条修复（文件:行号 · 怎么修）

| # | 修复说明 |
|---|---|
| A | 阻塞：null 与缺席混同。`internal/registry/registry.go:347-349` `heartbeatReq.Services/Agents` 改 `json.RawMessage` 承载原始字节；`:356-372` `optionalArray[T]` 泛型三态解释——字段缺席（len==0）→ present=false 无变化；**显式 `null`（TrimSpace 后精确比对）→ `errNullField` 协议违规 400**；显式数组（含空数组）→ 解码 present=true。`handleHeartbeat`（`:649-682`）：null 先于值域校验拒绝（400 不落任何行）；`:584-592` `svcRowsOrNil`/`agentRowsOrNil` 把三态映射为 store 层 nil 指针语义（nil=不替换）。测试 `internal/registry/registry_test.go:653`：services/agents/双字段/带空白 null 四态 400 + 不落 metrics + 缺席对照 200 落库；原「缺席无变化」「`[]` 全转 stale」「值域 400」用例保持通过（`TestHeartbeatServicesAndAgents`/`TestHeartbeatServicesValidation`） |
| B | 建议（文档）：`deploy/agent.example.yaml` services 段注明三态语义——本段整个缺席=无变化（console 保留既有清单），**移除全部受管服务必须显式写 `services: []`**（既有条目全转 stale） |
| C | 建议：指纹错误不回显原值。`internal/config/config.go:349` `NormalizeFingerprint` 非 hex 报错删除 `%q` 回显，只提示格式要求（长度错误路径本就不回显；agent 侧 `client.go` normalizeFingerprint 亦不回显）——错误信息不留任何证书材料片段 |
| D | 建议（与 R10 #2 合并执行）：CA 复用前配对校验。`internal/pki/pki.go:99-101` Ensure 复用分支先 `requireCAPair`（`:212-221`：ca.crt 的公钥必须就是 ca.key 的公钥，错配即报「不配对」拒绝——否则重签产物无法被 ca.crt 验证却报成功）再 `requireKeyPerms`（0600，R10 #2 已做）；server 侧 cert/key 错配由既有 `needsRenewal` 公钥比对触发重签自然修复（私钥不动）。测试 `internal/pki/pki_test.go:305`（CA A 的证书 + CA B 的私钥 → 报错含「不配对」） |
| E | 建议（文档）：`deploy/console.example.yaml` pki 段与 `Makefile` pki 目标注明**勿并行运行 make pki**（无跨进程锁，并发执行可能产生配对不一致产物；工具会校验配对并拒绝，仍应避免） |
| F | 建议：心跳三事务半轮数据。`internal/store/store.go:365-398` 心跳核心写入抽 `heartbeatTx`；`:564-607` 服务/agent 全量替换抽 `replaceServicesTx`/`replaceAgentsTx`（原 `ReplaceNodeServices`/`ReplaceNodeAgents` 公开方法保留、单表事务语义不变）；**新增 `:400-425` `HeartbeatFull`——单事务完成 metrics+节点时间戳+services 替换+agents 替换，任一步失败 defer Rollback 整体回滚**；`internal/registry/registry.go:692-694` handleHeartbeat 改调 HeartbeatFull 一次（原 Heartbeat→Replace×2 三次独立 store 调用删除）。测试 `internal/store/store_test.go:434`：一次调用三表齐写；nil 缺席不动既有清单、metrics 照写；**中途失败注入（4 万行服务批次超 SQLite 变量数上限 32766 令 markStale 的 NOT IN 报错，此刻 metrics 已在事务内写入）→ 断言 metrics 与既有服务行全部原样保留（无半轮数据）** |
| G | 建议（轻量版）：服务查询整轮总预算。`internal/agent/collect/services.go:34-37` `scanBudget=10s`（32 条×5s 病态最坏会阻塞心跳超过 offline_after 60s）；`CheckAll`（`:120-145`）整轮 `context.WithTimeout` 包裹，预算耗尽后剩余条目不再执行、如实报 unknown+`budget exhausted` 说明（不编造状态），单条 5s 超时语义不变；`budget` 字段测试可注入（`:107`）。测试 `services_test.go:337`（1ns 注入预算→全部条目 unknown+budget 说明；默认预算对照全部 active） |

**验证记录**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（**90 用例**：store 13、registry 17、agent 12、collect 15、agentdisc 8、config 16、pki 7、cmd/console 2）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。期间实测记录：① Go x509「叶子在信任池即直通」行为以独立探针程序复现并据此加固 `verifyServedChain`（R10 #1 ②）；② **真机冒烟抓到并修复一处缺陷**——CA 不完整报错文案的「缺失/存在」文件名写反（`loadCA` 的 swap 条件用了 certMissing，应为 keyMissing；单测只断言了关键词没断言文件名，已补强两场景断言），修复后冒烟确认：`meshconsole pki` 在 ca.crt 被删后报「CA 不完整：ca.crt 缺失但 ca.key 存在」且不覆盖 ca.key；③ 真机全链路冒烟：console HTTPS（curl --cacert → ok）、`meshagent register -fingerprint`（不带 ca_cert，走下发链+指纹）注册成功、篡改指纹 → `server certificate fingerprint mismatch`、`meshagent register -console http://…` → 启动即拒（明文路径已移除）。deploy 两样例、Makefile、DELIVERY.md（§一/§二.7/§三/§四/§六）同步更新。越界项（MCP/Headscale/Web）未触碰。

## R12 · codex 复审 M1b-a（2026-10-08 01:0x，值班员执行）——**作废**

审查启动时工作区仍在被首轮 zcode（m1b-a-fix，00:21 派工未中断）修改，审查对象为移动中的树，结论不具效力，意见不裁决。存档：.pipeline/m1b-a-review-codex2.log（1 阻塞 + 5 建议，均为 R11 已采纳项的未完成态，与 zcode 正在执行的修复范围一致，间接确认修复方向无误）。正式重审待真实 done 后执行（R13，codex3.log）。

事故根因记录：同一任务名重复派工（00:21 + 00:31），第二轮被取消后 EXIT trap 写出假 done，值班员据假 done 推进验证与复审。防复发：处理 done 前先确认无 zcode-cli/pipeline-run.sh 进程存活（见 STATE.md 备忘）。

## R13 · codex 复审 M1b-a（2026-10-08 01:30，Hana 主会话执行）

**结论：修改后通过。** R10+R11 累计意见大部分关闭；HeartbeatFull 原子性深挖通过；optionalArray 三态正确。4 条残留全部裁决采纳修复：

| # | 意见 | 级别 | 修复口径 |
|---|------|------|----------|
| 1 | self-issued 误当 self-signed（只比 RawIssuer==RawSubject 未验自身签名，同名单异密钥叶子可绕过） | 阻塞 | 锚入 Roots 前显式验证其自身签名（用锚公钥 verify 签名），补反例单测 |
| 2 | CA 复用未查有效期（过期/未生效 CA 可重签出废证书） | 阻塞 | 复用分支加 NotBefore/NotAfter 校验，过期拒绝并提示重新 make pki |
| 3 | 指纹错误 %q 回显非法字符（客户端 -fingerprint 路径） | 缺陷 | 去回显，仅提示格式要求 |
| 4 | 截断版本误判有效（stderr 截断不查、空行+截断漏判） | 缺陷 | 针对实际选中的版本行判断完整性（截断标记的行不作版本） |

## R14 · zcode 修复（2026-10-08 完成）

**R13 四条残留全部按裁决修完。** 修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **94 用例全绿**（修复前基线 90 + 本轮新增 4：agent 2、pki 1、agentdisc 1）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。未执行任何 git commit。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | 阻塞：self-issued 误当 self-signed。`internal/agent/client.go:140-145` `verifyServedChain`：`RawIssuer==RawSubject` 名单判定之后、锚入信任池之前，**新增 `anchor.CheckSignature(anchor.SignatureAlgorithm, anchor.RawTBSCertificate, anchor.Signature)`——用锚自身公钥验证其 TBS 自签名**，签名不符报 `anchor that is not genuinely self-signed` 拒绝。堵的口子：同名单异密钥的伪造锚（名单复制真 CA、TBS 由攻击者私钥签署）此前可凭名字入池，叠加 Go「叶子即在信任池」的直通捷径令签名链验证整体放行；显式 CheckSignature 用的是纯签名验证（不走 `CheckSignatureFrom`，避免其对锚 IsCA 的附加约束误伤自签叶子单证书场景）。反例单测 `client_test.go` `TestVerifyServedChainRejectsImpostorAnchor`：伪造锚（Issuer==Subject 同名单、证书公钥=impostorKey、签名=signingKey 异密钥）+ 同名单异密钥叶子，两形态（单证书呈现 / 双证书链）均断言拒绝且错误含 `self-signed`；真自签路径（httptest 自签锚、CA 签发完整链）既有用例全部保持通过 |
| 2 | 阻塞：CA 复用未查有效期。`internal/pki/pki.go:104-106` Ensure 复用分支（配对校验后、权限校验前）新增 `requireCAValidity`（`:226-239`）——用当前时间比对 NotBefore/NotAfter，**已过期报「CA 证书已过期（NotAfter …）…请人工整体移除 ca.crt/ca.key 后重新 make pki」、尚未生效同型报错，均拒绝复用**（过期 CA 重签出的服务端证书在客户端验证必然失败却会报成功）；注明作废后果（各 agent 的 ca_cert/指纹需同步更新）。单测 `pki_test.go` `TestEnsureRejectsExpiredCA`：过期（NotAfter=-1h）/未生效（NotBefore=+1h）两子场景断言报错含关键词与 `make pki` 提示、既有 CA 文件字节不变、不悄悄签发服务端证书；对照断言有效期内 CA 幂等复用不受影响（`TestEnsureIdempotent` 继续钉住零改动） |
| 3 | 缺陷：指纹错误 %q 回显。`internal/agent/client.go:177-188` `normalizeFingerprint`：非 hex 分支删除 `fmt.Errorf("fingerprint contains non-hex char %q", …)` 回显，改与 config 侧 `NormalizeFingerprint`（R11-C 口径）逐字一致的无回显文案 `fingerprint 含非 hex 字符（须为 64 位 hex，`meshconsole pki` 输出）`；长度错误分支同步对齐中文文案（本就不回显）。单测 `client_test.go` `TestNormalizeFingerprintNoEcho`：非 hex（尾部 `zz` 标记）与长度错误（独特标记串）两路断言错误信息不含标记/原值、且含 hex 格式提示；冒号/大写归一化正路径回归 |
| 4 | 缺陷：截断版本误判。`internal/agentdisc/agentdisc.go:233-260` `runVersion` 成功路径重写——版本值取自哪个缓冲（stdout 优先、空则 stderr 兜底）就按**该缓冲**的截断状态判断：新增 `firstLineComplete`（`:262-274`，行选取规则与 `firstLine` 完全一致）判定选中行在已捕获字节内是否有换行终止；**选中缓冲被封顶且选中行无换行终止 → 报 unavailable+`output truncated at 64KB before first line break`，半行不冒充版本**——修复前两处漏判：① stdout 空行的换行使 `Contains(captured,"\n")` 恒真、空行后被截断的半行照报；② 版本值兜底取自 stderr 时截断检查仍只看 stdout。另修复「退出 0 但全空输出」：任一缓冲被截断时「无输出」结论不可信，报 unavailable+truncated（不再静默 active+空版本）；选中行有换行终止（换行在封顶之内）仍可信照报（R10-#5 语义保持）。单测 `agentdisc_test.go` `TestVersionTruncatedSelectedLine` 四脚本：stdout 空行+半行截断、stdout 全空行+截断（两形态均 unavailable+truncated+版本空）、stderr 半行截断（unavailable）、对照 stderr 完整版本行+截断噪声（active 照报）；R10 既有 `TestVersionOutputCapped`（版本行先出+200KB 噪声照报 / 128KB 无换行拒绝）保持通过 |

## R15 · codex 复审 M1b-a（2026-10-08 01:45-02:10，值班员执行）

**结论：修改后通过。** m1b-a-fix2 真实 done 后的全量未提交改动复审（codex4.log，read-only 沙盒）。值班员先行独立验证：`go vet` 零输出、`gofmt -l` 无文件、`go test -count=1` **94 用例全绿**、`make cross` 三平台通过（已覆盖 #6 所述静态审查边界）。6 条意见逐条到源码核实：5 条属实裁决如下，#6 为审查方法自述无需动作。

| # | 意见 | 级别 | 核实 | 裁决 |
|---|------|------|------|------|
| 1 | `internal/config/config.go:78` UnmarshalYAML 拒绝纯字符串 token 时 `%q` 回显原值，秘密凭据进启动错误日志 | 阻塞 | 属实（`fmt.Errorf("…: %q", s)`） | 采纳：删回显，仅留格式要求与示例（R11-C/R13-#3 同口径：凭据材料不进日志/错误信息）；补无回显单测 |
| 2 | `internal/pki/pki.go:360` 附近 `needsRenewal` 只查 NotAfter，未生效（NotBefore 在未来）或无 ServerAuth EKU 的服务端证书被复用且报成功 | 阻塞 | 属实（现有检查：解析/NotAfter/CheckSignatureFrom/SAN） | 采纳：与 R13-#2 同类「报成功但客户端验证必然失败」；补 NotBefore 与 EKU（含 ExtKeyUsageAny 视为可用）检查触发既有重签路径；补两场景单测 |
| 3 | `internal/pki/pki.go:189` 附近 `ServerTLSConfig` 加载私钥未查 0600，被放宽后仍可启动 | 建议 | 属实（Ensure 生成侧查，启动加载入口不查） | 采纳：加载阶段复用 `requireKeyPerms`（非 Windows，与 R10-#2 口径一致）；补放宽拒绝单测 |
| 4 | 退出测试仅覆盖裸 `LimitListener.Close`（R5 回归），未覆盖 TLS 监听链 + drainHTTP Shutdown | 建议 | 属实（`cmd/console/main.go` drainHTTP 无端到端测试） | 采纳（轻量版，R11-G 先例）：补一例真实链路 Shutdown 回归（TLS 证书 + LimitListener 包裹 + 在途连接），不要求穷举全部故障组合 |
| 5 | `DELIVERY.md` §四.4 仍记「最坏 160s 拖慢心跳」，与 R11-G 已实现的 10s 整轮预算冲突 | 可选（文档） | 属实（`services.go:34-37` scanBudget=10s） | 采纳：文档同步为 10s 预算语义 |
| 6 | 静态审查边界自述（未重跑测试/未核定改动集） | 可选 | — | 无需动作：值班员已独立重跑全部验证 |

**修复轮范围**：本表 #1/#2/#3/#4（代码）+ #5（文档）→ 派 m1b-a-fix3。验证口径不变：`go vet` / `gofmt` / `go test -count=1` 全绿 + `make cross` 三平台。返工计数：5 条均为首次出现，未触发「同一条修两次不过」上限。

## R16 · zcode 修复（2026-10-08 完成）

**R15 裁决 5 条全部按口径修完。** 修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **97 用例全绿**（修复前基线 94 + 本轮新增 3：pki 2、cmd/console 1）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。未执行任何 git commit。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | 阻塞：纯字符串 token 报错回显原值。`internal/config/config.go:76-81` `RegistrationToken.UnmarshalYAML` 纯字符串分支删除 `: %q` 原值回显（凭据材料不进错误信息/日志，R11-C/R13-#3 同口径），报错只保留「不再接受纯字符串写法（无到期、不绑定节点，等于永久凭据）」的说明与映射写法示例（`tokenExampleYAML`，`config.go:44-47`）。单测 `config_test.go:71` `TestConsolePlainStringTokenRejected` 加固：既有四关键词指引断言（纯字符串/映射/expires_at/expected_node）之外，新增断言错误信息**不含原 token 值**（`config_test.go:88`，独特标记串全文不出现） |
| 2 | 阻塞：needsRenewal 未查 NotBefore/EKU。`internal/pki/pki.go:367` `needsRenewal` 在既有检查（解析/NotAfter `:383-386`/CheckSignatureFrom `:391-393`/SAN/公钥比对）之上新增两检查，均触发既有重签路径（复用既有 CA+私钥，CA 与两把私钥不动）：① 当前时间早于 NotBefore → 重签（`:387-390`，与 R13-#2 同类「客户端验证必然失败却报成功」）；② ExtKeyUsage 既不含 ServerAuth 也不含 ExtKeyUsageAny → 重签（`:394-405` `hasServerEKU` 扫描，Any 视为可用于服务端）。函数注释同步（`:362-366`）。单测 `pki_test.go:427` `TestEnsureRenewsUnusableServerCert`（助手 `rewriteServerCert` `:389`：用目录内既有 CA+私钥重签按 mutate 改写的 server.crt，SAN 与 `issueServerCert` 同源）——「尚未生效」（NotBefore=+1h）与「缺 ServerAuth EKU」（EKU=nil）两场景均断言 `ServerCertRenewed=true`、CA/两把私钥/ca.crt 字节不变、重签产物当期有效且带 ServerAuth（`:427-474`）；对照断言 EKU 仅含 ExtKeyUsageAny 时幂等保持不重签（`:476-480`）；既有 SAN 变更重签/幂等零改动/到期 CA 拒绝用例保持通过 |
| 3 | 建议：ServerTLSConfig 加载入口不查私钥权限。`internal/pki/pki.go:192-196`：`ServerTLSConfig` 在 `tls.LoadX509KeyPair` 成功后对 keyPath 复用 `requireKeyPerms` 校验 0600（Windows 豁免口径与 R10-#2 一致：`Mode().Perm()` 不反映真实 ACL，`pki.go:246-258`），权限放宽时启动报错并给 chmod 提示（既有文案「权限为 %o（chmod 600 …后重试）」）。单测 `pki_test.go:483` `TestServerTLSConfigRequiresKeyPerms`（非 Windows）：放宽 0644 拒绝且报错含 0600 与 chmod 提示、恢复 0600 正常加载 |
| 4 | 建议：退出链无端到端测试。`cmd/console/main.go:98-101` `drainHTTP` 抽出时长可注入的同款序列 `drainHTTPSeq`（`main.go:103-127`，三段式 Shutdown→超时强断→inFlight.Wait 归零逐行不变，仅宽限/等待上限成参；生产入口仍传 `shutdownGrace`/`handlerWaitCap` 真实常量，main 调用点零改动——注入口径对齐 R11-G budget 先例）。新增 `cmd/console/main_test.go:172` `TestDrainHTTPOverTLSLimitListener`：`newTestTLSListener`（`main_test.go:135`）以 `pki.Ensure` 临时生成真实证书 + `pki.ServerTLSConfig` 组装与 `listenTLS` 同构的 `TLS(LimitListener(TCP))` 监听链，`tlsClient`（`:159`）按自签叶子构造信任客户端，两子场景各持一条在途 TLS 连接——① graceful drain：在途 handler 在宽限期内放行，跑生产版 `drainHTTP`（真实 35s 常量），断言函数返回且在途请求被排空（客户端收到完整 200 响应）；② forced close：handler 持续阻塞，`drainHTTPSeq` 注入 300ms 宽限 → Shutdown 超时 `srv.Close()` 强断 → handler 随连接断开返回 → WaitGroup 归零函数返回，断言客户端连接被切断（无干净响应）。不穷举满配额/未完成握手等组合（R15-#4 轻量口径）；连跑 3 遍无 flake |
| 5 | 可选（文档）：DELIVERY §四.4 与 R11-G 已实现语义冲突。`DELIVERY.md:109` §四.4 重写为 R11-G 实现语义——整轮查询有 **10s 总预算**（scanBudget）兜底：预算耗尽后剩余条目不再执行、如实报 unknown+`budget exhausted` 说明（不编造状态），单条 5s 超时语义不变；病态环境整轮最坏 ~10s（预算到期即在途条目一并截断），远低于 offline_after 60s（原「最坏 160s 会拖慢该轮心跳」口径删除）。关联处同步：§一 pki 行（`DELIVERY.md:11`：重签触发补「尚未生效/缺 ServerAuth EKU」，ServerTLSConfig 补启动 0600 校验）、§一 cmd/console 行（`:13`：退出链回归单测）、§二.7 用例数 94→97 及分包数（`:88`）、§六处置记录补 R15 轮引述与三条行为变化 bullet（`:140-153`） |

## R17 · codex 复审 M1b-a（2026-10-08 02:30，值班员执行）——**通过，M1b-a 收口**

审查日志 `.pipeline/m1b-a-review-codex5.log`（静态只读；codex 自述未跑测试/构建，构建与 97 用例由值班员独立验证全绿后才采信结论）。结论「**通过**」（无 [阻塞] 项）。

值班员独立验证（02:35-02:40）：`go vet` 零输出、`gofmt -l` 无文件、`go test -count=1` **97 用例全绿**、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；抽验 R15 #1 token 无回显断言（`TestConsolePlainStringTokenRejected`）与 #2 两场景重签单测（`TestEnsureRenewsUnusableServerCert/not-yet-valid + missing-server-auth-eku`）及对应源码落点均通过。

3 条非阻塞意见裁决：**全部记录、不派修复轮**（结论为通过，建议/可选级属加固项而非验收缺陷，与 R15-#6「可选无动作」口径一致；三条均转入 M1b-b 任务书候选清单）：

| # | 级别 | 意见 | 裁决 |
|---|---|---|---|
| 1 | 建议 | `internal/pki/pki.go:443` 非法非空 tailnet_ip 被静默忽略，证书生成仍报成功但缺配置要求的 SAN | 采纳转后续：证书生成前校验 IP 合法性，非法即报错（M1b-b 候选） |
| 2 | 建议 | `internal/config/config.go:417` systemd/docker target 无长度上限，服务端仅保留 256 字节，可能截断甚至心跳超限 | 采纳转后续：启动校验统一 256 字节上限（M1b-b 候选） |
| 3 | 可选 | `internal/agent/runner.go:135` 注释仍称 null 被视为字段缺席，与显式 null 返回 400 的实现相反 | 采纳转后续：注释同步三态语义（M1b-b 候选，纯注释改动） |

处置：R17 通过 → `.pipeline/` 过程日志本轮起加入 .gitignore 不入库（结论已沉淀于本文件与 DELIVERY.md，审查原始日志留本地）；`git add -A && git commit` 收口 M1b-a。全轮次轨迹：R10 审查（1 阻塞+5 建议）→ R11 复审 → R12 作废（工作区移动）→ R13 复审（4 条残留）→ R14 修复 → R15 复审（2 阻塞+2 建议+2 可选，5 条采纳）→ R16 修复 → R17 终审通过。


---

# 自测报告 · zcode M1b-b（2026-10-08）

> 交付内容：SPEC-M1b-b.md 三交付物（MCP Server / Headscale 集成 / Web 只读面板）+ 三个观察点修复 + R17 三条转来候选闭环。全部只读，无任何写操作路径；未执行 git commit。
> 验证基线：`go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **124 用例全绿**（M1b-b 收口基线 97 + 新增 27：store 3、headscale 5、meshview 3、panel 5、mcpserver 5、collect 4、pki 1、config 1）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。依赖新增唯一一项：`github.com/modelcontextprotocol/go-sdk v1.8.0`（官方 SDK，GOPROXY=goproxy.cn 可拉，锁定 go.mod/go.sum；间接依赖 jsonschema-go/segmentio encoding 等随 SDK 带入）。

## SPEC §7 验收清单逐条

| # | 验收项 | 结果 | 证据 |
|---|--------|------|------|
| 1 | vet/gofmt/test 全绿 + 新模块单测 | ✅ | 124 用例全绿；SPEC 点名四类覆盖：**MCP 三方法握手**（`mcpserver_test.go`：内存传输上 client.Connect=initialize + `TestHandshakeToolsList` tools/list 五工具齐备不许多不少 + tools/call ×4）、**tailnet 全量替换事务**（`store_m1bb_test.go` `TestReplaceTailnetNodesFullReplace`：两次写入后表内容恰为第二集合、空集合清空；`headscale_test.go` `TestSyncFullReplace` 同语义）、**Origin/Host 校验**（`panel_test.go` `TestGuardHostAllowlist`：localhost/127.0.0.1/[::1] 带或不带端口与大小写放行；evil.com / `attacker.com:127.0.0.1` rebinding 变体 / 后缀伪装 / 空 Host 一律 403；`TestGuardOrigin`：跨源与 null 拒绝、四形态同源放行）、**panel overview 聚合**（`TestOverviewEndpoint` + `meshview_test.go` `TestOverviewAggregation`：nodes/services/agents/tailnet/freshness 全字段） |
| 2 | `make cross` 三平台 | ✅ | linux/amd64、darwin/arm64、windows/amd64 全部 built（CGO_ENABLED=0 纯静态） |
| 3 | MCP stdio 握手 + list_nodes 真数据 | ✅ | 本机 E2E（/tmp/m1bb-e2e，含心跳数据）：stdin 逐帧喂 `initialize` → `serverInfo {meshconsole, 19f3a33-dirty}`、协议版本 2025-06-18；`tools/list` 五工具；`tools/call list_nodes` 返回 e2e-node 真行（status/agent_version/last_seen）；`get_node` 24h samples=17 + services 2 + agents 2；`get_mesh_status` 汇总+异常名单+tailnet.tracked=2 |
| 4 | Headscale 拉取 + 断 key 优雅降级 | ✅（mock E2E）/ 真机待上 | 本地 mock E2E：配置 headscale 段启动即拉取 2 节点入库（id 字符串/数字两形态、未知字段忽略）；**mode=401 → WARN 一次（`consecutive_failures:1`）+ 旧数据原样保留**；恢复 → INFO `sync recovered after_failures=1` + 数据续替。未配置段 → INFO disabled 一次不报错；配段缺 api_key → 拒绝启动。**真 headscale（云机现成实例）连测列部署上机项**（本机无 headscale 实例） |
| 5 | 面板 4.4 验收 + liquid glass 五要素 | ✅（五要素自证见下） | `curl -s https://127.0.0.1:7790/ \| grep -c glass` = **6**；`/api/panel/overview` 聚合 JSON 200；`-H 'Host: evil.com'` → **403**（attacker.com:127.0.0.1 同样 403）；安全头 nosniff+no-store 全路径；JS 渲染冒烟（Node + 最小 DOM 桩 + 真实 overview 数据）：节点卡/仪表/清单行/sparkline/页脚全构建且 15s 轮询钉住。**浏览器人眼过布局/动效留部署上机项**（本机 Edge headless 受窗口服务器限制无法截图，渲染管线已由 DOM 冒烟实证；还借此抓出并修复 lead 卡 head 未 append 的真实渲染缺陷） |
| 6 | 观察点三条闭环 | ✅ | 见下节 |
| 7 | REVIEW/DELIVERY 更新、禁止 commit | ✅ | 即本节与 DELIVERY.md 顶部新节；未执行任何 git commit |

## 观察点闭环

| # | 现象 | 核查结论 | 处置 |
|---|------|----------|------|
| ① | `--version` 缺失 | 确认缺失（M1a/M1b-a 均无） | `cmd/console/main.go:90` `cmdVersion` + `cmd/agent/main.go` 同款：`meshconsole --version` / `meshagent --version` 输出 `<bin> <version> (commit <commit>)`；`-h/--help/help` 统一 usage 出口（exit 2），`pki -h`、`mcp -h` 由 flag.Usage 输出统一风格用法+flag 默认值 |
| ② | 部署显示 `31eafac-dirty` 旧号 | Makefile 注入链本身存在，部署用的是 M1a 期旧产物；且缺 commit 注入无从核对 | `Makefile` LDFLAGS 补 `-X main.commit=$(COMMIT)`（`git rev-parse --short=8`），版本+commit 双注入两二进制；`bin/meshconsole` 重建依赖补 `internal/panel/web/*`（改面板不重跑 build 会打出旧静态资源）；部署核验口径写入 DELIVERY（拉代码后必须重 make build/cross） |
| ③ | process 型对 agent 自身判 inactive | **核查结论：不存在 exclude-self 设计**——`checkProcess` 是裸 `pgrep -f <target>`，pgrep 只排除 pgrep 自身、不排除其祖先（agent 进程）；本机实测（macOS）`bash -c 'pgrep -f <pat>'` 能命中含该 pat 的父进程。代码语义下「agent 自身含 target 即被计入」，部署观察到的 inactive 最可能是该节点 target 与实际进程 cmdline 不匹配（配置层），或平台边缘（僵尸态空 cmdline 等）致 pgrep 漏检 | 按 SPEC「等效方案」加固 `services.go`：pgrep **exit 1（无匹配）时先做自进程确定性核对再判 inactive**——`selfMatches`（`services.go:129`）以本进程命令行（os.Args，`/proc/self/cmdline` 的跨平台等效物）按 pgrep 同口径 ERE 匹配（非法正则退化子串），命中报 `active` + detail `count=1 (self process matched; pgrep reported no other)`（来源如实标注，不编造其余进程）；不命中维持 inactive（不放过一切）。单测 4 例：注入 selfArgs 命中/不命中、正则/非法正则、真机自探活（跳过 Windows/pgrep 缺失）+ 真机反例（幽灵目标不改判）。E2E 实测 process 型自探 target=meshagent → `active count=1` |

## Liquid Glass 五要素自证（SPEC §4.3，技能 `docs/skills/liquid-glass-frontend`）

1. **真实 frost**：`glass.css` `.glass-surface` = `color-mix(in srgb, var(--app-panel) 42%, transparent)` + `backdrop-filter: blur(34px) saturate(190%)` + specular 顶边（`inset 0 1px 0 rgb(255 255 255/.14)`）；`.glass-surface-soft`（仪表条）blur(44px)+saturate(200%)+投影。玻璃面板内部一律 `.glass-inset`（半透明提亮+顶缘反光，**无 backdrop-filter**——嵌套破坏 GPU 合成是技能第一坑）；JS 行构建用 `glass-inset glass-inset-hover`（hover 是提亮浮起而非换色）。
2. **atmosphere**：body 后景三枚漂移 blob（blur(76px)+mix-blend screen，26s/31s/37s 错频永不同步）+ 出血细线弧 + 淡方格图纸网格 + 颗粒叠层（feTurbulence SVG data-URI）；主节点卡右上角自带一枚 aura 光斑——玻璃之下始终有可折射的颜色。
3. **色彩单一源**：三个 RGB 三元组 `--primary`(mint 52 211 153)/`--accent`(amber 245 158 11)/`--alarm`(red 248 113 113)——控制室方向「状态色即品牌色」；派生 tint 走 `color-mix`/`rgb(var() / α)`，组件零硬编码 hex（语义层 `[data-app]` 中性冷墨色板；浅色主题按技能口径留 TODO 注释，M1b-b 仅深色）。与技能样例三元组结构（primary/primary-soft/accent）的差异：本面板需要红黄绿三态信号色，soft tint 改由 color-mix 派生，三元组纪律不变。
4. **排版**：数据全部等宽（`--font-mono`，系统等宽栈，**离线取舍**：不引 webfont，本机装有 JetBrains Mono/IBM Plex Mono 自动命中否则落 SF Mono/Menlo/Consolas）；标题=超大 mono（clamp 2.6–4.6rem，-0.04em 紧排）+ 一处衬线斜体 accent（`Network <em>control</em>`，ui-serif）；kicker 宽字距大写 mono；大数字 tabular-nums；状态一律「色点+文字」双通道不依赖颜色单通道。
5. **动效**：全站唯一 easing 签名 `--ease: cubic-bezier(0.22,1,0.36,1)`。载入一次性编排：标题 clip-path 揭示 → 仪表条/面板按 0.22s→0.85s stagger 浮起（非全元素 uniform fade-in-up）；数据刷新数字 tween（JS ease-out，与 CSS 同族）、freshness 闪色、offline/failed 呼吸红点（breathe 2.4s）；`prefers-reduced-motion` 全关。

**Honesty check**（技能 creative-direction.md）：非对称 7/5 与 5/7 交替 bento（主节点大卡+次节点小卡+tailnet 面板为右列，次行镜像）；仪表条四联大数字非卡片；单一签名交互=「控制室仪表读数 + 离线呼吸点」；无图标卡行、无居中 hero、无 SaaS 模板脸。

## 结构与落点（新增文件）

- `internal/mcpserver/mcpserver.go`（140 行）：SDK 组装，五工具输出顶层一律对象（structuredContent 稳妥形态），get_node 未知节点走 IsError 工具错误并提示 list_nodes
- `internal/headscale/headscale.go`（233 行）：Client（10s 超时、Bearer、≤4MB body、非 200 报错含状态码+前 200B 说明）+ Fetcher（Sync 单元可注入 / Run 循环失败计数+1h WARN 节流+恢复 INFO）
- `internal/meshview/meshview.go`（507 行）：MCP 与面板同源只读服务层；异常口径 `serviceIssueGrace=300s`（`status != active` 且数据未过宽限；`stale` 为簿记态不计）——SPEC「未过宽限」按「数据仍在宽限窗口内才计现行异常」实现（陈旧行=离线节点遗留数据，不当现行故障），单测双向钉住
- `internal/panel/panel.go`（142 行）+ `internal/panel/web/{index.html,glass.css,app.js}`（108/462/528 行）
- `internal/store/store.go` 扩展：migration v3（tailnet_nodes）、`OpenReadOnly`（mode=ro+query_only 双保险、9 个写路径方法加 ErrReadOnly 守卫、Close 兼容只读句柄）、tailnet 全量替换、ListNodes/ListAllServices/ListAllAgents/MetricsStatsSince/MetricsSeriesSince/LatestMetricsTime
- `internal/config/config.go`：`headscale:` 段（url 缺省 127.0.0.1:8080、api_key 必填、interval_s 缺省 300 下限 30）
- `cmd/console/main.go`：`mcp` 子命令（LoadConsoleForPKI 读 db_path 不强制 token；OpenReadOnly；日志全走 stderr 防 JSON-RPC 污染）+ headscale goroutine 接线 + 面板路由挂载

## 已知限制与上机项

1. **真 headscale 连测**（云机现成实例）与**浏览器人眼过面板**（布局/动效/玻璃质感）列部署上机项；本机以 mock E2E + DOM 冒烟兜底。
2. MCP stdio 传输对「stdin 全部缓冲后立即 EOF」会随 EOF 快速退出（管道喂帧需像真实 MCP 客户端一样保持会话或分帧写入）；真实客户端（Hana/zcode）均为常驻会话，不受影响。
3. 面板 poll 间隔固定 15s、sparkline 取最近 48 点、异常宽限 300s 为固定常量（SPEC 口径），未入配置。
4. tailnet_nodes.online 直接采信 headscale 报告值；旧版 API 无 online 字段时按 offline 存（不推导，不编造），last_seen 仍可见。

## R19 · codex 审查 M1b-b（2026-10-08 11:10，Hana 主会话执行）

**结论：修改后通过。** 正向：MCP 只读边界（stdio/ro 开库/固定 SELECT/无注入入口）、embed 固定映射无穿越、agent API 路由隔离无回退、Headscale 未知字段容忍/超时/401 保旧数据。

### 裁决（Hana，全部采纳）

| # | 级别 | 意见 | 修复口径 |
|---|------|------|----------|
| 1 | 阻塞 | headscale.go:110 非法成功响应（{}/null/缺 nodes）被当空列表清库 | 区分合法 nodes:[] 与非法响应，非法保留旧数据+记错误 |
| 2 | 阻塞 | meshview.go:38 节点视图缺 id | 补 id 字段（SPEC §2） |
| 3 | 阻塞 | meshview.go:164 overview 缺 metrics 摘要 | 复用已有 24h 摘要查询（均值/峰值/磁盘占比） |
| 4 | 阻塞 | app.js:156 面板自算异常数未用 300s 宽限 | service 层统一输出异常名单口径，前端不做业务判断 |
| 5 | 阻塞 | Makefile VERSION 非语义版本 | VERSION 语义化变量（m1b-b）+ git commit 后缀，-X 注入 |
| 6 | 阻塞 | DELIVERY.md "§7 全部通过"夸大（真 headscale/人眼验收未做） | 改口径：明确"本机验证通过 + 部署上机项清单" |
| 7-11 | 建议 | Host 端口/畸形变体严格解析；store NULLIF 分指标处理；headscale 节流归零策略；config URL scheme 校验；SVG stop-color 无效值/easing 统一/状态点过渡 | 全部采纳 |
| 12 | 可选 | 全量替换回滚断言缺失 | 采纳：补中途失败保留旧数据断言 |

## R20 · zcode 修复（2026-10-08 完成）

**R19 审查意见 12 条全部裁决采纳、全部按口径修完。** 修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **130 用例全绿**（修复前基线 124 + 本轮新增 6：headscale 3、store 2、config 1；meshview/panel 为既有用例扩展断言不计新函数）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。真机冒烟复核：`meshconsole --version` → `meshconsole m1b-b+git_19f3a339 (commit 19f3a339)`（meshagent 同）；`/api/panel/overview` 实测输出节点 `id`、每节点 24h `metrics` 摘要与 `service_issues` 名单（inactive 行在列、active 不在列）；Host 严格校验变体 `localhost:evil`/`localhost:`/`[localhost]`/`::1`/`127.0.0.1:99999` 全 403（`evil@localhost` 被 net/http 层 400 拒绝，双层防御）、合法四形态 200；headscale 不可达场景 WARN 恰一条 + 旧数据保留。未执行任何 git commit。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | 阻塞：HTTP 200 非法 body 被当空列表清库。`internal/headscale/headscale.go`：`wireResp.Nodes` 改 `*[]wireNode` 指针承载「键存在性」——顶层 `null`/`{}`/缺 `nodes` 键/`nodes:null` 在 Decode 后均为 nil，`FetchNodes` 显式判 nil 报 `headscale response missing "nodes" array (malformed body)`；`dec.More()` 补充拒绝尾部多余数据（`{"nodes":[]} junk` 同样非法）。非法响应与解析失败同路径返回错误 → Sync/Run 走既有「保留旧数据 + WARN 计数」降级链，错误日志由失败计数机制天然携带；只有显式 `nodes` 数组（**含空数组**）才全量替换（空列表清库是合法语义，非法响应清库是数据丢失）。测试 `headscale_test.go` `TestFetchNodesIllegalBody`（六形态非法 + 空数组合法对照）、`TestSyncIllegalResponseKeepsOld`（走真 client HTTP 层：非法 200 → Sync 报错且旧行原样） |
| 2 | 阻塞：节点视图缺 id。`internal/meshview/meshview.go` `Node` 首字段补 `ID int64 \`json:"id"\``（SPEC §2 list_nodes 契约字段），`nodeView` 从 `NodeRecord.ID` 填充——MCP list_nodes/get_node 与面板 overview 同源受益。测试 `meshview_test.go` `TestOverviewAggregation` 补断言：全部节点卡 `Node.ID != 0` 且 cloud-1 id=1、JSON 契约含 `"id"` 键 |
| 3 | 阻塞：overview 缺每节点 metrics 摘要。`meshview.go` `NodeCard` 补 `Metrics *MetricsSummary \`json:"metrics,omitempty"\``；`Overview` 循环内复用 store 既有 `MetricsStatsSince`（24h 窗口，**未新写 SQL**），并抽出 `statsView` 助手与 `NodeDetail`（get_node）共用同一转换。测试同用例补断言：`lead.Metrics.Samples ≥ 1` 且 CPUAvg=33.5 与 get_node 口径一致、JSON 含 `"metrics"` |
| 4 | 阻塞：前端自算异常数未用 300s 宽限。`meshview.go` `Overview` 补 `ServiceIssues []ServiceIssue \`json:"service_issues"\``（初始化空切片保证 JSON 为 `[]` 非 null）；异常判定抽唯一谓词 `serviceIsIssue`（status != active、非 stale、`updated_at ≥ now-300s`），`Status`（get_mesh_status）与 `Overview` 共用，`sortIssues` 同步抽出——口径只在 service 层。`app.js` `renderReadout` 删除本地 `status!=='active'&&!=='stale'` 过滤（该计算无宽限口径，陈旧行会被误计），改 `const issues = (ov.service_issues \|\| []).length` 纯消费。测试：`meshview_test.go` overview 断言两行新鲜 inactive 在名单 + 拨旧超宽限后 overview 与 status 同步收敛到 1 条；`panel_test.go` `TestOverviewEndpoint` 既有断言不回退 |
| 5 | 阻塞：VERSION 非语义版本。`Makefile`：`VERSION` 由 `git describe --tags --always --dirty`（产出 `19f3a33-dirty` 类非语义串）改为 `M1B_B := m1b-b` + `VERSION ?= $(M1B_B)+git_$(COMMIT)`，ldflags 注入值形如 `m1b-b+git_19f3a339`；两二进制 `--version` 输出 `<bin> m1b-b+git_<short_hash> (commit <short_hash>)`（`meshconsole --version`/`meshagent --version` 实测核对；commit 注入链不动，agent 心跳 `agent_version` 字段同样携带该语义版本） |
| 6 | 阻塞：DELIVERY「全部通过」夸大。DELIVERY.md M1b-b 批次头改口径——状态行改为「**本机可验证项全部自测通过**」并明示两项未做；§四已知限制重组为显式「**部署上机项清单**」（①真 headscale 连测（云机现成实例）②浏览器人眼过面板布局/动效/玻璃质感③Windows 真机）+ 逐项现状与兜底说明；「全部通过」「§7 验收清单全部自测通过」表述全文消除（REVIEW.md R19 前的自测报告为历史记录不回改，R19 已裁决其口径问题） |
| 7 | 建议：Host/Origin 校验严格化。`internal/panel/panel.go` `hostAllowed` 重写：`url.Parse("http://"+raw)` 解析 authority 后对 hostname 严格比对白名单（localhost/127.0.0.1/[::1]）——拒绝 `url.Parse` 报错、userinfo（`evil@localhost`）、携带 path/query/fragment、非数字/越界端口（>65535/0）、空端口（`localhost:`/`[::1]:`，u.Host 后缀 `:` 判定）、方括号非 IPv6 字面量（`[localhost]`/`[127.0.0.1]`，方括号仅接受 ParseIP 通过且 To4()==nil 的 IPv6）、裸 IPv6（`::1` 解析不出合法 authority）、首尾空白/内嵌空白；`originAllowed` 加 scheme 限 http/https（ftp/chrome-extension 等拒绝）+ 拒绝 userinfo/Opaque/path/query/fragment。测试 `panel_test.go` `TestGuardHostAllowlist` 拒绝清单补 `localhost:evil`/`localhost:`/`[::1]:`/`:0`/`:99999`/`[localhost]`/`[127.0.0.1]`/`::1`/`evil@localhost`/`localhost/evil`/` localhost` 十一变体（允许清单补 `LocalHost`）；`TestGuardOrigin` 改表驱动补 ftp/chrome-extension/userinfo/path/query/无 scheme 六拒绝形态。真机冒烟复核全过（`evil@localhost` 由 net/http 层 400 先拒，handler 层 403 兜底为双层防御） |
| 8 | 建议：mem/disk total=0 行整行剔除。`internal/store/store.go` `MetricsStatsSince`：删除 WHERE 中 `(mem_total IS NULL OR mem_total>0) AND (disk_total…) ` 整行过滤，占比分指标改 `AVG/MAX(mem_used*100.0/NULLIF(mem_total,0))`、`MAX(disk_used*100.0/NULLIF(disk_total,0))`——除零样本只让对应占比保持 NULL，**CPU 等可用指标照常参与**（COUNT 为窗口内全部样本数，与 `MetricsSeriesSince` 既有 NULLIF 口径一致）。测试 `store_m1bb_test.go` `TestMetricsStatsZeroTotals`：total=0 行（cpu=50）入窗口 → Samples=2、CPUAvg=31.25/CPUMax=50（旧实现为 1/12.5），mem/disk 占比仅由 seed 行贡献 25%/10% |
| 9 | 建议：失败计数单次成功即归零致节流被反复突破。`internal/headscale/headscale.go`：Run 循环结果处理抽 `handleResult`（可单测直调），新增 `successResetStreak=3` 与 `streak` 计数——失败计数在**连续成功 3 次**后才归零并 INFO `recovered`，期间的成功记 Debug `recovery pending`；闪断（失败→单次成功→失败）不再归零计数、不再重置节流窗口（WARN 保持 1/h 上限），streak 在任一失败时清零。测试 `headscale_test.go` `TestFailureCounterStreakReset`（捕获式 slog handler：fail/ok/fail 序列计数累计到 2 不归零、全程 WARN 恰 1 条、连续 3 次成功后 INFO 恰 1 条且 fails/streak 归零；对照单次成功夹失败场景）；`TestRunRecoveryAndThrottle` 行为级回归保持通过 |
| 10 | 建议：headscale url scheme 未限制。`internal/config/config.go` `validateHeadscale`：scheme 白名单收紧为 `http`/`https`——`ftp://`/`file://`/`ssh://`/`javascript:` 等其他 scheme 与无 scheme 形式（`127.0.0.1:8080`，解析为空 scheme 空 host）启动即报错，报错注明 scheme 限制（不回显 url 原值，防配置中可能的 userinfo 泄漏）。测试 `config_test.go` `TestHeadscaleURLScheme`：五非法形态全拒（报错含 HTTP(S)）+ http/https 两合法形态正常加载 |
| 11 | 建议：SVG stop-color 无效值 / easing 不一致 / 状态点跳变。`internal/panel/web/app.js` 三处：① **SVG 完整色值**——表现属性（stop-color/stroke）不接受 `var()`（CSS 变量在 presentation attribute 非法，原写法渲染结果未定义）：启动时 `getComputedStyle` 读 `--primary` 的 RGB 三元组拼完整 `rgb(...)` 色值（格式校验，读取失败回退 glass.css 字面量 `52 211 153`，token 单一源不变），gradient 两处 stop 与折线 stroke 全部改完整色值；② **easing 统一**——`tweenNum` 的 `1-Math.pow(1-p,3)`（时间参数直代 y(p)，非 CSS 语义）替换为 `EASE`：与 glass.css `--ease` 同一条 `cubic-bezier(0.22,1,0.36,1)` 的贝塞尔求值（牛顿迭代解 x(t)=x 再取 y(t)，8 次迭代收敛）；③ **状态点原地更新**——minis/services/agents/tailnet 四类行与 lead 卡状态格重构为「build 建结构一次、update 原地改 className/textContent」：`.dot` 的 background/box-shadow transition（glass.css 既有 .4s var(--ease)）此前因每次轮询 `replaceChildren` 重建元素从不生效，现状态色变连续过渡，呼吸类切换不再重建节点。验证：`node --check` 语法过；EASE 与标准贝塞尔二分参照实现全曲线对比最大偏差 2.67e-5；SVG 属性 grep 确认无残留 `var()`（`TestStaticServed` glass-inset 契约保持） |
| 12 | 可选：全量替换回滚断言缺失。`internal/store/store_m1bb_test.go` 新增 `TestReplaceTailnetNodesRollbackKeepsOld`：同批两行第二行主键冲突（确定性中途失败）→ `ReplaceTailnetNodes` 报错，断言旧两行（cloud-1/mac-mini）字节级原样保留——先清后插的 DELETE 不因中途失败单独生效，事务回滚语义钉死（与 R11-F `TestHeartbeatFullAtomic` 同型断言） |

**验证记录**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（**130 用例**：cmd/console 3、agent 14、collect 19、agentdisc 9、config 18、headscale 8、mcpserver 5、meshview 3、panel 5、pki 11、registry 17、store 18）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过。真机冒烟（/tmp 临时实例，已清理）：见本节首段。对用户可见的行为变化：非法 headscale 响应不再清库（保留旧数据+WARN）；`--version` 版本号改 `m1b-b+git_<hash>` 语义格式；面板异常服务数与 get_mesh_status 同口径（300s 宽限生效）；Host/Origin 更严格（裸 `::1`、空端口等畸形一律 403）；headscale url 配置非 http(s) scheme 拒绝启动；total=0 样本不再拖累 CPU 摘要。

## R21 · codex 复审 M1b-b 修复轮（2026-10-08 11:55，值班员执行）

**结论：修改后通过。** R19 十二条中十条核销关闭，两条残余；另新增两条建议。值班员先行独立验证（.pipeline/verify-m1b-b-fix.out，VERIFY_EXIT=0）：`go vet` 零输出、`gofmt -l` 无文件、`go test -count=1` 全绿（12 包全 ok）、`make cross` 三平台通过。codex 只读复审存档 .pipeline/review-m1b-b-fix.out，全程未修改仓库文件。SPEC 符合度：MCP stdio/只读开库/五工具/同源查询/静态 embed 与安全边界符合；真 Headscale 连测与浏览器人眼验收仍为部署上机项（commit 后由 Hana 主会话执行）。

### 裁决（Hana）

| # | 意见 | 级别 | 裁决 | 口径 |
|---|------|------|------|------|
| 1 | headscale.go:130 非法响应校验不严——`dec.More()` 非 EOF 校验，`{"nodes":[]}]` 等尾随垃圾仍被接受并清库；4MiB 截断可伪装正常结束 | 阻塞 | 采纳 | 二次 `Decode` 要求 `io.EOF` 的严格校验（M1a `decodeJSONStrict` 同口径）；读满 4MiB 仍未见结尾或 `io.ErrUnexpectedEOF` 一律 malformed 拒绝，走既有「保留旧数据+错误」降级链。**返工计数：R19 同条意见第 2 次修复，本轮复审仍不过即触发上限请示伦哥** |
| 2 | headscale.go:223/237 WARN 节流未独立于失败计数——恢复归零后再失败立即 WARN，「失败→恢复→失败」循环可突破 1/h 上限 | 建议 | 采纳 | WARN 节流窗口独立维护（记上次 WARN 时刻，与 streak/失败计数解耦），任意时刻相邻两条 WARN 间隔 ≥1h；恢复 INFO 不受限 |
| 3 | app.js:529 tailnet 空→有数据时 `.empty` 提示不删除，在线节点与 "disabled or not synced yet" 同显 | 建议 | 采纳 | syncList 拿到数据即移除空态提示节点 |
| 4 | Makefile:24/27 重建依赖缺 Makefile/go.mod/go.sum，已有 bin/ 产物时跳过构建，携带旧版本/旧依赖 | 建议 | 采纳（轻量版） | bin 目标补依赖 Makefile go.mod go.sum；commit 哈希变化不进依赖（避免每次提交全量重建），版本新鲜度由交付流程显式 `make cross` 保证 |

**返工计数**：#1 第 2 次修复（m1b-b-fix 首修未完全关闭）；#2/#3/#4 首次出现，均未触发「同条修两次不过」上限。

**修复轮范围**：#1/#2/#3/#4 全部 → 派 m1b-b-fix2。验证口径不变：`go vet` / `gofmt` / `go test -count=1` 全绿 + `make cross` 三平台。

## R22 · zcode 修复轮 2 / m1b-b-fix2（2026-10-08 完成）

**R21 四条意见全部按裁决口径修完。** #1 为 R19-#1 同条意见第 2 次修复，本轮按 registry `decodeJSONStrict` 同口径彻底关闭（详见下表）；#2/#3/#4 首次修复。测试 headscale 包 8 → 10 个用例（新增 `TestFetchNodesTruncatedOversized`、`TestWarnThrottleIndependentOfReset`，`TestFetchNodesIllegalBody` 扩至 9 个非法形态）。无 git commit。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | 阻塞（R19-#1 残余，第 2 次修复）：`dec.More()` 对 `]`/`}` 起始的尾随垃圾返回 false（`{"nodes":[]}]` 被静默放过并清库），且 `io.LimitReader` 截断点恰落在完整值之后时「读满上限」被伪装成正常 EOF。`internal/headscale/headscale.go` FetchNodes 重写校验链：① body 先 `io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))` 整读（新常量 `maxRespBytes=4<<20`，:128）——`len > maxRespBytes` 即「读满上限仍未见结尾」，显式报 `headscale response exceeds 4194304 bytes (malformed body)`（:132-134），多读 1 字节保证截断探测不误伤恰满 4MiB 的合法 body；② 解码改在 `bytes.NewReader` 上进行，首次 `Decode` 遇 `io.ErrUnexpectedEOF`（中途截断）显式报 `truncated mid-value (malformed body)`（:138-141），不与语法错混同；③ 尾部校验弃 `dec.More()` 改二次 `Decode(&struct{}{})` 必须返回 `io.EOF`（:152-155，registry `decodeJSONStrict` 同口径）——`{"nodes":[]}]`/`{"nodes":[]}}`/拼接第二个 JSON 值一律 `trailing data after JSON value (malformed body)`。三条错误路径全走既有「返回错误 → Sync/Run 保留旧数据 + WARN 计数」降级链，只有严格合法的 `{"nodes":[...]}`（含空数组）才全量替换。测试：`TestFetchNodesIllegalBody` 补 trailing bracket/brace/two json values 三形态（共 9 非法 + 空数组合法对照）；新增 `TestFetchNodesTruncatedOversized`（完整值+4MiB 尾随空白、mid-value 截断、mid-string 截断三形态均拒绝且报 malformed/exceeds）；`TestSyncIllegalResponseKeepsOld` 降级链回归保持 |
| 2 | 建议（R19-#9 残余）：`f.fails == 1 \|\| time.Since(...)` 的短路使恢复归零后的首个新失败绕过 1h 窗口立即 WARN，「失败→恢复→失败」循环可突破上限。`internal/headscale/headscale.go` `handleResult`（:245-256）：删除 `fails == 1` 特判，节流只看 `time.Since(f.lastWarn) >= warnThrottle`（窗口独立于计数/streak；lastWarn 零值 → 首个失败仍立即 WARN）；窗口内失败降级 `Debug "headscale sync failed (warn throttled)"` 不输出 WARN；恢复 INFO `recovered` 不受限。`Fetcher` 注释与包文档同步（节流窗口独立维护，R21-#2）。测试新增 `TestWarnThrottleIndependentOfReset`：fail → 3×ok（恢复归零）→ fail → fail，全程 WARN 恰 1 条、恢复 INFO 恰 1 条、节流 Debug 恰 2 条、失败计数跨恢复累计到 2；`TestFailureCounterStreakReset`/`TestRunRecoveryAndThrottle` 回归保持 |
| 3 | 建议：tailnet 空→有数据时 `.empty` 空态提示残留，"disabled or not synced yet" 与节点行同显。`internal/panel/web/app.js` `syncList`（:183-185）：`items.length > 0` 时先 `container.querySelectorAll(':scope > .empty').forEach(remove)` 再做行 diff——空态提示不在行复用 map 内，由 syncList 统一负责移除（四类列表共用该路径，tailnet 空→有数据的下次轮询即收敛）。`node --check` 语法过；静态资源经 embed 依赖随 bin 重建（R21-#4 后 Makefile go 产物依赖链覆盖） |
| 4 | 建议（轻量版）：bin 产物目标缺 Makefile/go.mod/go.sum 依赖，已有产物时 `make build` 跳过构建，携带旧依赖/旧 ldflags 配方。`Makefile`（:23-32）：新变量 `BUILD_DEPS := Makefile go.mod go.sum`，`bin/meshconsole`/`bin/meshagent` 两目标补挂（构建配方与依赖清单变更触发重建）；commit 哈希变化不进依赖（不因提交触发全量重建），版本新鲜度由交付流程显式 `make cross` 保证（R21 裁决口径）。实测：touch go.mod → `make build` 两产物立即重编（mtime 刷新） |

**验证记录（m1b-b-fix2）**：`gofmt -l` 无文件；`go vet ./...` 零输出；`go test -count=1 ./...` 全绿（见 DELIVERY.md §五 更新后的用例数）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check internal/panel/web/app.js` 过；Makefile 依赖行为实测（touch go.mod → bin 重编）。对用户可见的行为变化：① headscale 对尾随垃圾（含 `]`/`}` 起始）、超 4MiB、中途截断的 200 响应一律按拉取失败处理（保留旧数据），其中部分形态此前会被误接受甚至清库；② 「失败→恢复→再失败」场景 WARN 严格 1/h（窗口内降级 Debug），不再因计数归零立即重复告警；③ 面板 tailnet 列表从空态转有数据时提示文案不再与节点行同显；④ bin 产物在 go.mod/go.sum/Makefile 变更后自动重建，不再复用旧依赖产物。

## R23 · codex 复审 m1b-b-fix2 + 裁决放行（2026-10-08 12:30，值班员执行）

**codex 结论：通过（代码审查）。** R21 四条全部核销关闭，未发现新增阻塞；全程未修改仓库文件。复审存档 .pipeline/review-m1b-b-fix2.out。核销明细：① headscale.go 非法响应校验（4MiB 超限探测 + 严格二次 Decode EOF）正确、非法响应保留旧数据——**R19-#1 同条意见第 2 次修复后彻底关闭**（未触发「同条修两次不过」升级：该条件指修两次仍不过）；② WARN 节流窗口独立于失败计数，恢复归零不再绕过 1h 上限；③ 面板空态提示随数据出现正确移除；④ Makefile bin 产物依赖 Makefile/go.mod/go.sum，符合轻量版裁决。SPEC 符合度确认：MCP stdio/只读库/五工具/同源查询/Headscale 事务替换/静态 embed 与 Host-Origin 边界符合。

### 裁决（Hana，值班员自主）

| # | 级别 | 意见 | 裁决 | 口径 |
|---|------|------|------|------|
| 1 | 可选 | headscale.go:41 注释仍称恢复会重置「节流窗口」，与 R22-#2 后实现（窗口独立维护）不符 | 采纳转候选 | 纯注释改动、不影响通过，转 M1b-c 候选清单（R17「可选无动作」同口径），不派修复轮 |

**裁决结论：通过 → 放行 commit（COMMIT_OK）。** m1b-b 至此完成 R19→R21→R23 两轮修复收敛（fix/fix2 各一轮，未触 3 轮护栏），由 worker 执行 commit 后回 idle。真 Headscale 连测与浏览器人眼验收仍为部署上机项，commit 后由 Hana 主会话执行。

**M1b-c 候选清单（累计）**：① headscale.go:41 注释文案与节流窗口实现同步（本轮新增）；② pki.go 证书生成前校验 tailnet_ip 合法性（R17-#1）；③ config.go systemd/docker target 256 字节启动校验（R17-#2）；④ agent/runner.go:135 注释三态语义同步（R17-#3）。

---

# 自测报告 · zcode M1b-c（2026-10-08）

**结论：SPEC-M1b-c 五节逐条落地，本机可验证项全部自测通过；真机 cloud-agent 上报与浏览器人眼验收列部署上机项。** 验证基线：`go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **162 用例全绿**（12 包全 ok；M1b-b 收口 132 → 净增 30）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过、`node --check internal/panel/web/app.js` 过。未执行 git commit。

## SPEC §5 验收清单逐条

| # | 验收项 | 结果 | 证据 |
|---|--------|------|------|
| 1 | vet/gofmt/test 全绿 + cross 三平台 + 点名单测 | ✅ | SPEC 点名五类全覆盖：**进程匹配解析**（`collect/agenttasks_test.go` `TestScanProcessesParse`：首 token 基名匹配、无辜进程不误报（vim 打开会话文件/grep 参数含 agent 名）、表头残行跳过、cpu/mem 解析非法报 null）、**etime 反推**（`TestParseEtime` 九形态含 `mm:ss`/`hh:mm:ss`/`dd-hh:mm:ss`/非法输入返回 0 不编造；`TestScanProcessesParse` 断言 started_at=now−etime）、**三态语义**（`registry_m1bc_test.go` `TestHeartbeatAgentTasksThreeStates`：显式数组落库→缺席无变化→空数组清空→显式 null 400 且整条不落 metrics）、**migration v4**（`store_m1bc_test.go` `TestMigrationV4Schema`：schema 版本 ≥4 + ai_agents 新列 PRAGMA 断言）、**list_agent_tasks**（`mcpserver_test.go` `TestCallListAgentTasks`：真数据 + 节点过滤 + 未知节点空清单；`TestHandshakeToolsList` 六工具不许多不少） |
| 2 | 真机：伪装进程匹配与消失清行 | ✅（本机 E2E）/ cloud-agent 上报待上机 | 本机 console+agent 全链路（/tmp/m1bc-e2e，心跳 5s）：`exec -a zcode /bin/sleep 300` 伪装进程 → 下一拍 overview `running_tasks` 出现 `zcode pid=33793 elapsed/started_at（etime 反推）`；进程自然退出 → 1-2 拍内 zcode 从快照消失（清行）。附带真实验证：本机在跑的 3 个真实 codex 进程（含 ChatGPT.app 内嵌 codex-cli）全部被正确匹配上报。**cloud-agent（云机）上报 agent_tasks 列部署上机项** |
| 3 | 面板亮色 + 中台三区块人眼验收 | ✅（结构/数据自测）/ 人眼待上机 | 亮色为默认（`data-theme="light"`，curl 实证）；顶栏四仪表（online/running tasks/svc issues/tailnet up）+ 运行任务大卡区（`running_tasks` 聚合、空态「全部安静」）+ 终端卡区（每终端状态/角色/sparkline/disk/agent 清单带 last_activity）+ 服务/tailnet 两区块保留；overview 聚合 JSON 实证含 `running_tasks` 与 agents `last_activity/session_files`。**浏览器人眼（亮色玻璃质感/动效观感）列部署上机项**（本机 Edge headless 受沙箱限制无法截图，与 M1b-b 同口径；Node+最小 DOM 桩冒烟以真 overview 数据跑通 render 全路径——任务大卡/终端卡/agent 清单/空态往返/活动计时 tick 全断言，抓出并修复 2 个运行时缺陷，见「四个真实缺陷」节 3/4） |
| 4 | MCP list_agent_tasks 返回真数据 | ✅ | 本机 E2E：stdio 逐帧 `initialize` → `tools/list` 六工具 → `tools/call list_agent_tasks` 返回 4 条真任务（3×codex 真实进程 + 1×zcode 伪装进程，含 pid/elapsed） |
| 5 | REVIEW/DELIVERY 更新；禁止 commit | ✅ | 即本节 + DELIVERY.md 顶部 M1b-c 交付说明；全程未 commit |

## SPEC 各节落地说明

- **§1 亮色主题**：token 三元组换亮色（emerald 700 `4 120 87` / amber 700 `180 83 9` / red 600 `220 38 38`，白底对比度 4.8/4.6/4.5:1，等宽字数据/状态点/offline 红全部白底可读）；语义层换冷调 porcelain/ink 中性系（非纯灰）；frost 按技能「light-mode tuning」收薄（panel mix 42/46%→30/34%）+ 拉高 saturate（190/200%→210/215%）+ 亮 specular（白 lip 0.55/0.65）；atmosphere 换亮色 mesh（blob `screen`→`multiply`，`--aura` 0.30→0.24——技能 theming.md：暗色 aura 复用亮色会发闷）；glass-inset 亮色改 ink 轻染 + 白顶缘（白 elev 叠白板不可见）。**暗色实现完整保留在 `[data-theme="dark"]` 变量块（三元组+语义层+材质覆盖+网格线），TODO 注释标 M1c+ 切换**，翻 body 属性即可整体回暗、组件零改动。
- **§2.1 agent 侧采集**（`internal/agent/collect/agenttasks.go` 新文件）：进程扫描 unix `ps -eo pid,etime,pcpu,pmem,command`（首 token 基名匹配，宁漏不误——解释器包装漏报如实）/ Windows `tasklist /FO CSV /NH` 兜底（cpu/mem/etime/started_at 缺席报 null 不填 0）；会话目录 stat（known 内置映射 zcode `~/.zcode/cli/rollout`、codex `~/.codex/sessions`、claude `~/.claude/projects`、gemini `~/.gemini/tmp`；aider 无公认目录不统计；custom 走配置新字段 `agent_scan.custom[].session_dir`，同名声明优先）——**只 stat 不读内容**（WalkDir 的 d.Info() 即 lstat），取树内文件最近 mtime + 文件数（文件数上限 8192 防失控），目录缺失该 agent 缺席（null）。随心跳上报：`agent_tasks` 顶层数组（三态沿用：进程扫描**成功**即上报含空数组清空；**扫描失败缺席字段 + collect_errors 说明——失败≠没有任务，绝不发空数组清行**）+ `agents[].last_activity/session_files`（runner 每拍把活动度合并进缓存的发现清单，15s 刷新不等 5 分钟扫描）。整轮预算 8s 超时中断；单 beat 任务数上限 64。
- **§2.2 console 侧**：migration **v4**（`agent_tasks` 表 node_id/pid/agent_name/cmd/elapsed_s/cpu_pct/mem_pct/started_at/updated_at + `ai_agents` 增列 last_activity/session_files；节点删除级联清任务行）；心跳 `agent_tasks` 三态（`optionalArray` 泛型沿用 R11-A：显式 null 400、缺席不覆盖、数组全量替换）**按节点先清后插全量替换**（进程消失=任务结束不留历史行，无 stale 簿记态）；`HeartbeatFull` 单事务扩到 metrics+services+agents+tasks 四组写入；校验（`validateAgentTasks`）：agent_name 非空 ≤128、pid>0 且同 beat 去重、elapsed_s≥0、cpu/mem ∈[0,100]（NaN/Inf 拒绝）、started_at 非负且不晚于 now+1d（时钟漂移容忍）、cmd 服务端再截断 200 字节+去控制字符、条目 ≤64；`/api/panel/overview` 聚合扩展 `running_tasks`（全网任务快照带节点名）+ agents 带活跃度；MCP 新增只读 `list_agent_tasks(node?)`（六工具）。
- **§2.3 面板中台信息架构**：顶栏 readout 四大数字（在线终端/运行中任务总数（>0 亮品牌绿「活着」）/服务异常/tailnet 在线——tailnet 仪表从 tracked 改为 **online** 口径）；**运行中 agent 任务大卡区**（col-12 通栏、auto-fill ≥330px 大卡：终端名/agent 名/cmd 摘要（ellipsis+title 全文）/活动计时（**本地每秒走动**，15s 轮询刷新锚点消漂移）/cpu、呼吸绿点；无任务显示「全部安静」空态且空态元素受管可往返）；终端区每终端一张卡（状态点/角色 chip/心跳/cpu+mem sparkline/disk meter/该终端 agent 清单行带 `act <rel>` last_activity 与文件数 title）；服务与 tailnet 两区块保留（亮色化，tailnet 行的 ips 列修复后可见，见 §4）。
- **§4 顺手修 tailnet ips=null**：根因=headscale 版本演进中节点地址字段改名——新版 `addresses`、旧版 proto `ip_addresses`（JSON `ipAddresses`），旧解析只认 `addresses` → 旧版部署 ips 空串 → 视图 null。修复：wireNode 双字段声明、取非空者（同现时优 addresses 不拼接）；`availableRoutes` 是子网路由（非 100.x 节点地址）**刻意不采**。单测：旧字段响应解析入库（`TestFetchNodesLegacyIPAddresses` 全链路到 tailnet_nodes.ips）+ 双字段同现优先级（`TestFetchNodesAddressesPreferred`）。真 headscale 上机连测仍列部署上机项。

## 真机 E2E / DOM 冒烟抓出并修复的四个真实缺陷（自测价值记录）

1. **known 清单会话目录 `~` 未展开**：内置映射值直接 `os.Stat("~/.…")` 失败 → last_activity 恒 null。修复：`newAgentTaskScanner` 对内置映射统一过 `config.ExpandHome`（与 custom 的 session_dir 同口径）。本机 E2E 实证修复后 zcode last_activity/files=3 上报。
2. **ps 全量输出被 64KB 级上限截断、高 pid 进程行静默丢失**：任务采集复用了服务查询的 `maxExecOutput=64KB`，真机（macOS 558 进程）`ps -eo …command` 实测 ≈150KB → 截断点后的进程（本例伪装 zcode pid=33793）被静默丢弃且表现为「任务消失」的错误快照。修复：任务采集独立 `TaskScanMaxOutput=1MB` 封顶（R10-#5 有界语义不变），且**触顶显式报错**（缺席字段+collect_errors）而非拿不完整清单硬扫——回归单测 `TestScanOutputTruncatedIsError` 钉住。
3. **终端卡 build 漏挂状态字段**（Node + 最小 DOM 桩冒烟抓出，M1b-b 同款手段）：`buildNodeCard` 未把 dot/name/role/hb 挂到 card，`updateNodeCard` 首帧即 `undefined.className` TypeError——纯 `node --check` 抓不到的运行时缺陷。修复并纳入冒烟断言。
4. **「全部安静」空态会同显两份**（DOM 桩冒烟抓出）：syncList 克隆 `[data-empty]` 占位后原型节点未从容器移除，任务清零时原型+克隆两份同显。修复：克隆受管、原型立即移除；冒烟钉住「有数据→空态回位→任务恢复」三段往返。

## 已知限制与上机项（不含糊）

- **部署上机项**：① cloud-agent（云机）升级 m1b-c 二进制上报 agent_tasks/last_activity 真机核对；② 真headscale 连测验证 ips 字段（旧版 ipAddresses 形态是否为现网实际形态待核——本机 mock 双形态已覆盖）；③ 浏览器人眼过亮色面板（frost 薄饱和/atmosphere pastel/任务大卡/活动计时动效）。
- 本机验证边界内限制：① known 会话目录映射内置四家（aider 无公认固定目录不统计，last_activity 如实 null；可用 `known: []`+custom+session_dir 整体替换）；② 进程匹配只看命令行首 token 基名——解释器包装（`python -m aider`）与改名进程（如本机真实 `zcode-host-local-1`）漏报如实（宁漏不误，注释与 DELIVERY 均声明）；③ 任务历史档案不做（SPEC §3 非目标，agent_tasks 表仅存当前快照）；④ 面板任务大卡的活动计时在 15s 轮询间隙本地走动，锚点随每拍校准（时钟偏移容忍）；⑤ Edge headless 在本沙箱无法截图（与 M1b-b 同因），面板视觉自测以 DOM/数据链路实证+人眼上机验收兜底。
- 顺手修正：Makefile 版本批次号 `m1b-b` → `m1b-c`（R19-#5 口径：版本=批次号），`--version` 输出 `m1b-c+git_<hash>`。

## R27 · codex 首审 m1b-c + 裁决（2026-10-08 15:20，值班员执行）

**结论：修改后通过（裁决）。** codex 首审 m1b-c（review-m1b-c.out，63,055 tokens）判**不通过**：2 阻塞 + 2 建议；R23/R21 核销 4 关闭 + 1 可选残留。SPEC 主链路（亮色、中台布局、migration v4、三态、MCP 六工具）确认符合。值班员逐条抽验源码后裁决如下（轮次号跳至 R27：脚本注释已占用 R24/R26 作巡检教训编号，避免撞号）。

### 裁决（Hana 值班员，按伦哥授权自主裁决流程性问题）

| # | 意见 | 级别 | 裁决 | 修复口径 |
|---|------|------|------|----------|
| 1 | 进程 CPU 值域不一致：`registry.go:619`/`agenttasks.go:273`，多核 ps %CPU 可 >100%，采集端原值上报、服务端 ∈[0,100] 校验拒绝 → 整条心跳与指标丢库 | 阻塞 | 采纳 | 服务端 `validateAgentTasks` cpu_pct 放宽为「有限、≥0」（只拒 NaN/Inf/负数，不设伪上限）；采集端保持 ps 原值（%CPU 多核超 100 是 ps 语义非脏数据）。补多核 >100 通过的单测 |
| 2 | 目录遍历未传 context：`agenttasks.go:193、315`，8192 上限只数文件、海量空目录仍无限遍历，8s 预算对该路径失效 | 阻塞 | 采纳 | WalkDir 全路径吃 ctx（8s 预算真正生效）；8192 上限改为文件+目录合计计数，触顶按截断处理（与既有 truncated 语义同口径：如实报不完整） |
| 3 | 离线/扫描失败节点旧任务仍显示运行、持续计时：`meshview.go:457`/`app.js:315` | 建议 | 采纳 | 该节点快照过期标记（面板任务卡/计时随节点失联态停走+标注），避免误报 |
| 4 | >64 任务静默截断全量覆盖：`agenttasks.go:283` | 建议 | 采纳 | 触顶时上报截断状态（清单不完整可见），面板相应标注 |
| 5 | R23 可选残留：`headscale.go:41` 注释仍误称恢复重置节流窗口 | 可选 | 顺手修 | 一行注释文案对齐实际行为 |
| — | SPEC:49,50 云机上报 + 人眼验收未完成 | 上机项 | 维持列部署上机项 | 与 M1b-b 同口径：本机沙盒边界所致，REVIEW/DELIVERY 已列明，不阻塞本轮 |

**修复轮范围**：本表 5 条全修 → 派 m1b-c-fix。验证口径不变：go vet / gofmt / go test -count=1 全绿 + make cross 三平台 + 两阻塞各补单测，M1b-c 已验收项不回退。禁止 git commit。

## R28 · zcode 修复轮 m1b-c-fix（2026-10-08 完成）

**R27 裁决 5 条全部按口径修完**（2 阻塞 + 2 建议 + 1 可选顺手修）。修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **171 用例全绿**（修复前基线 162 + 本轮新增 9：collect 3、registry 3、store 2、meshview 1；12 包全 ok）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过、`node --check` 面板 JS 过 + Node DOM 桩冒烟（stale 冻结/截断标注/meta 口径/空态往返全断言）通过。未执行 git commit。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | 阻塞：进程 CPU 值域统一。`internal/registry/registry.go:629-633` `validateAgentTasks` 拆分双字段口径——新增 `finiteNonNeg`（`:596-598`，只拒 NaN/Inf/负数），**cpu_pct 仅须有限且 ≥0 不设上限**（多核 ps %CPU 超 100 是 ps 语义，8 核打满 = 750，设伪上限会把重载心跳连任务快照带指标整条丢库）；**mem_pct 为常驻物理内存占比维持 [0,100] 封顶**（同函数 `:632`）。采集端不动：`agenttasks.go` 保持 ps 原值上报，`AgentTask` 结构注释钉住口径（`:66-70`）。测试：`registry_m1bc_test.go` `TestHeartbeatAgentTasksCPUAbove100`（cpu_pct=750 心跳 200 且落库 750.0、同拍 mem 42 正常）+ `TestValidateAgentTasksNonFinite`（±Inf/NaN/负数 × cpu/mem 八形态直测拒绝、cpu=750/mem=100 边界放行）+ 既有值域用例改造（原「cpu 100.5 → 400」按新口径改负数/负 mem/mem>100 三拒绝态） |
| 2 | 阻塞：目录遍历可控。`internal/agent/collect/agenttasks.go`：① `Scan`（`:184-198`）把整轮 8s 预算的 bctx 传入 `scanActivity(ctx)`（`:309`）→ `statDirTree(ctx, root)`（`:328`）——WalkDir 回调每条目先查 `ctx.Err()`，超时即终止整树返回 ok=false（预算内没数完 = 未知，缺席如实上报，不拿半程结果冒充；`:336-338`）；② 8192 上限改**文件+目录合计计数**（`entries` 计数器含根目录，`MaxSessionEntries` 取代 `MaxSessionFiles`，`:38-41`）——海量空目录不再绕过上限无限遍历；③ 触顶当前条目照常计入后 `SkipAll` 停止并置 `DirActivity.Truncated`（`:84-90` 新字段），runner（`runner.go:170-184`）把截断 agent 名单如实并进 `collect_errors["agent_activity"]`（与 ps 输出触顶同口径：截断的 Files 是下界不静默；目录缺失/预算耗尽本就缺席不在此列）。测试：`TestScanSessionDirCapCountsDirs`（8292 空目录收敛、Files==0 证明上限被目录命中、Truncated=true）、`TestStatDirTreeRespectsCtx`（预取消 ctx → ok=false）、`TestScanActivityBudgetExpires`（1ns 注入预算 → 活动度缺席=未知）、`TestScanSessionFileCap` 更新（根目录占 1 条目位、Files=cap-1、Truncated=true） |
| 3 | 建议：离线/扫描失败节点旧任务误报。服务端口径唯一化：`internal/meshview/meshview.go` 新增 `taskStaleAfter=90s`（offline_after 60s 的 1.5 倍，`:22-27`）；`AgentTask` 视图新增 `Stale bool json:"stale"`（`:94-95`）；`AgentTasks`（`:458-484`）载入节点状态表，**`Stale = 节点 status != "online" || updated_at < now-taskStaleAfter`**——前者覆盖节点失联，后者覆盖「节点在线但进程扫描持续失败」（失败时 agent_tasks 字段缺席、旧快照原样保留的库内形态）；overview 与 MCP list_agent_tasks 同源受益（面板任务大卡与 get_mesh_status 消费同一标记）。面板：`app.js` `updateTaskCard`（`:312-352`）stale 时**计时冻结在快照值、移出每秒走动集合 liveRows、呼吸点停转 warn、卡上「snapshot stale」标注**（`.task-flag` chip，glass.css 新增，琥珀 accent 色+文字双通道）；`renderReadout`（`:236-237`）运行任务仪表只计非 stale；`tasks-meta` 文本带 `N stale`。测试：`meshview_m1bc_test.go` `TestAgentTaskStaleMarking`（离线节点新鲜快照 → stale；在线新鲜 → 不 stale；在线但快照拨旧超窗 → stale；JSON 契约含 `"stale"` 键）；DOM 冒烟断言冻结/不进 liveRows/标注/恢复路径 |
| 4 | 建议：任务清单截断可见。全链路：① 采集端 `scanProcesses` 返回 `(tasks, truncated, err)`（`agenttasks.go:211`）——触顶**不 break**，剩余行继续解析只计数（输出已 1MB 封顶代价可忽略）换准确判定：匹配数 >64 才置 truncated，恰 64 不误报；`Scan` 四返回值（`:190`）。② runner（`runner.go:200-202`）心跳体加顶层可选字段 `agent_tasks_truncated: true`（未截断不带该字段=缺席）。③ console：`heartbeatReq.AgentTasksTruncated *bool`（`registry.go:356-360`，缺席=false；仅随显式数组生效，数组缺席时忽略）；store **migration v5** `nodes.tasks_truncated` 列（`store.go:293-301`，NOT NULL DEFAULT 0）；`HeartbeatFull` 签名扩 `tasksTruncated bool`（`:498`）——tasks 非 nil 时与快照**同事务**写 `nodes.tasks_truncated`（`:525-533`，未截断显式快照复位 false，快照缺席不改动，无半拍错位）。④ 视图与面板：`NodeRecord`/`Node`/meshview `Node.TasksTruncated`（`tasks_truncated,omitempty`）透传；`app.js` `render` 按节点名取 `tasks_truncated`（`:521-523`），命中节点的任务卡标「list truncated」chip + meta 追加 `· list truncated`（stale 优先占位）。测试：store `TestHeartbeatTasksTruncatedFlag`（true 落库/缺席不动/未截断复位三段）+ `TestMigrationV5Schema`（版本 ≥5 + PRAGMA 列存在）；registry `TestHeartbeatAgentTasksTruncatedFlag`（HTTP 三拍：截断置位 → 数组缺席不动 → 未截断快照复位）；collect `TestScanMaxTasks` 扩断言（>64 truncated=true、恰 64 false） |
| 5 | 可选：`internal/headscale/headscale.go:39-44` `successResetStreak` 注释修正——恢复连续成功 3 次仅归零**失败计数**；WARN 节流窗口独立维护、不随恢复重置（R21-#2/R22-#2 后的实际行为），任意时刻相邻两条 WARN 间隔仍 ≥ warnThrottle。纯注释改动 |

**验证记录（m1b-c-fix）**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（**171 用例**：cmd/console 3、agent 14、collect 33、agentdisc 9、config 18、headscale 12、mcpserver 6、meshview 6、panel 5、pki 11、registry 24、store 24；其中含 6 个子测试）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check` 与 Node+DOM 桩冒烟（真 overview 数据含 stale/truncated 形态：任务卡标注/冻结计时/仪表口径/meta 文本/空态往返全过）通过。对用户可见的行为变化：① 多核打满进程（cpu_pct>100）的心跳不再被整条拒绝丢库；② 离线/快照过期节点的任务在面板停止计时并标注 snapshot stale，仪表与 meta 不再冒充运行中；③ 任务清单触顶 64 时节点带截断标记、面板标注 list truncated；④ 会话目录海量空目录场景遍历收敛且截断进 collect_errors；⑤ migration v5 自动执行（console 服务模式首次启动，nodes 增 tasks_truncated 列）。M1b-c 已验收项（SPEC §5 清单、R27 前核销项）无回退——既有 162 用例全部保持通过，SPEC 点名用例（三态/migration v4/六工具/解析 etime）原样绿。

## R29 · codex 复审 m1b-c-fix + 裁决（2026-10-08 16:20，值班员执行）

**结论：修改后通过（裁决）。** codex 复审 m1b-c-fix（review-m1b-c-fix.out，47,683 tokens）判**不通过**：R27 五条关闭四条（#1 CPU 值域、#3 stale 冻结、#4 截断标记、#5 注释均核销），阻塞 #2 遍历预算残余 + 新增 2 建议；验证独立复核全绿（171 用例、vet/gofmt/三平台交叉编译），全程未修改仓库文件。SPEC 主链路符合，真机上报/人眼验收沿用 R27 非阻塞裁决。值班员逐条抽验后裁决如下。

### 裁决（Hana）

| # | 意见 | 级别 | 裁决 | 口径 |
|---|------|------|------|------|
| 1 | agenttasks.go:338 `filepath.WalkDir` 内部先一次性整读并排序单目录再逐项回调，超大单目录（数十万文件）读取量不受 8192 条目配额与 8s ctx 约束 | 阻塞 | 采纳 | 弃 WalkDir 改显式分批遍历（Open+ReadDir(n) 分批，批间检查 ctx.Err() 与条目累计配额），触顶/超时立即收敛并如实上报截断（不静默）。实现自由度：分批大小不强制，「单目录读取量受 ctx 与配额双重支配」成立即可。**返工计数：R27-#2 同条意见第 2 次修复，本轮复审仍不过即触发「同条修两次不过」上限请示伦哥** |
| 2 | app.js:613 overview 拉取持续失败时旧卡留在 liveRows 继续计时、显示运行数 | 建议 | 采纳 | 与 R27-#3 stale 冻结同机制：数据超新鲜度期限即冻结（移出 liveRows、停转、标注），恢复自动解冻 |
| 3 | mcpserver.go:146 list_agent_tasks 仅返回任务清单，未透出节点截断标记，MCP 用户不知结果仅为前 64 条 | 建议 | 采纳 | MCP 返回结构补 tasks_truncated（nodes.tasks_truncated 已落库，视图透出即可） |

**返工计数**：#1 第 2 次修复（m1b-c-fix 首修未关闭，仍未触发上限——该条件指修两次仍不过）；#2/#3 首次出现。修复轮计数：m1b-c-fix 第 1 轮、m1b-c-fix2 第 2 轮，未触 3 轮收敛护栏（第 3 轮无论意见多少强制收口转候选）。

**修复轮范围**：本表 3 条全修 → 派 m1b-c-fix2。验证口径不变：go vet / gofmt / go test -count=1 全绿 + make cross 三平台 + node --check；M1b-c 已验收项不回退。禁止 git commit。

## R30 · zcode 修复轮 m1b-c-fix2（2026-10-08 完成）

**R29 裁决 3 条全部按口径修完**（1 阻塞第 2 次修复 + 2 建议）。修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **174 用例全绿**（修复前基线 171 + 本轮新增 3：collect 2、mcpserver 1；12 包全 ok）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过、`node --check` 面板 JS 过 + Node DOM 桩冒烟（拉取失败冻结/解冻全路径断言）通过。未执行 git commit。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | 阻塞（R27-#2 第 2 次修复）：目录遍历预算对超大单目录真正生效。`internal/agent/collect/agenttasks.go` **弃 `filepath.WalkDir`**——其内部对单个目录 `os.ReadDir` 一次性整读并排序后才逐项回调，数十万条目的单目录在回调检查（配额/ctx）发生之前就已无界读盘。改为**显式栈式 DFS + 分批 readdir**：`walkDirTree`（`:346-429`，`statDirTree`（`:331-334`）以生产常量 `MaxSessionEntries`/新常量 `dirReadBatch=256`（`:322-327`）委托）——每个目录 `os.Open` 后按批 `ReadDir(256)`，**批间查 `ctx.Err()`、批内逐条查条目累计配额**（配额先于下一批读取生效，单目录读取量被封顶在「配额 + 一批」≈8448 条之内，不再随目录体积增长）；触顶立即停止并置 `Truncated`（计数口径不变：根目录占 1 位、文件+目录合计、Files 是下界）；ctx 中途超时返回 `ok=false` 走既有「未知=缺席」语义（`DirActivity{}` 零值不外泄半程统计）；单目录打开失败跳过该子树、读错误放弃该目录余量（均与旧 WalkDir 的 SkipDir 口径一致）；不跟随符号链接（ReadDir 条目 `IsDir()` 对 symlink 为 false，天然无环）。上报语义不变：截断仍由 runner 并进 `collect_errors["agent_activity"]`，预算耗尽仍缺席。测试：`agenttasks_test.go` 新增 `TestWalkDirHugeSingleDirQuotaDominates`（984 文件单目录 × 配额 600 × 批 64——强制跨多批，Files 恰停 cap-1、Truncated 如实：配额在批中/批间都能截停）+ `TestWalkDirCtxExpiresMidWalk`（`flipCtx` 第 k 次 `Err()` 确定性翻转——真实 timer 与遍历竞速必闪失——中途超时 → ok=false 且不外泄半程统计）；既有 R28 用例（8292 文件/8292 空目录/入口 ctx/1ns 预算）原样全绿，生产常量下多批路径由 `TestScanSessionFileCap`（8292 文件 ÷ 256 = 33 批）继续覆盖 |
| 2 | 建议：overview 拉取持续失败旧卡冻结。`internal/panel/web/app.js`：模块级记录 `lastGoodOv`/`lastGoodAt`（`:610-612`），新鲜度期限 `FETCH_STALE_MS = 90s`（与服务端 `taskStaleAfter` 同窗口径，R27-#3，约 6 拍轮询）。`tick` catch 分支（`:635`）：超线即调 `renderStaleSnapshot()`（`:618-624`）——把上一次成功 overview 以「全部任务 `stale: true`」形态重渲，**完全复用 render/updateTaskCard 既有冻结语义**（计时移出 liveRows 停走、呼吸点停转转 warn、「snapshot stale」标注、运行仪表与 meta 只计非 stale），不另起炉灶；渲染后再叠 crit「fetch failed · retrying」新鲜度行。窗口内失败（刚断 1-2 拍）保持原样不冻结；恢复刷新后正常 render 自动解冻。DOM 桩冒烟（可控时钟 + fetch 脚本）四拍断言全过：成功渲染计时走动 → 窗口内失败不冻结 → 超 90s 失败冻结（标注/停转/仪表 0/meta「0 running · 1 stale」/计时停走）→ 恢复自动解冻 |
| 3 | 建议：MCP 透出截断标记。`internal/mcpserver/mcpserver.go`：`listAgentTasksOut` 增 `TasksTruncated bool json:"tasks_truncated"`（`:40-47`，恒输出，false=完整清单，消费方免缺席特判）；handler（`:137-170`）在任务清单之外取节点视图按过滤口径求并集——未过滤=全网任一节点截断即 true，按节点过滤=该节点自己的标记，无涉及节点=false（数据源即 R27-#4 落库的 `nodes.tasks_truncated`，零新 SQL）；工具 Description 同步注明标记含义。测试：`mcpserver_test.go` `newSession` 参数化落库标记（`newSessionTasksTrunc`），`TestCallListAgentTasksTruncated`（截断节点 → true；未知节点过滤 → false 且键在）+ 既有用例补 `tasks_truncated:false` 契约断言 |

**验证记录（m1b-c-fix2）**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（**174 用例**：cmd/console 3、agent 14、collect 35、agentdisc 9、config 18、headscale 12、mcpserver 7、meshview 6、panel 5、pki 11、registry 24、store 24）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check` 与 Node DOM 桩冒烟（冻结/解冻四拍）通过。对用户可见的行为变化：① 数十万条目的单个会话目录不再引发无界读盘——遍历读取量被条目配额与 8s 预算双重封顶，触顶/超时如实报不完整（collect_errors/缺席），清单与活跃度上报口径不变；② 面板在 console 不可达超 90s 后，任务卡停走并标注 snapshot stale、运行仪表归零，恢复后自动回活；③ MCP `list_agent_tasks` 返回结构新增 `tasks_truncated` 键（恒在，截断节点并集），工具描述同步。M1b-c 已验收项（SPEC §5 清单、R27 前核销项）无回退——既有 171 用例全部保持通过，SPEC 点名用例（三态/migration v4+v5/六工具/解析 etime）原样绿。

## R31 · codex 复审 m1b-c-fix2 + 裁决：3 轮收敛收口（2026-10-08 17:05，值班员执行）

**结论：3 轮收敛收口，实质通过（带候选残留）。** codex 复审 m1b-c-fix2（review-m1b-c-fix2.out，65,910 tokens）判**修改后通过**：R29 #1 阻塞（超大单目录无界读盘）**核销关闭**——分批读取、批间 ctx 检查、条目配额全部确认生效；无新增阻塞；残余 2 条建议。验证独立复核全绿（174 用例、vet/gofmt、JS 语法、三平台双程序编译），全程未修改仓库文件。SPEC 主链路符合；云机上报/人眼验收沿用 R27 非阻塞口径。

**护栏触发**：m1b-c 修复轮已累计 2 轮（m1b-c-fix、m1b-c-fix2），本次复审为第 3 轮决策点。按值班制度收敛护栏：**不派 fix3，残余意见全部转候选，收口锁定**。

### 残余意见裁决（全部转候选，不阻塞收口）

| # | 意见 | 级别 | 裁决 | 候选口径 |
|---|------|------|------|----------|
| 1 | `app.js:627/635` 冻结仅由 catch 触发：fetch 挂起（既不成功也不失败）时超 90s 仍继续计时，且轮询请求叠加（codex 复现 120s 未冻结） | 建议 | 转候选 | 独立于 catch 的新鲜度检查（每次 tick 校验 lastGoodAt）+ fetch 超时（AbortController）+ 轮询防重入。属 R29 #2 的挂起路径残余，非新问题 |
| 2 | `mcpserver.go:144/154` list_agent_tasks 任务清单与截断标记分两次读取，并发心跳下可出现旧任务快照配新截断标记 | 建议 | 转候选 | 任务与标记同一读事务取得。属数据一致性打磨，窗口极窄、无功能破坏 |

**收口理由**：① codex 判「修改后通过」，R29 全部阻塞清零，残余仅 2 条边界场景建议（fetch 挂起、并发读窗口），不影响主功能正确性；② 3 轮收敛护栏制度要求到此为止，避免建议级意见无限循环消耗修复预算；③ 174 用例全绿 + 三平台编译通过 + 主链路符合 SPEC，质量已达提交线。残余意见转入 M1b-c2/M1d 候选清单，随后续批次处理。

**COMMIT_OK 授权**：本裁决 touch COMMIT_OK，worker 执行本地 git commit（无 push）。理由：多设备开发约定「未提交的本地代码等于丢失」，收口即锁定成果；commit 为本地操作、git 可回退，若伦哥对收口有异议可 git reset 回退。R27-R31 全程无 push，远端推送仍等伦哥指令。

## R32 · zcode 交付轮 m1b-c2（2026-10-08 完成）

**SPEC-M1b-c2 v4 定稿全条款实现完毕**（§0 DESIGN 同步、§1 账号体系、§2 公网 MCP、§3 中文化、§4 公网门槛与数据权限、§5 来源收敛、§6 SAN 校验、§7 listen/并发闸、§8 runbook 入 DELIVERY）。验证口径：`go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **202 用例全绿**（基线 174 + 本轮新增/改造；13 包全 ok）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过、`node --check` 面板 JS 过、二进制级 E2E 验收矩阵 16 项实过（见下）。未执行 git commit。

### 实现说明（按 SPEC 条款）

| 条款 | 落点 |
|---|---|
| §1 migration | `internal/store/store.go` migration **v6**（users/sessions/api_tokens；token_hash 主键/唯一索引）。**序号偏差声明：SPEC 原文「migration v5」与 R27-#4 已占用的 v5（nodes.tasks_truncated）冲突，按既有序号顺延为 v6，表结构以 SPEC 为准** |
| §1 CLI | `cmd/console/usercmd.go`：`user add/passwd/disable/enable`（交互口令两遍确认，x/term ReadPassword，无明文参数路径）+ `token create/list/revoke`（明文仅显示一次）。CLI 先跑 migration 再操作（`store.Open` 内置）；stdio MCP 仍 `OpenReadOnly` 不迁移 |
| §1 认证常开 | 认证无开关、所有网络形态生效：`internal/auth`（Manager）+ `internal/panel` 会话守卫——未登录一律 302 /login（含 /api/panel/*）；豁免显式：GET /login、POST /login（登录动作，SPEC 豁免清单的 login 资源含其动作面，受登录防护约束）、静态资源（/glass.css、/app.js）、GET /healthz |
| §1 会话 | 256bit token（服务端只存 SHA-256）；Cookie `mc_session` Path=/、host-only 不设 Domain、HttpOnly、Secure、SameSite=Lax；滑动续期节流 ≤1 次/小时/会话；**绝对期限 30 天**（created_at 基点，续期不越过）；改密/disable 单事务撤销该用户全部 session+api_tokens（`store.ChangeUserPassword/SetUserEnabled`）；disable 最后一个启用用户拒绝（`ErrLastEnabledUser`） |
| §1 登录防护 | per-IP 5 次失败/分 → 429（滑动窗，IP 条目 cap 4096，触顶淘汰最旧）；**全局 bcrypt 并发 ≤2**（闸满排队 ≤32 人、等待 ≤5s，超限 429）；未知用户名执行 dummy bcrypt（同成本时序填充，惰性生成一次） |
| §2 MCP HTTP | **选型：官方 go-sdk v1.8.0（go.mod 锁定，本就为 M1b-b 依赖，本次无版本变更）**。`StreamableHTTPOptions{Stateless: true, JSONResponse: true}` 即 SPEC 指定的 JSON 同步响应模式（仅 POST、无 SSE 流、GET/DELETE 405），与 WriteTimeout 30s 无冲突——手写回退路径不需要。**兼容客户端声明：实现 MCP Streamable HTTP 传输（2025-06-18 及之后规范）的任意客户端**（HanaAgent MCP connector、Claude 等标准 MCP 客户端）；body 上限 1MB（SDK MaxRequestBodyBytes 强制）。Bearer api_token 哈希查表 + 应用层 constant-time 复核 + 用户启用态 + 到期校验（401 中文化、不泄露工具列表）。**MCP 独立 semaphore 4**、超出 429 不排队，与 overview 的 8、agent API 的 64 三闸独立 |
| §3 中文化 | 面板 UI 全量（导航/仪表/空态/任务卡/终端卡/登录页/页脚，`index.html`/`login.html`/`app.js`；状态值翻译只在渲染层，API/DB 值保持英文机器契约）；用户可见 API 错误（registry 全部错误体、面板/登录/MCP 错误体，agent 仅按状态码分支不解析文本、已核实无兼容影响）；MCP description/error 本就中文（M1b-b），HTTP 层错误中文化。代码注释与结构化日志键保持英文 |
| §4 公网门槛 | `cmd/console/main.go publicModeGate`：实际绑定地址（listener 解析结果）非 {127/8, ::1}（`net.IP.IsLoopback` 内建 IPv4-mapped 规范化）→ 公网模式，逐项检查（≥1 启用用户 / config 普通文件+0600+属主 `config.CheckConfigFileSecurity`（Windows 仅普通文件，ACL 等价约束如 requireKeyPerms 先例）/ Host 白名单非空），任一失败拒绝启动且错误指明原因与 remediation。**数据文件权限：`store.Open` data 目录 0700（MkdirAll 后显式 Chmod 收紧）、DB/WAL/SHM 0600（WAL/SHM 由 SQLite 沿用主库权限位，既有旧文件就地收紧）** |
| §5 来源收敛 | `registry.sourceFilter`：RemoteAddr ∈ 允许网段放行、公网 403（中文化错误体）；缺省={127/8, ::1, 10/8, 172.16/12, 192.168/16, 100.64/10}（IPv4-mapped 先 To4 规范化）；`agent_allowed_cidrs` 覆写（nil=缺省；显式空=仅回环）；token 校验独立保留；不读任何转发头 |
| §6 SAN | `pki.NormalizeSANs`：IP 字面量 → IP SAN（v4/v6）、合法 DNS → DNS SAN（小写去尾点）、URL/端口/CIDR/空值/非法字符**显式报错**（修复旧 ExtraIPs 静默跳过）、规范化去重保序；`Ensure` 在任何 PKI 写入（含 MkdirAll）之前统一校验；`Config.ExtraDNS/ExtraIPs` 移除（本就无调用方），替换为 `ExtraSANs []string`；追加/变更 SAN 自动判重签。部署值 `tls_extra_sans: ["1.13.158.180"]` 样例入配置注释 |
| §7 listen/并发 | overview semaphore 8（`panel.limitConcurrent`）；MCP semaphore 4（上条）；listen 公网部署值 0.0.0.0:7700 入 runbook（回环缺省不变）；no-store/nosniff 全链（面板 guard、healthz、MCP——SDK 自写 `Cache-Control: no-cache, no-transform`，`noStoreWriter` 在 WriteHeader 拦截点覆盖为 no-store 保证字面口径） |
| §8 runbook | DELIVERY.md M1b-c2 节（部署步骤 + 客户端更新顺序 + 验收矩阵 + 回滚） |

### 裁决遵循与实施要点

| # | 事项 | 口径 |
|---|---|---|
| 1 | go-sdk v1.8.0 能力核验 | SDK 该版 `StreamableHTTPOptions` 已含 `Stateless`（SEP-2567 sessionless 方向）与 `JSONResponse`（§2.1.5 JSON 模式）——SPEC 双路径（SDK 优先/手写回退）取 SDK 路径即得 JSON 同步响应模式，无 SSE、无会话表 |
| 2 | 新依赖 | `golang.org/x/crypto v0.57.0`（bcrypt cost 10）+ `golang.org/x/term v0.46.0`（交互口令），x/net v0.59.0 等既有依赖未动 |
| 3 | E2E 发现并修复 ① | `normalizeHostList` 原实现把裸 IPv6 条目 `::1` 误判非法（url.Parse 无法解析多冒号 authority）——改为 IP 字面量 ParseIP 直通（方括号先剥离、ip.String() 规范化），单测+样例配置双覆盖 |
| 4 | E2E 发现并修复 ② | SDK 覆写 Cache-Control（见 §7 行）——`noStoreWriter` 拦截修复，成功路径断言进 http_test |
| 5 | 验收「错密码 429」口径 | 前 5 次失败 401（统一文案「用户名或密码错误」，未知用户/错密码/禁用不区分防枚举），第 6 次 429；限流先于凭据校验（窗口内正确密码同样 429） |
| 6 | stdio MCP 不变 | 仍 `OpenReadOnly`（mode=ro + query_only=1），不迁移、零写路径；六只读工具复用 |

### 验证记录（m1b-c2）

- `go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` **202 用例全绿**（cmd/console 6、agent 14、collect 35、agentdisc 9、config 23、headscale 12、mcpserver 9、meshview 6、panel 9、pki 13、registry 27、store 31、auth 8；13 包全 ok）。
- `make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`make build` 版本 `m1b-c2+git_<hash>`。
- `node --check` 面板 JS 过。
- **二进制级 E2E**（本机 /tmp/mc-e2e，端口 17801，TLS + 真实 CA 链验证、未用 -k）：匿名 / 与 /api/panel/overview 302→/login 且无业务数据泄露；登录页中文；healthz ok；静态匿名 200；MCP 无 token 401 中文化不泄露工具名；错密码 ×5 → 401、第 6 次 **429**；登录成功 Set-Cookie（HttpOnly/Secure/Path=/）→ 面板中文（运行中的 agent 任务/受管服务/登出）→ overview 200 含真实节点与任务；登出后会话即失效；MCP 带 token initialize/tools/list（6 工具全中文 description）/tools/call list_agent_tasks 返回真实心跳数据；agent 注册（一次性 token 用后即废：二次注册 401）+ 心跳 + 跨节点 403「节点身份不匹配」；数据文件 data 0700 / DB/WAL/SHM 0600 实测；服务日志仅 WARN/INFO 结构化键英文、无凭据材料。
- 公网形态门槛未在本机实跑公网绑定（沙箱无公网地址）——判定与检查逻辑由 `TestPublicModeGate`（0.0.0.0 addr × 无用户/0644 config/缺失 config/全过四态）钉住；上机验证列 DELIVERY 部署上机项。

## R33 · codex 审查 m1b-c2 + 裁决：派修复轮（2026-10-08 18:20，值班员执行）

**结论：不通过 → 派 m1b-c2-fix（本任务修复轮 1）。** codex 审查 m1b-c2（review-m1b-c2.out）判不通过：2 条阻塞 + 3 条新建议 + 2 条 R31 候选沿用。独立验证面全绿（202 用例、vet/gofmt、三平台、JS 语法），codex 全程未修改仓库文件。m1b-c2 为新交付任务首轮审查，修复轮计数 0→1，未触 3 轮护栏；两条阻塞均首次出现，未触「同条修两次不过」升级。

### 裁决（Hana，值班员自主）

| # | 级别 | 意见 | 裁决 | 口径 |
|---|------|------|------|------|
| 1 | 阻塞 | auth.go:231,262 + store/auth.go:224 会话签发竞态——登录读旧哈希后改密事务先完成撤销，登录仍插入有效会话，旧密码绕过撤销 | 采纳修复 | 签发事务内复核密码版本与启用态，不一致回滚；并发语义用测试钉死。安全类阻塞零容忍 |
| 2 | 阻塞 | config.go:490 Windows 直接放行未查 ACL/属主，违反 SPEC §4；部署提醒不能替代启动门槛 | 采纳修复 | Windows 实装 ACL/属主检查，不可检查即按失败拒绝启动（公网形态）。§4 是公网门槛启动闸门，必须真实执行 |
| 3 | 建议 | mcpserver/http.go:74 SDK 错误英文原样输出（unknown tool 等），§2/§3 中文化未完成 | 采纳修复 | 错误类型映射中文错误体，不泄露内部细节与工具清单 |
| 4 | 建议 | main.go:319 + registry.go:166 安全头未覆盖 agent API 与 mux 默认错误响应，§7「全链」未满足 | 采纳修复 | 全局 ResponseWriter 包装覆盖错误路径，agent 兼容性不回退 |
| 5 | 建议 | usercmd.go:208 忽略 flag 解析错误，-expires 缺值/拼错仍签发永久 token | 采纳修复 | 解析失败报错退出，不签发 |
| 6 | 建议·R31 候选 | app.js:638,646,654 fetch 挂起不冻结、轮询叠加 | 采纳修复（核销候选） | R31 裁决口径原样执行：lastGoodAt 新鲜度检查 + AbortController 超时 + 轮询防重入。R31 已预告去向 M1b-c2 批次，本轮顺带核销，终止沿用噪声 |
| 7 | 建议·R31 候选 | mcpserver.go:144,154 任务清单与截断标记分次读取，快照错配 | 采纳修复（核销候选） | 同一读事务取得，R31 口径。同 #6 一并核销 |

**修复轮范围**：7 条全部 → 派 m1b-c2-fix（值班员 nohup pipeline-run.sh 直接派发，不占 NEXT-TASK 队列）。验证口径不变：vet/gofmt/test 全绿 + cross 三平台 + node --check，m1b-c2 已验收面（E2E 矩阵、SPEC 已核销条款）不回退。若修复后复审仍有建议级残余，按既有口径转候选或收口，阻塞级零容忍。

## R34 · zcode 修复轮 m1b-c2-fix（2026-10-08 完成）

**R33 裁决 7 条全部按口径修完**（2 阻塞 + 3 建议 + 2 条 R31 候选顺带核销）。修复后 `go vet ./...` 零输出、`gofmt -l` 无文件（1.26.0/1.26.8 两版工具链 gofmt 结论一致）、`go test -count=1 ./...` **211 用例全绿**（修复前基线 202 + 本轮新增 9：cmd/console 2、auth 2、config 3、mcpserver 1、meshview 1；13 包全 ok）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过（windows/amd64 即新 ACL 代码的编译验证）、`node --check` 面板 JS 过 + Node DOM 桩冒烟（防重入/超时 abort/冻结/恢复四段全断言，连跑 5 遍稳定）通过、二进制级 E2E 冒烟（登录→overview→改密撤销→重登、agent 注册/心跳、MCP 未知工具中文/六工具/list_agent_tasks、404 与 401 拒绝路径安全头实测携带）通过。未执行 git commit。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | 阻塞：会话签发竞态绕过改密撤销。方案按裁决示例落 **password_version**：`internal/store/store.go:371` migration **v7**（`users.password_version INTEGER NOT NULL DEFAULT 1`，序号顺延声明与 v6 同口径）；`internal/store/auth.go:179` ChangeUserPassword 与 `:206` SetUserEnabled（仅禁用向）同事务递增版本；**`:246` CreateSession 重写为单条条件插入**——`INSERT INTO sessions … SELECT … FROM users WHERE id=? AND password_version=? AND enabled=1`，0 行受影响即 `ErrSessionIssuanceDenied`（`:30` 新增）：条件求值与会话插入同为一条语句原子完成，改密/禁用事务在「凭据校验后、签发前」提交必然使版本/启用态失配，旧凭据签不出活会话（单写连接下写事务天然串行，无 read-then-write 升级窗口）。`internal/auth/auth.go:274-283` Login 传入 `u.PasswordVersion` 并把该错误并入统一 401 文案（记 Warn 留痕）；`:97-99/:264-267` `afterValidate` 测试钩子（凭据校验后、签发前注入，生产恒 nil，一个 nil 判断开销）供交错单测确定性复现「事务间隙」。测试钉死并发语义：`auth_test.go:265` TestLoginRacePasswordChange（钩子里抢先改密 → 登录必须 ErrInvalidCredentials+空 token+零会话行，新密码照常登录拿活会话）、`:305` TestLoginRaceDisable（钩子里抢先禁用 → 同口径拒绝）；store 层 `TestChangePasswordRevokesAllCredentials`/`TestDisableRevokesAndLastEnabledProtected` 扩断言版本递增、旧版本拒签、禁用态持最新版本仍拒签（enabled 条件独立生效）、重新启用后恢复签发 |
| 2 | 阻塞：Windows config 安全检查直接放行。`internal/config/config.go:484` CheckConfigFileSecurity 重构为「普通文件 + 平台钩子」；**`config_owner_windows.go:18` checkConfigPlatformSecurity 实装**（golang.org/x/sys/windows v0.48.0 转直接依赖，go.mod 更新）：`GetNamedSecurityInfo(OWNER|DACL)` 取安全描述符——属主须为当前进程用户或 BUILTIN\Administrators；DACL 中每条生效（非 INHERIT_ONLY）的 ACCESS_ALLOWED_ACE trustee 须 ∈ {当前用户, SYSTEM, Administrators}；**无法取得安全描述符/属主/DACL（含 NULL DACL=人人可访问）、读 ACE 失败一律按检查失败返回错误**，公网形态拒绝启动且错误带 `icacls … /inheritance:r` remediation。判定核心抽为平台无关纯函数 `config.go:500` aclVerdict（ownerSelfOK + allowTrustees + trusted 集合），单测 `config_acl_test.go` 三用例（放行形态×5、属主不合规、Everyone/Users/陌生 SID trustee 拒绝+remediation 关键词）在任意平台覆盖错误路径；macOS/Linux 口径不变（`config_owner_unix.go` 仅重组不动语义，0600/属主报错文案逐字保留）。Windows 真机核验列部署上机项（判定函数已测、薄封装交叉编译过） |
| 3 | 建议：MCP HTTP SDK 错误中文化。SDK 未提供错误定制钩子（核查 v1.8.0 ServerOptions/StreamableHTTPOptions），在 HTTP 层拦响应：`internal/mcpserver/http.go:101` zhErrorWriter 缓冲 SDK 响应（cap 1MB，超限/流式 Flush 即放弃翻译原样透出），`:174` translateSDKBody 按 Content-Type 分派——application/json 解 JSON-RPC 信封，**仅替换 error.message，code/id/data 原样保留**（协议层兼容）；`:220` translateSDKErrorMessage 闭合集类型映射：`unknown tool`→「未知工具：请求的工具不存在（可用 tools/list 获取清单）」（不回显请求名、不泄露清单）、ParseError/InvalidRequest/MethodNotFound/InvalidParams 按码映射、unsupported protocol version/会话未初始化/重复请求 ID 按签名映射、未知错误统一「请求处理失败」；text/plain 覆盖 SDK `http.Error` 固定文案（Method Not Allowed/JSON RPC not handled/malformed payload/body exceeds/invalid Host 等 10 形态）。安全头链不变（noStoreWriter 仍在链上，落笔点后移语义不变）。测试 `http_test.go:174` TestMCPSDKErrorChinese：未知工具中文且无 SDK 原文/无工具名泄漏/code 保留 -32602、未知方法中文、非法帧中文、GET 405 状态与文案中文；二进制 E2E 实测 unknown tool 中文 |
| 4 | 建议：安全头覆盖缺口。`cmd/console/main.go:326` root handler 以 `secureHeaderWriter`（`:459`）包装 mux 出口——WriteHeader 拦截点强制写入 `Cache-Control: no-store` + `X-Content-Type-Options: nosniff`（覆写语义，与既有各 handler 自设头一致），隐式 200（直接 Write）与 Flush 透传同覆盖；mux 默认 404/405、agent API 全部拒绝路径（401/403/413/503）一并命中，无遗漏路径；agent 仅按状态码分支不解析响应头（SPEC §3 已核实），兼容性不回退。测试 `main_m1bc2_test.go:105` TestSecureHeaderWriter（显式 403/隐式 200/mux 404/mux 405 四态断言）；二进制 E2E 实测 404 与 heartbeat 401 路径两响应头在场 |
| 5 | 建议：usercmd 忽略 flag 解析错误。`cmd/console/usercmd.go:210` `token create` 的 `fs.Parse` 返回值接住——解析失败打印「未签发任何 token」并 exit 2（flag 包已回显具体解析错误）；`-expires` 缺值/未知 flag 不再滑向「无到期=长期有效」的签发。`-expires` 值本身的格式/过去时刻校验原本就在签发前报错退出（既有路径复核保持）。测试 `main_m1bc2_test.go:219` TestTokenCreateFlagParseError：缺值/未知 flag/尾随 flag 三形态 exit 2 且**库中零 token**（解析失败路径不得落任何凭据）+ 合法 -expires 对照正常签发 |
| 6 | 建议·R31 候选核销：面板 fetch 挂起不冻结、轮询叠加。`internal/panel/web/app.js` tick 重写（`:644`），R31 裁决三件套全落：① **新鲜度独立校验**——每次 tick 先行校验 `Date.now()-lastGoodAt > FETCH_STALE_MS(90s)` 即 `renderStaleSnapshot()`，不依赖 catch（fetch 挂起/慢响应照冻）；`staleShown` 去重标记（`:631`）保证一轮失联只渲一次，避免「冻结行/crit 行」逐拍互斥闪烁，crit 文案由 catch 维持；② **fetch 超时**——`FETCH_TIMEOUT_MS=10s`（`:625`）AbortController 强制收敛，挂起请求至多占用一拍，`AbortError` 显示「请求超时」；③ **轮询防重入**——`tickInflight` 标记（`:628`）前序未返回不叠发（与超时配合不死锁）。Node DOM 桩冒烟四段断言（挂起窗口内不叠发→超时 abort→过线冻结 crit→恢复续轮询）连跑 5 遍稳定；`node --check` 过 |
| 7 | 建议·R31 候选核销：list_agent_tasks 快照错配。store 层新增 `internal/store/store.go:1178` `ListAgentTaskSnapshot`——**单只读事务**（read 池 BeginTx，WAL 快照）一次取 `agent_tasks` 全表与 `nodes` 状态表（status/tasks_truncated 同批）；扫描器抽 `scanAgentTaskRowsFrom`/`scanNodeRecordsFrom`（`queryContext` 接口统一 *sql.DB/*sql.Tx）。meshview 层 `internal/meshview/meshview.go:472` 新增 `AgentTasksSnapshot(ctx, node) (tasks, truncated, error)`：名称/状态 join、stale 标记、截断并集（**按过滤后涉及节点计**，沿用 R29-#3 口径——被截断节点零任务时标记仍 true）全部出自同一快照；`AgentTasks` 变为其薄封装（overview 同源受益，零新 SQL）。`internal/mcpserver/mcpserver.go:147` handler 删除第二次 `q.Nodes` 读取改调快照方法。测试 `meshview_m1bc_test.go:110` TestAgentTasksSnapshotConsistency（全网/按节点/未知节点/零任务截断节点四态 + AgentTasks 包装回归）；既有 `TestCallListAgentTasks(Truncated)` HTTP/stdio 两侧语义原样绿 |

**验证记录（m1b-c2-fix）**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（**211 用例**：cmd/console 8、agent 14、collect 35、agentdisc 9、auth 10、config 26、headscale 12、mcpserver 10、meshview 7、panel 9、pki 13、registry 27、store 31）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check` 与 DOM 桩冒烟通过。二进制级 E2E 冒烟（/tmp 临时实例，已清理）：表单登录 302 → overview 200；`/no/such/path` 404 与 heartbeat 无 token 401 均带 no-store/nosniff；MCP 无 token 401 中文、unknown tool 返回「未知工具」中文且 code=-32602 保留、tools/list 六工具齐备、list_agent_tasks 走新快照路径返回 `{"tasks":[],"tasks_truncated":false}` 契约形态；agent 注册（一次性 token）+ 心跳 200 兼容不回退；`user passwd` 后旧会话 302、旧 MCP token 401、新密码重登 200（改密撤销链 + 版本复核放行正常凭据）。对用户可见的行为变化：① 「改密/禁用的瞬间正在进行的登录」不再可能拿到活会话（此前存在竞态窗口）；② Windows 公网形态 config ACL/属主检查真实执行（此前仅普通文件校验即放行），取不到 ACL 即拒绝启动；③ MCP SDK 层英文错误（unknown tool 等）改为中文错误体（code 保留）；④ 全部 404/405/agent 拒绝路径响应携带 no-store/nosniff；⑤ `meshconsole token create` 参数拼错/缺值即退出码 2 且不签发；⑥ 面板 fetch 挂起 10s 后超时重试、超 90s 冻结、不再叠加轮询请求；⑦ migration v7 自动执行（users 增 password_version 列）。

## R35 · codex 复审 m1b-c2-fix + 裁决（2026-10-08 19:24，值班员执行）

**结论：不通过（3 条残余），全部采纳修复 → 派 m1b-c2-fix2。** codex 复审 m1b-c2-fix（review-m1b-c2-fix.out，47,823 tokens）判**不通过**：R33 七项核销 4 关 3 缺——阻塞 #1 会话竞态核销关闭（password_version 条件插入 + 交错测试确认），建议 #4 安全头、#6 轮询挂起、#7 快照错配均核销关闭；残余 3 条：阻塞 #2 Windows ACL 跳过非普通允许型 ACE（callback/object ACE 可携带不可信 trustee 仍过启动检查）、建议 #3 zhErrorWriter 未覆盖未匹配错误/空 POST/Last-Event-ID POST 路径、建议 #5 token create 未拒绝多余位置参数（`token create lunge extra -expires <未来>` 在 extra 停止解析仍签长期 token）。验证独立复核全绿（go test -count=1 全绿、vet、gofmt、JS 语法），全程未修改仓库文件。值班员逐条抽验源码后裁决如下。

### 裁决（Hana，值班员自主）

| # | 级别 | 意见 | 裁决 | 口径 |
|---|------|------|------|------|
| 1 | 阻塞 | config_owner_windows.go:56 跳过所有非普通允许型 ACE（callback/object 允许型 ACE 同样可授访问权），不可信 trustee 藏于此类 ACE 即绕过启动检查 | 采纳修复（第 2 次修复） | 安全类阻塞零容忍（R33 口径不变）：callback/object 允许型 ACE 的 trustee 须正确解析并纳入可信集判定；无法判定/解析失败的 ACE 类型一律按检查失败拒绝启动（公网门槛安全侧默认拒绝，不允许静默放行）。**返工计数：R33-#2 首修未关闭，本轮第 2 次修复，复审仍不过即触发「同条修两次不过」上限请示伦哥** |
| 2 | 建议 | http.go:215 未匹配错误原样透出；空 POST 返回 `POST requires a non-empty body`、带 Last-Event-ID 的 POST 返回英文，SPEC §2/§3 未闭合 | 采纳修复 | 延续 R34-#3 闭合集映射口径：zhErrorWriter 覆盖面补齐——未匹配错误、空 POST、Last-Event-ID POST 三条路径全部中文化，JSON-RPC code/协议语义不变，不泄露内部细节 |
| 3 | 建议 | usercmd.go:210 未检查 fs.NArg()：多余位置参数使解析提前停止，后续 -expires 被忽略仍签发长期 token | 采纳修复 | 解析后校验剩余位置参数（fs.NArg() == 期望值），多余即报错退出 2、零签发——与 R34-#5「解析失败路径不得落任何凭据」口径一致 |

**返工计数**：#1 第 2 次修复（首修未关闭，未触上限——该条件指修两次仍不过）；#2/#3 均第 2 次修复（R34 首修部分关闭）。修复轮计数：m1b-c2-fix 第 1 轮完成、m1b-c2-fix2 第 2 轮，未触 3 轮收敛护栏（第 3 轮无论意见多少强制收口转候选）。

**修复轮范围**：本表 3 条全修 → 派 m1b-c2-fix2（值班员 nohup pipeline-run.sh 直接派发，不占 NEXT-TASK 队列）。验证口径不变：vet/gofmt/test 全绿 + cross 三平台 + node --check，m1b-c2-fix 已验收面（R34 七项已核销部分、E2E 矩阵）不回退。COMMIT_OK 不授（复审未通过）。禁止 git commit。

## R36 · zcode 修复轮 m1b-c2-fix2（2026-10-08 完成）

**R35 裁决 3 条（1 阻塞 + 2 建议）全部按口径修完**。修复后 `go vet ./...` 零输出、`gofmt -l` 无文件、`go test -count=1 ./...` **218 用例全绿**（m1b-c2-fix 基线 211 + 本轮新增 7：config 5、mcpserver 1、cmd/console 1；13 包全 ok）、`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过（windows/amd64 即新 ACE 解析代码的编译验证）、`node --check` 面板 JS 过。二进制级冒烟：CLI `token create lunge extra -expires <未来>` 实测 exit 2 且零签发、合法形态照常签发；真实 TLS 实例上空 POST → 400「请求体不能为空」、带 Last-Event-ID 的 POST → 400 中文提示，SDK 原文均不在场；回归面 tools/list 六工具中文 description、unknown tool 中文且 code=-32602 保留、404 携带 no-store/nosniff 均不回退。未执行 git commit。

| # | 修复说明（文件:行号 · 怎么修） |
|---|---|
| 1 | 阻塞（第 2 次修复）：Windows ACL 跳过非普通允许型 ACE。**「跳过」语义彻底清除**（R35-#1）：新增平台无关判定层 `internal/config/config_acl.go`——`classifyDACLACE`（`:61`）按 winnt.h 字节布局分类单条 DACL ACE：普通(0x00)/callback(0x09)/object(0x05)/callback-object(0x0B) 四类允许型全部解析 trustee（callback 型与普通型同布局仅 SID 后多回调数据；object 型按 Flags 位各 16 字节 GUID 推 SidStart 偏移），reject 型(0x01/0x06/0x0A/0x0C)归 `aceDenyIgnore`——**拒绝型只收权不授权、不构成「不可信账户获权」通道，故不参与放行判定（注释明示，判据仍是 aclVerdict 的「每条生效允许型 trustee ∈ 可信集」）**；INHERIT_ONLY（不作用于本对象）与类型无关跳过；未知类型（系统 SACL 类/保留/未来型）、尺寸撒谎（<8 / 越给定字节 / object 型缺 Flags 字段）、SID 区间越界（头越界 / SubAuthorityCount 谎报）一律 `aceUnparsableFail`——安全侧默认拒绝，无「看不懂就跳过」路径。`daclAllowTrustees`（`:117`）整链枚举，任一 ACE 无法判定即报错带下标。Windows 薄封装 `config_owner_windows.go:53` 起重写：GetAce 枚举 → 原始字节切片（AceSize 下限前置校验）→ daclAllowTrustees → SID.IsValid 校验后字符串化 → 既有 aclVerdict；无法判定条目的错误文案含「callback/object 等允许型 ACE 同样可授予访问权，按检查失败处理」+ icacls remediation。测试 `config_acl_test.go` 新增 5 用例（平台无关）：四类允许型 ×（0–2 GUID 形态 × 有/无回调数据）trustee 解析一致、**callback/object 允许型藏 Everyone 必被 aclVerdict 拒绝（阻塞场景纯函数层钉死）**、四类拒绝型归 ignore、INHERIT_ONLY 各型跳过、未知类型与五类解析失败全 fail、整链混排（可信普通+拒绝型+callback 允许+INHERIT_ONLY+callback-object）trustee 集正确且混入未知类型报错带下标；Windows 真机核验仍列部署上机项 |
| 2 | 建议（第 2 次修复）：MCP 错误中文化覆盖缺口。`internal/mcpserver/http.go` `translateSDKBody`（`:176`）text/plain 闭合集补齐 + 收口（R35-#2）：新增空 POST `POST requires a non-empty body`→「请求体不能为空」、Last-Event-ID POST `can't send Last-Event-ID for POST request`→「POST 请求不支持 Last-Event-ID（该头仅用于 GET 事件流续传）」两条复审点名映射；**未匹配路由/方法及一切未收录文案不再原样透出**——text/plain 兜底统一「请求处理失败」（SDK 的 text/plain 仅出自 http.Error 错误路径，无成功体经此；与 JSON 路径 translateSDKErrorMessage 默认分支同口径），状态码由 zhErrorWriter 原样保留。三条路径测试 `http_test.go:230` TestMCPHTTPErrorPathsChinese：空 POST 与 Last-Event-ID POST 各断言（状态 400 不变、中文在场、SDK 原文不在场）+ 兜底通用中文断言 + 成功响应不被改写 |
| 3 | 建议（第 2 次修复）：多余位置参数未拒绝。`cmd/console/usercmd.go`（R35-#3）：`token create`（`:206`，NArg 复核 `:227`）改在 stripConfigFlag 后的 rest 上取用户名与解析 flags，`fs.Parse` 成功后**复核 `fs.NArg()!=0` 即报「未签发任何 token」退出 2**——flag 包遇首个位置参数即停，`token create <用户名> extra -expires <未来>` 此前把 -expires 连同 extra 静默忽略后照签长期 token（R33-#5 只挡了显式解析失败这一半）；`user add/passwd/disable/enable`（`:80`）同样以 rest 复核「仅 <用户名> 一个位置参数」，多余即退出 2 且在打开库之前（零副作用、口令读取不触发）；`token list`（`:276`）不接受位置参数、`token revoke`（`:301`）仅 <id> 一个，多余均退出 2。测试 `main_m1bc2_test.go:261` TestExtraPositionalArgsRejected：`token create` 多余位置参数 × 有/无 -expires 两形态均 exit 2 且**库中零 token**、user add（断言口令读取不被触发）/user disable/token revoke/token list 四形态 exit 2、对照合法 `-expires` 签发正常且 expires_at 确实落库 |

**验证记录（m1b-c2-fix2）**：`go vet ./...` 零输出；`gofmt -l` 无文件；`go test -count=1 ./...` 全绿（**218 用例**：cmd/console 9、agent 14、collect 35、agentdisc 9、auth 10、config 31、headscale 12、mcpserver 11、meshview 7、panel 9、pki 13、registry 27、store 31）；`make cross` linux/amd64 + darwin/arm64 + windows/amd64 通过；`node --check` 过。二进制级冒烟（/tmp 临时实例，已清理）：`user add`（pty 交互）→ `token create lunge extra -expires 2027-06-30T23:59:59Z` exit 2 且 token list 为空 → 合法 `token create` 照常签发；TLS 实例（loopback + 真实 CA 链）空 POST → 400「请求体不能为空」、Last-Event-ID POST → 400「POST 请求不支持 Last-Event-ID…」，SDK 原文不在场；R34 已验收面回归不回退（tools/list 六工具中文、unknown tool 中文 + code -32602、404 带 no-store/nosniff）。对用户可见的行为变化：① Windows 公网形态 config 含 callback/object 允许型 ACE 时 trustee 照常纳入判定（此前跳过）；DACL 出现无法判定的 ACE 类型/损坏条目即拒绝启动（此前静默跳过）；② MCP 空 POST 与带 Last-Event-ID 的 POST 返回中文错误体（状态码不变），未收录英文错误文案不再透出；③ `token create`/`user`/`token list`/`token revoke` 收到多余位置参数即退出码 2，不产生任何变更。

## R37 · codex 复审 m1b-c2-fix2 + 裁决：3 轮收敛收口（2026-10-08 20:09，值班员执行）

**结论：3 轮收敛收口，实质通过（带 1 条候选残留）。** codex 复审 m1b-c2-fix2（review-m1b-c2-fix2.out）判**不通过**：R35 三项核销 2 关 1 残——阻塞 R35-#1（Windows ACL callback/object 允许型 ACE）**核销关闭**（classifyDACLACE 四类允许型全解析、不可判定即拒绝启动，纯函数测试钉死阻塞场景），建议 R35-#2（MCP 空 POST/Last-Event-ID POST 中文化）**核销关闭**；残余 1 条建议：R35-#3 的 usercmd `stripConfigFlag` **等号写法分支**把 `-config=...` 之后的全部参数静默丢弃（含 `-expires` 与多余位置参数），使 R36 新增的 NArg 校验被绕过——`token create lunge -config=/tmp/c.yaml extra -expires 2027-06-30T23:59:59Z` 被裁成 `create lunge`，仍签发长期 token。验证独立复核全绿（218 用例、vet/gofmt、三平台编译、JS 语法），codex 全程未修改仓库文件。

**护栏触发**：m1b-c2 修复轮已累计 2 轮（m1b-c2-fix、m1b-c2-fix2），本次复审为第 3 轮决策点。按值班制度收敛护栏：**不派 fix3，残余意见全部转候选，收口锁定**。叠加因素：R35-#3 已是第 2 次修复（R34 首修部分关闭、R36 第 2 次修仍未闭合），触发「同条意见修两次不过」升级条件——升级动作并入本次 notify（如实告知伦哥该意见两修未闭合与收口理由），不另派修复轮猜测意图。

### 残余意见裁决（转候选，不阻塞收口）

| # | 意见 | 级别 | 裁决 | 候选口径 |
|---|------|------|------|----------|
| 1 | `usercmd.go` stripConfigFlag 等号写法（`-config=值`）分支丢弃该参数之后的全部 argv，`-expires` 被吞仍签长期 token、多余位置参数逃过 NArg 校验 | 建议 | 转候选 | stripConfigFlag 仅剥离 config 参数本身、保留其余后缀（等号后值不与其他参数合并丢弃），补等号写法测试（含 `-config=x extra -expires <未来>` 拒绝、`-config=x -expires <未来>` 正常签发两形态）。属 CLI 参数解析打磨：默认空格写法路径已正确校验（R36 测试过），触发需「等号写法 + 敏感参数在后」组合，非主链路缺陷 |

**收口理由**：① R35 三项中唯一阻塞已彻底关闭（第 2 次修复通过），唯一残余为 CLI 边角建议，触发条件组合苛刻，不影响安全主链路（config ACL 检查、会话竞态、MCP 错误面均已闭合）与主功能正确性；② 3 轮收敛护栏制度要求到此为止，避免建议级意见无限循环消耗修复预算；③ 218 用例全绿 + 三平台编译 + 二进制冒烟（R36 记录）质量已达提交线，与 R31 收口时质量面同构。

**COMMIT_OK 授权**：本裁决 touch COMMIT_OK，worker 执行本地 git commit（无 push）。理由同 R31 口径：多设备开发约定「未提交的本地代码等于丢失」，收口即锁定成果；commit 为本地操作、git 可回退，若伦哥对收口有异议可 git reset 回退。全程无 push，远端推送仍等伦哥指令。候选残留随后续批次（M1b-c3/M1d）处理。

