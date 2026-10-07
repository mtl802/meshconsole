# MeshConsole M1 骨架 · 第一批开发任务书（SPEC-M1a）

> 依据 DESIGN.md v0.7（已通过 codex 七轮审查）。本批只做 M1 的核心数据链路，
> Web 面板/MCP/Headscale 集成在第二批（M1b）。

## 目标

跑通「agent 采集 → 控制台接收 → SQLite 存储」最小闭环，三平台可编译，认证从第一行代码就在。

## 范围（本批交付）

### 1. 项目骨架

- Go module `github.com/mtl802/meshconsole`，Go 版本 go.mod 声明 1.26
  （DESIGN §3「固定受支持的版本」；依赖 gopsutil v4.26.9 要求 go≥1.26，1.26 为当前受支持版本——R1 审查裁决，原 1.24 为示例值）
- 目录：`cmd/console/`、`cmd/agent/`、`internal/{registry,store,config}/`
- 配置加载：console 与 agent 各自 config.yaml（样例放 `deploy/`），字段见 DESIGN §4/§7
- 日志：结构化（slog），JSON 输出
- 构建：`make build` 本地产出两二进制；`make cross` 产出 linux/amd64、darwin/arm64、windows/amd64

### 2. 节点注册与心跳（internal/registry + cmd/agent）

- `POST /api/agent/register`：携带一次性注册 token（配置注入）+ 节点声明（name/role/os/arch），
  服务端校验后签发节点 token（随机 32B hex），存 SQLite；注册 token 用后即废（单次有效）
- `POST /api/agent/heartbeat`：Bearer 节点 token 认证；上报指标快照 + agent 版本；
  服务端刷新 last_seen；60s 无心跳标记 offline（后台 goroutine 巡检）
- agent 侧：15s 间隔采集上报；**采集失败的字段报 null 并附 error 说明，禁止填 0**
  （gopsutil 跨平台语义不一致）；CPU 采样异步进行，不阻塞心跳循环
- 断线恢复：agent 用持久化在本地的节点 token 续接，不重复注册；
  注册响应与 token 持久化在 agent 本地 state（JSON 文件，600 权限）
- 断线退避：上报失败按指数退避重试（1s→2s→…→60s 封顶，带抖动）

### 3. 指标采集（internal/agent/collect）

- gopsutil：cpu 百分比（0-100，interval 内采样）、mem（used/total）、disk（/ 或配置挂载点，used/total）、
  net（累计 rx/tx 字节）、uptime、load（linux/macOS；Windows 无 load 报 null）
- 上报 JSON 结构见 DESIGN §5 metrics 表字段

### 4. 存储（internal/store，modernc.org/sqlite 纯 Go）

- 打开参数：WAL、busy_timeout=5000ms、单写连接（SetMaxOpenConns(1) 写连接 +
  读连接池分离或串行写队列，按你判断实现但必须规避 SQLITE_BUSY）
- schema migration：内嵌 SQL + 版本表 `schema_migrations`，启动时增量执行
- M1 表：`nodes`、`metrics`（按 DESIGN §5，含 last_success 语义）；时间列建索引
- 保留策略：后台每小时清理 7 天前 metrics

### 5. 认证中间件（从第一行代码就在，不许后补）

- 所有 /api/agent/* 端点认证：register 验注册 token，其余验节点 token
- 节点 token 与 node 绑定：只能上报本节点数据（heartbeat body 的 node 须与 token 匹配）
- 认证失败统一 401，不泄露原因细节
- 节点 token 哈希存储（SHA-256），库中不存明文

### 6. 节点 CLI 与可观测

- console 启动加载 config.yaml（监听地址默认 127.0.0.1:7700，M1 不做 TLS——TLS 与证书管理在 M1b 统一上，
  但认证中间件必须就位）
- agent 子命令：`meshagent run`（前台运行）、`meshagent register -console <url> -token <注册token> -name <名称>`
- /healthz 端点：仅返回 ok，无认证（唯一例外）

## 明确不做（本批）

TLS/证书、Web 面板、MCP、Headscale 集成、探测引擎、告警、服务清单、agent 发现、
任何写操作（restart/invoke）、Docker 采集（M1b）。

## 验收标准（自测后写进交付说明）

1. `make cross` 三平台编译通过、无 vet 警告
2. 本机起 console + agent（macOS），注册→心跳→落库全链路通，SQLite 里能查到 nodes 与 metrics 行
3. 错误 token 访问 heartbeat 返回 401；A 节点 token 报 B 节点数据被拒
4. 杀 agent 60s 后 nodes.status 变 offline；重启 agent 复活为 online 且**不产生重复节点行**
5. 采集异常（如 Windows 无 load）字段为 null 非 0
6. 单元测试：store 层（migration/插入/清理）、registry 层（注册/认证/重复注册防护）覆盖核心路径
7. console 常驻内存打印实测值（RSS），目标 <80MB（记录实测数）

## 交付物

- 代码 + go.mod/sum + Makefile + deploy/console.example.yaml + deploy/agent.example.yaml
- DELIVERY.md：验收标准逐条自测结果 + 实测 RSS + 已知限制
