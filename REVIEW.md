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
