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

