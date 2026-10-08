# MeshConsole 组网控制台 · 详细设计

> 版本 v0.8（设计稿） · 2026-10-08
> v0.8 修订：§7-4 重写「公网面板例外」——三层认证（面板=账号会话 / MCP=Bearer
> token / agent=来源收敛+token）、登录防护与限流、并发三闸、公网启动门槛、
> 证书信任与 pin 更新流程、回滚路径；删除「公网无暴露端口」绝对表述（改为
> 「公网暴露须满足本节全部条件」）；§8 补账号/会话/token 运行条款（M1b-c2）。
> 版本 v0.7（设计稿，第六轮 codex 复审修订） · 2026-10-07
> v0.7 修订：幂等键释放改 archived_key 独立归档字段（消除 expired: 命名空间冲突）、
> L2 状态机移除 unknown 并明确结果不确定路径、degraded 语义统一（可附加于任意状态）。
> v0.6 修订：幂等键过期释放机制（事务内改写旧键，保窗口精确）、统一终态时序
> （一律先清理→扫描确认→落终态→释放锁，消除条款冲突）、状态机纳入 terminating、
> cancel_task 收敛为仅 L2、探测并发上限 ≥ 全网探测项总数 + 强制超时<间隔（SLA 严格成立）。
> v0.5 修订：commands 表补 identity 字段与幂等窗口过期规则、CA 轮换带外预装与旧指纹移除时点、
> M3b 验收 POSIX/Windows 差异化、日志落盘失败先终止再标 degraded、macOS L2 严格串行
> （消除按用户清理误伤）、terminating 前置持久化、cancel_task 归属梳理、告警 SLA 适用条件
> 与探测并发上限约束。
> v0.2 修订：能力分级、可靠任务协议、日志链路、cwd 强化、HTTPS 统一、资源限额、M3 拆分。
> v0.3 修订：幂等提交与崩溃恢复、CA 生命周期持久化、Docker 受限代理、cwd 句柄绑定防 TOCTOU、
> 硬上限补全（队列/连接/执行资源/磁盘满）、杀树防脱离（cgroup/孤儿扫描）、授权矩阵、
> L2 受信绝对路径、告警公式区分检测时延与投递目标。
> v0.4 修订：intent-only 崩溃恢复与控制台对账、幂等键作用域/冲突拒绝/唯一约束、
> CA 生成措辞统一与轮换双信任、cwd 逐级 O_NOFOLLOW 解析与 Windows 强度诚实标注、
> 资源上限具体化（cgroup memory.max / RLIMIT_AS / Job Object）、macOS 孤儿按用户归属、
> 正常退出清理与锁释放安全时序、cancel_task 归属 L2、告警公式计入超时与调度约束、
> 磁盘满保留空间与写入失败规则。
> 目标：部署于组网中心（腾讯云 1.13.158.180）的自托管控制台，统一管理所有节点与服务，
> 并以 MCP 为一等公民向 AI 助手（HanaAgent）暴露操作接口。

---

## 1. 目标与范围

### 管理对象（当前台账，以系统内数据为准，不再依赖助手记忆）

| 节点 | 系统 | 角色 | 现有服务 |
|------|------|------|----------|
| 腾讯云服务器 | Ubuntu 22.04 | 组网中心 | Headscale(8080/systemd)、DERP(Docker,443+3478)、Nextcloud(compose 五容器)、RustDesk hbbs/hbbr(21115-21119)、dsh-tunnel(3000) |
| Mac mini | macOS (Apple Silicon) | 日常工作机 | meshagent + 日常应用 |
| Windows 机器 | Windows | 家用机 | meshagent |

### 核心功能

1. **节点管理**：注册、心跳、在线状态、系统资源（CPU/内存/磁盘/网络）
2. **服务清单**：每节点上受管服务的状态（systemd unit / Docker 容器 / 进程），全网一张表
3. **探测与延迟**：HTTP/TCP 探测（ICMP 暂缓），历史延迟曲线，故障判定与告警
4. **基础操作**：白名单服务重启/启停，全程审计
5. **组网视角**：对接 Headscale REST API，展示节点注册状态与 last_seen
6. **告警**：飞书 webhook 通知
7. **MCP Server**：streamable HTTP 端点 + API key，供 AI 助手远程调用
8. **AI agent 调度**：发现并远程调用编码 CLI（见 4.2，高权限能力，独立授权）

### 能力分级（权限模型，贯穿全设计）

| 级别 | 能力 | 授权要求 |
|-----|------|---------|
| L0 | 只读查询（节点/服务/探测/组网状态） | MCP API key 或面板登录即可 |
| L1 | 服务操作（重启/启停白名单服务） | MCP key 附带 L1 scope；结构化参数；审计 + 飞书播报 |
| L2 | 编码任务调度（invoke_agent） | 独立 L2 开关（节点级默认关闭）；仅 Mac/Windows 节点可用，**云节点默认禁止**；审计 + 飞书播报 |

> 定性：L1 是有限能力（固定对象固定动作）；L2 是**高权限远程执行**——编码 CLI
> 可能动 shell、改文件、联网，与 L1 严格分开授权与隔离，M3b 前置条件见 §9。

### 明确不做（v1）

- 不做日志聚合、不做配置管理、不做用户体系（单管理员 + token 即可）
- 不做通用 shell 通道（安全边界，操作仅限白名单）
- ICMP 探测暂缓（避免为 ping 提权；HTTP/TCP 已覆盖首期主要服务）
- 服务型 agent（如 HanaAgent）v1 只发现登记，不纳入 invoke 调用范围（缺调用适配器）

---

## 2. 总体架构

```
                     Tailnet (Headscale 组网)
  ┌──────────────┐        ┌──────────────┐        ┌──────────────┐
  │   Mac mini   │        │ Windows 机器 │        │  腾讯云服务器 │
  │  meshagent   │        │  meshagent   │        │   五套服务    │
  └──────┬───────┘        └──────┬───────┘        └──────┬───────┘
         │ HTTPS 上报/拉取命令    │                       │
         └───────────────┬───────┘                       │
                         ▼                               ▼
              ┌─────────────────────────────────────────────┐
              │        meshconsole（同机部署，云服务器）        │
              │  ┌─────────┐ ┌────────┐ ┌────────┐ ┌──────┐ │
              │  │节点注册/ │ │探测引擎│ │操作通道 │ │告警  │ │
              │  │心跳/指标 │ │http/tcp│ │白名单+ │ │飞书  │ │
              │  │接收      │ │/icmp   │ │审计    │ │webhook│ │
              │  └─────────┘ └────────┘ └────────┘ └──────┘ │
              │  ┌─────────┐ ┌─────────────────────────┐    │
              │  │SQLite   │ │ Headscale 集成器(REST)   │    │
              │  │存储     │ └─────────────────────────┘    │
              │  └─────────┘ ┌─────────────────────────┐    │
              │              │ MCP Server (HTTP+apikey) │    │
              │  ┌─────────┐ └─────────────────────────┘    │
              │  │Web 面板  │                                │
              │  │(只读)    │                                │
              │  └─────────┘                                │
              └─────────────────────────────────────────────┘
                              ▲
                              │ streamable HTTP + API key
                     ┌────────┴────────┐
                     │ HanaAgent (Mac)  │ → list_nodes / restart_service / ...
                     └─────────────────┘
```

通信原则：**一切优先走 Tailnet；公网暴露是例外形态，须满足 §7-4 全部条件**
（三层认证 + 登录防护 + 启动门槛 + 防火墙收敛）。meshconsole 缺省只监听
Headscale 分配的 Tailnet IP（或 localhost）；经伦哥批准的公网形态下，腾讯云
防火墙仅放行 TCP 7700，边界依赖 §7-4 防线而非网络隐匿。
信任边界修正：Tailnet 内部仍是需要设防的领地——节点间用 Tailnet ACL 收敛访问，
云服务器主机防火墙（ufw/iptables）限制 7700 只允许本机与授权源；MCP 端点校验
Origin 头与请求体/连接限额。传输统一 HTTPS：**CA 与证书由首次部署脚本生成并持久化**
（详见 §7），进程启动仅加载，不重新生成；agent 与 MCP 客户端以 CA 指纹固定验证，
禁止跳过证书校验。

---

## 3. 技术选型与理由

| 项 | 选择 | 理由 |
|----|------|------|
| 语言 | **Go（固定受支持的版本，如 1.24.x，锁定 toolchain 与依赖版本）** | 单二进制跨平台编译（agent 需 mac/win/linux 三平台）、内存占用低（2核2G 现状）、并发模型契合探测与上报场景；CI 验证 linux/amd64 + macOS/arm64 + Windows 目标 |
| 采集库 | **gopsutil**；Docker 容器统计**不经 SDK 直连 socket**，统一经受限只读途径（见 §7-5） | gopsutil 是 Beszel 验证过的跨平台系统指标方案；Docker 采集与操作同走受限通道，避免实现歧义 |
| MCP | **官方 Go SDK**（modelcontextprotocol/go-sdk） | streamable HTTP 传输内置，协议兼容有保障 |
| 存储 | **SQLite**（modernc.org/sqlite，纯 Go 免 CGO）；运行参数：WAL + busy_timeout + 单写连接 + 有界连接池 + schema migration，处理 SQLITE_BUSY | 单文件好备份；指标保留 7 天足够；2核2G 不引入独立 DB 进程 |
| 传输 | REST（agent：定时上报 + 长轮询取命令，带取消/退避抖动/单节点轮询上限；心跳、命令、日志三通道独立） | 比 WebSocket 简单可靠，断线自愈，NAT 友好 |
| Web UI | 嵌入式静态页（embed + htmx/原生） | 只读面板无需前端框架，减少构建链 |
| Agent↔控制台 | **HTTPS REST**（定时上报 + 长轮询取命令；心跳、命令、日志三通道独立） | 比 WebSocket 简单可靠，断线自愈，NAT 友好 |
| 告警 | 飞书自定义机器人 webhook | 通知直达伦哥手机 |

---

## 4. 模块设计

### 4.1 meshconsole（控制台，单二进制）

**A. 节点注册与心跳**
- `POST /api/agent/register`：首次注册，携带**一次性短期注册 token**（绑定预期节点身份，用后即废），签发节点 token；凭据支持吊销与轮换
- `POST /api/agent/heartbeat`：每 15s 上报指标快照 + 服务状态清单；采集失败的指标上报 `unavailable`，**不填零**（gopsutil 跨平台语义不一致，CPU 采样异步进行不阻塞心跳）
- 超过 60s 无心跳判定 offline；区分 last_seen（最近收到心跳）与 last_success（最近采集成功），避免混淆
- 断线/睡眠恢复后凭节点 token 续接，**不重复注册**（M1 验收项）

**B. 指标接收与存储**
- 每次心跳写入 metrics 表（cpu_pct、mem、disk、net）
- 后台任务每小时清理 7 天前数据
- 磁盘量估算：3 节点 × 5760 点/天 × ~200B ≈ 每天约 3.5MB，一周 ~25MB，可控

**C. 探测引擎**（参考 Uptime Kuma 的设计，自研简化版）
- 探测类型：http（状态码+耗时，响应体限 64KB）、tcp（端口握手+耗时）；每项探测有独立超时与并发上限，防重入
- 判定防抖：连续 3 次失败才算 down（恢复同理），避免瞬时抖动误报
- 默认间隔 15s，可按探测项配置
- **告警耗时预算**：区分两个指标——
  最坏检测时延 = 故障发生至下次探测（≤间隔）+ (阈值-1)×间隔 + **探测超时**；
  **严格成立的前提（配置校验强制）**：①探测超时 < 间隔（默认 5s < 15s，超配直接拒绝），
  使防重入顺延永不发生；②探测执行并发 ≥ 全网探测项总数（v1 规模 3 节点×5 项=15，
  上限设 32），保证零排队；两项前提任一不满足则配置校验失败；
  默认参数下 ≤ 50s；投递目标 = 飞书正常 <10s，
  含重试退避最长 5 分钟（重试耗尽记录失败，firing 状态保留待下轮重发）；
  告警 SLA 写作「检测 ≤60s（默认参数，前提强制），投递正常秒级、最坏 5 分钟」
- 健康分层：process（agent 上报的进程/容器状态）→ port/http（传输可达）→ app（业务健康，v1 仅 Nextcloud 做 HTTP 首页探测），告警注明探测视角，端口通不代表业务正常（DERP 443 不验证中继、RustDesk 21116 的 TCP 不覆盖 UDP）
- 每次探测写 probe_results（status, latency_ms, error，error 截断 2KB），保留 7 天
- 内置默认探测：Headscale 8080 / DERP 443 / RustDesk 21116 / dsh-tunnel 3000 / Nextcloud

**D. 操作通道**（白名单硬约束 + 可靠执行协议）

*可靠执行协议*：
- 操作持久化为 commands 表记录，全局唯一 command_id 作幂等键
- **提交幂等**：MCP 客户端可携带 idempotency_key（可选），**绑定调用方身份**（同一身份才比较）；控制台以 (identity, idempotency_key) **全局唯一索引** + 事务判定：同 key 同参数且 created_at 在 10 分钟窗口内 → 返回原记录；同 key 不同参数（窗口内）→ 409；**窗口过期**：同一事务内将旧行主键字段移入独立归档字段 `archived_key`（主 idempotency_key 置 NULL，SQLite 唯一索引允许多 NULL），随后正常新建——**无命名空间冲突**（客户端无法伪造归档字段），过期键立即可复用
- 状态机：`queued → sent → running → acked / failed / **unknown**`
- agent 领取带租约（lease）；**去重记录持久化**：agent 本地 state 库（SQLite）
  在执行前先落盘 intent（command_id + 时间），执行后写结果；本地保留 7 天；
  崩溃重启后加载，重复 command_id 直接返回历史结果，不重复执行
- **过期租约回执**：租约超时后 agent 仍完成执行时允许迟交（late result），
  控制台将 unknown 恢复为终态并标 late；租约超时 30 分钟仍无回执 → 维持 unknown，
  等待人工/MCP 查询裁决，永不自动重发
- **崩溃恢复对账**：
  - agent 侧重启：intent 已落盘但无结果 → 该 command 置 `unknown`（不重执行），上报控制台对齐状态；intent+结果俱全 → 直接返回历史结果
  - 控制台侧重启：queued（未发出）→ 正常重新入队下发；sent/running 且租约未过期 → 维持等待；租约已过期且无回执 → 按 unknown 规则处理

*L1 服务操作*：
- MCP/面板传入**结构化参数** `{node_id, service_id, action}`，不传原始命令字符串；
  agent 将 service_id 映射到**配置里固定的本地对象**（unit 名 / 容器名）与
  **固定绝对路径的可执行文件**，防 PATH 替换；未映射的 service_id 拒绝
- 审计记录凭据身份、request_id、授权级别、时间、结果

**E. 任务日志链路**（v0.2 新增）
- agent 本地流式落盘任务输出，分块上传（序号 + 字节偏移）到控制台，
  控制台确认后推进指针；断线时本地缓存（上限 20MB/任务）恢复后重传，去重幂等
- 单任务日志硬上限 10MB（口径：该任务全部分块累计，超出截断并标记 truncated）；
  agent 本地未确认分块缓存 ≤20MB（口径：单任务），达上限丢弃最旧分块并在续传时标记缺失；
  全局 tasks 目录上限 1GB
- 飞书播报只发**脱敏摘要**（任务名、节点、状态），不投递完整输出（防泄密）

**F. Headscale 集成器**
- 用 Headscale REST API + 预生成 API key，每 60s 拉取：节点列表、在线状态、last_seen、tailscale IP
- 拉取失败时保留上次缓存并标记 stale，**不将缓存节点判为离线**；区分 synced_at 语义
- 存入本地表，供 Web 面板与 MCP 工具查询（对 Headscale 只读，不代操作）

**G. 告警器**
- 规则：探测 down / 节点 offline / 服务异常退出
- 通知生命周期：firing→resolved **成对通知**；维护窗口静默；同对象 10 分钟静默窗但**状态变化立即通知**；飞书发送失败重试（退避），积压上限 100 条后丢弃最旧并记录
- 飞书消息含：对象、状态、时间、最近一次探测详情（脱敏）
- **监控盲区补位**：控制台自身失联（云机宕机/进程退出/Tailnet 断裂）由 Mac mini 侧
  轻量看门狗监测（定时探测控制台 healthz，失联经飞书直报）——M2 交付

**H. MCP Server**（见第 6 节）
- streamable HTTP，路径 `/mcp`，Bearer API key 认证（scope 绑定 L0/L1/L2）；
  校验 Origin 头；请求体 ≤1MB、并发连接限额；MCP 返回内容视作不可信数据，
  Web 端渲染一律转义

**I. Web 面板**（只读，M1 先上）
- 单页：节点卡片（在线状态/资源条）+ 服务总表 + 探测状态列表
- htmx 轮询刷新，无需构建工具

### 4.2 AI Agent 发现与调度（新增章节，控制台的"能力总线"）

> 定位：除管设备、管服务之外，控制台统一登记全网终端机上安装的 **AI agent 资源**
> （zcode、codex、claude 等 CLI 型；HanaAgent 等服务型），并通过 MCP 提供远程调用。
> 这是对伦哥代码协作制度（zcode 写 + codex 审）的系统化：agent 从本机工具升级为全网可调度资源。

**A. Agent 发现（meshagent 侧扫描）**

- 双轨发现机制：
  1. **内置已知清单**：meshagent 内置常见 AI CLI 名单（zcode / codex / claude / gemini / aider 等），
     定期探测 PATH 中是否存在（`exec.LookPath`）+ 执行 `--version` 取版本号
  2. **配置显式声明**：自定义 agent 写进 meshagent 配置
     ```yaml
     agent_scan:
       custom:
         - name: my-agent
           type: cli
           command: /path/to/agent
           version_flag: "--version"
       services:
         - name: hana-agent        # 服务型：本地端口探测存活性
           port: 5800
           probe: tcp
     ```
- 扫描频率：低频（每 5 分钟），避免频繁 exec；结果随心跳上报
- 每个 agent 记录：名称、类型（cli/service）、版本、可执行路径、健康状态、所属节点

**B. Agent 注册表（控制台侧）**

- agents 表汇总全网 agent，Web 面板与 MCP 均可查询
- 健康状态：cli 型 = 最近一次 version 探测成功；service 型 = 端口探测存活
- 同名 agent 多节点共存时按 节点+名称 唯一定位
- **发现 ≠ 可调用**：自发现路径不自动成为可调用路径；可调用性仅来自配置 allow（见 C）。
  M1 以显式配置为主，PATH 扫描为辅（仅探测登记，不自动开放）

**C. Agent 远程调用（异步任务模型）**

- 调用走模板化命令，**可执行文件固定受信绝对路径**（部署期写入受信目录，任务用户不可写），
  {task} 作为**单一 argv 参数**注入，不经过 shell 拼接：
  ```yaml
  agent_invoke:
    allow:
      - name: zcode
        command: /opt/meshconsole/bin/zcode   # 受信绝对路径，符号链接部署期解析
        template: ["{cmd}", "-p", "{task}"]
        timeout_min: 60
        max_concurrent: 2
        allowed_dirs:      # 工作目录白名单（规范化身份比对）
          - /Users/tl.m/coding
        default_dir: /Users/tl.m/coding
      - name: codex
        command: /opt/meshconsole/bin/codex
        template: ["{cmd}", "exec", "{task}"]
        timeout_min: 30
        max_concurrent: 1
        allowed_dirs:
          - /Users/tl.m/coding
        default_dir: /Users/tl.m/coding
  ```
  发现登记的路径仅作参考，**不得自动成为可调用路径**；CLI 程序本体、其配置、
  权限参数所在目录属受信区，任务用户（L2 独立用户）无写权限
- **工作目录约束（代码目录规则）**：
  - invoke_agent 必须携带 cwd（任务工作目录）；不传时取条目 default_dir，
    **绝不使用进程默认 cwd**（launchd/systemd 下为 /，防项目乱建）
  - cwd 校验：clean 后路径前缀 + 分隔符判断（`~/coding` 与 `~/coding-evil` 不得混淆），
    **禁止裸字符串前缀**；目录不存在时校验最近已存在祖先在白名单内，再安全创建并复验；
    Windows 节点需处理盘符大小写与 junction/reparse point（M3b 验收项）
  - **TOCTOU 防护**：校验与启动全程**逐级受控解析**——从白名单根目录开始
    `open(O_DIRECTORY|O_NOFOLLOW)` 逐级打开到目标（任一级为符号链接即拒绝），
    目录不存在时用 `mkdirat` 相对父句柄创建，fstat 核对最终句柄 dev/ino，
    启动子进程用 fchdir 绑定句柄，全程不重走路径解析；
    **Windows 诚实标注**：CreateProcess 的 cwd 参数是路径字符串，无法句柄绑定，
    等价强度做不到——替代缓解：校验后到启动窗口极短 + 白名单祖先目录对任务用户
    **只读 ACL**（无写权限即无法替换）+ reparse point 拒绝；残余风险写入 M3b 验收说明；
    同 cwd 锁使用规范化对象身份（dev/ino 或 volume serial+file index）作锁键
  - 白名单外目录直接拒绝并返回错误；default_dir 对齐伦哥代码存放铁律（~/coding/ 之下）
  - 任务记录 cwd，审计可查每次派工的工作目录
  - **诚实声明：cwd 只限启动位置，不限制进程访问范围。** 文件系统级隔离
    （macOS seatbelt profile / Windows AppContainer / 独立低权限系统用户）
    作为 **M3b 前置条件**落地，此前 L2 能力不得开放
- **注入缓解**：单 argv 消除 shell 注入，但任务文本/仓库文档/工具输出仍可能诱导编码 CLI 越权。
  缓解：固定各 CLI 非交互模式与权限参数（禁止默认跳过安全检查的模式）、
  CLI 凭据与控制台/节点凭据隔离、任务上下文审计留存、
  文件系统隔离（见上，M3b 前置）三道防线
- **进程树终止与对账**：**防脱离分级**——Linux 用 cgroup v2（systemd-run --scope）
  包住任务进程，终止时杀整个 cgroup，后代无法逃逸；
  macOS 进程组 + **孤儿扫描兜底，按用户归属而非 PID 快照**（L2 任务跑在专用用户下，
  扫描该用户全部进程即覆盖之后创建/脱离的后代，天然免疫 PID 复用）；
  **macOS L2 并发固定为 1（严格串行）**——按用户清理与多任务并发互斥，
  避免一个任务结束误杀同用户其他任务；Linux/Windows 保持三层并发上限；
  Windows 用 Job Object（kill-on-close，天然覆盖全部后代）；
  **统一终态时序（所有结束路径一致，含正常完成/超时/取消/日志落盘失败）**：
  ①进入清理前持久化 `terminating` → ②终止/清理（杀树）→ ③日志流冲刷 →
  ④孤儿扫描**确认无残留进程** → ⑤落终态（done/failed/timeout/cancelled，
  清理异常时标 degraded 附加标记）→ ⑥释放 cwd 锁与并发额度；
  ④失败 → 保持 terminating 并标 degraded、保持锁、飞书通知，待人工处置；
  正常完成与异常结束走同一时序，不存在「先落终态后扫描」的路径；
  meshagent 重启后对账：状态为 running/terminating 的任务按归属规则扫描
  孤儿进程并终止/标记（按上述归属规则）；
  任务事件（queued/started/terminating/stopped/timeout）写入 task_events 供审计
- **异步执行**：编码任务动辄几十分钟，MCP 的 invoke_agent 立即返回 task_id，
  进程输出流式落盘并按 E 节日志链路上传，任务状态机：
  `queued → running → terminating → done / failed / timeout / cancelled`；
  **L2 不设 unknown**（unknown 仅为 L1 结果丢失态）：L2 结果不确定由双侧重启规则覆盖——
  agent 重启发现 intent-only → 任务置 `failed(degraded)` 并如实上报；控制台重启
  不影响 agent 侧继续执行与回传；**degraded 语义统一：可附加于任意状态**的标记
  （如 terminating+degraded = 清理异常保持锁待人工；failed+degraded = 结果不可信），
  非独立状态
- 轮询工具 get_agent_task(task_id) 返回状态 + 输出尾部（默认 200 行，可调）
- 资源保护：并发上限三层——节点总上限（默认 3）> CLI 条目 max_concurrent >
  同一 cwd 加互斥锁（同项目不并行两任务，防写码与审查互相覆盖）；
  **执行资源上限（具体化）**：
  - Linux：cgroup v2 `memory.max` 2GB（覆盖整棵任务树累计）+ `pids.max` 128
  - macOS：`RLIMIT_AS` 2GB（单进程地址空间近似，无法覆盖树累计——诚实标注，
    树累计靠专用用户+孤儿扫描间接收敛）+ `RLIMIT_NPROC` 128（L2 专用用户，按用户计数正好隔离）
  - Windows：Job Object `JOB_OBJECT_LIMIT_PROCESS_MEMORY` 2GB/进程 + ActiveProcessLimit 128
  - L1 命令超时 60s；task_input 硬上限 32KB，超出拒绝；**云节点默认禁止 L2**（节点配置 allow_coding_tasks: false）
- **全局硬上限汇总**：每节点待执行命令队列 ≤10（超出拒收）；每节点并发轮询 ≤2；
  MCP 并发请求 ≤8；**磁盘满处理（非 M4）**：控制台磁盘水位 85% 告警、90% 拒收新任务、
  探测结果与任务日志（仅内存计数）；**保留空间规则**：心跳、审计、回执、
  SQLite WAL 属必须继续写入的最小集合，预留 ≥200MB 且水位拒收对其豁免；
  任务日志落盘失败 → **先走终止/清理流程（杀树+确认无残留，同取消流程）**，
  再标 `failed(degraded)` 并飞书通知——严禁进程仍在运行时落失败终态；
  agent 上传失败时本地缓存达上限即丢最旧并标记缺失
- 审计：每次调用写 operations 表 + 飞书播报（谁、在哪台机、调了什么 agent、cwd、任务摘要脱敏）

**D. 与代码流水线的衔接**

- 助手可通过 MCP 完成「查有哪些 agent → 派 zcode 写码 → 派 codex 审查 → 取回结果」全流程，
  不再依赖"CLI 只在 Mac 本地"的隐含前提，Windows 机器装了 agent 同样可调度

### 4.3 meshagent（节点代理，单二进制）

- 采集（每 15s）：
  - 系统：gopsutil（cpu、mem、disk、net、uptime、load）
  - 服务：配置文件声明受管服务清单
    ```yaml
    services:
      - name: rustdesk
        type: systemd
        target: rustdesk
      - name: derp
        type: docker
        target: derp
    ```
    状态查询：systemd 走 `systemctl show`（或 dbus）；docker 走受限只读途径
    （`docker ps --format` 经 helper，或只读白名单端点的 socket-proxy，见 §7-5，不经 SDK 直连 socket）
  - AI agent 扫描（每 5 分钟低频）：按 4.2-A 双轨机制，结果随心跳上报
- 上报：heartbeat 携带指标 + 各服务 active 状态
- 命令：长轮询取命令 → 精确白名单匹配 → 执行 → 回传
- 平台支持：linux（systemd）、macOS（launchd 部署，服务清单暂留空或声明进程型）、windows（NSSM/服务清单按需）
- 体积目标：< 10MB，常驻内存 < 30MB

---

## 5. 数据模型（SQLite 核心表）

```sql
nodes(id, name, role, os, arch, tailnet_ip, public_ip, agent_version,
      status, last_seen, last_success, created_at)
services(id, node_id, name, type, target, status, detail, updated_at)
metrics(id, node_id, ts, cpu_pct, mem_used, mem_total, disk_used, disk_total,
        net_rx, net_tx)                     -- 保留 7 天
probes(id, name, type, target, interval_s, enabled, node_id, created_at)
probe_results(id, probe_id, ts, status, latency_ms, error)  -- 保留 7 天
operations(id, node_id, action, operator_identity, scope_level, request_id,
           status, output, created_at)      -- 含凭据身份归因
commands(id UNIQUE, node_id, type, payload, identity, idempotency_key, archived_key,
         lease_owner, lease_until, state, result_ref, created_at, updated_at)
         -- 可靠执行协议核心表；(identity, idempotency_key) 唯一索引（NULL 不参与），
         -- 幂等键过期时主键字段移入 archived_key 并置 NULL 释放
ai_agents(id, node_id, name, type, version, path, status, invokable, updated_at)
agent_tasks(id, node_id, agent_name, task_input, cwd, status, log_path,
            exit_code, started_at, finished_at)
task_events(id, task_id, event, ts, detail)  -- 任务状态历史
alerts(id, rule, object_ref, state(firing/resolved), fired_at, resolved_at,
       notified_at, suppressed)
headscale_nodes(id, name, online, last_seen, tailnet_ip, synced_at, stale)
```

约定：外键 + 唯一约束 + 时间列索引；历史查询按时间范围强制 LIMIT，
长范围自动降采样；删除旧行后定期 incremental vacuum（不频繁全库 VACUUM）。
容量估算需计入页开销/索引/WAL/审计/任务内容，预留 ≥100MB；
上线前以基线实测为准（见 §9 M1）。

---

## 6. MCP 工具集（AI 助手接口，核心交付物）

| 工具 | 功能 | 读写 |
|------|------|------|
| `list_nodes` | 全部节点与在线状态、基础资源 | 读 |
| `get_node` | 单节点详情：实时指标、服务清单 | 读 |
| `list_services` | **全网服务总表**（名称/节点/类型/状态） | 读 |
| `get_service` | 服务详情 + 最近探测结果 | 读 |
| `get_mesh_status` | Headscale 视角：注册节点、在线、last_seen | 读 |
| `list_probes` / `get_probe_history` | 探测配置与延迟历史（支持时间范围） | 读 |
| `restart_service` | 白名单重启指定服务 | **写** |
| `cancel_task` | 取消运行中的 L2 编码任务（触发杀树流程；仅 L2 权限可调用；L1 服务命令执行 ≤60s，不提供取消） | **写** |
| `list_agents` | 全网 AI agent 清单（节点/类型/版本/健康状态） | 读 |
| `invoke_agent` | 在指定节点调用指定 agent 执行任务；必须携带工作目录 cwd（或用默认目录），异步返回 task_id | **写** |
| `get_agent_task` | 查询任务状态与输出尾部 | 读 |
| `get_operations` | 操作审计记录查询 | 读 |
| `get_alerts` | 近期告警 | 读 |

接入方式：HanaAgent MCP connector 配置 streamable HTTP 指向
`https://<tailnet_ip>:7700/mcp`（与全局 HTTPS 统一，无 http 入口），
API key 走环境变量；首次配置时通过带外方式（SSH/手动拷贝）获取 CA 证书并固定指纹。
（7700 为默认端口，可配）

### 授权矩阵（v0.3 新增）

| 主体 | 接口范围 | 数据权限 |
|------|----------|----------|
| 管理员（面板登录） | 全部接口 + 管理操作（凭据轮换、节点增删、配置修改） | 全部数据含任务原文与完整审计 |
| MCP key · L0 | 只读查询工具 | 节点/服务/探测/组网状态；任务仅状态 + 输出尾部 200 行，**不返回 task_input 原文**；审计仅摘要 |
| MCP key · L1 | + restart_service | + 操作结果（cancel_task 不在 L1：取消 L2 任务需 L2 权限，按目标任务级别校验） |
| MCP key · L2 | + invoke_agent / get_agent_task / cancel_task（仅限取消 L2 任务，按目标任务级别校验） | + 任务原文与完整日志 |
| 节点 token | 仅 /api/agent/* 三通道（心跳/命令/日志） | 仅本节点对象；**禁止访问管理接口、其他节点数据与审计查询** |

凭据吊销/轮换后旧 key 即时失效；所有管理接口仅管理员可达。

---

## 7. 安全设计

1. **能力分级授权**：L0 只读 / L1 服务操作 / L2 编码调度（见 §1）；MCP API key 绑定 scope；
   节点 token 只能操作本节点对象；跨节点访问一律拒绝
2. **全端点认证**：Web 面板（登录会话）、查询 API、任务日志读取、结果上传接口均需认证，
   无匿名端点（健康检查 healthz 除外，仅返回 ok）
3. **传输统一 HTTPS**：**CA 持久化生命周期**——首次部署时由安装脚本生成 CA + 证书
   （/opt/meshconsole/pki/，600 权限），**不随进程启动重新生成**；agent 与 MCP 客户端
   首次配置经带外方式（SSH/手动）获取 CA 并固定指纹；全程禁止跳过证书校验；
   **轮换流程**：①带外向全部客户端预装新 CA（此时客户端同时信任新旧）→ ②服务端切换
   用新 CA 签发证书 → ③确认全部客户端已信任新 CA（心跳/握手验证）→ ④移除旧指纹、
   服务端吊销旧证书；证书有效性不等价于客户端信任，以客户端实际握手为准
4. **信任边界与公网面板例外**（v0.8 重写，M1b-c2 落地）：公网暴露不是缺省
   形态，但也不再绝对禁止——**公网暴露须满足本条全部条件**，未满足前回环/
   Tailnet 是唯一入口。当前批准的公网形态与防线：

   - **三层认证**（网络形态无差别，始终开启，无「内网免登录」路径）：
     ①面板 = 账号会话（登录发 256bit 会话 token，Cookie `mc_session`
     HttpOnly/Secure/SameSite=Lax；滑动续期、30 天绝对期限；未登录一律
     302 /login，匿名可及仅登录页/静态资源/healthz）；
     ②MCP = Bearer API token（api_tokens 哈希查表，401 不泄露工具列表）；
     ③agent = 来源收敛（RemoteAddr 限回环/私网/Tailnet CGNAT 段，公网 403，
     不读转发头）+ 节点 token / 一次性注册 token（网络过滤非身份认证，
     token 校验独立保留）。
   - **登录防护与限流**：per-IP 5 次失败/分 → 429（滑动窗 cap 4096）；全局
     bcrypt 并发 ≤2（排队有上限）；未知用户名也执行 dummy bcrypt（防时序
     枚举）；改密/禁用用户即时撤销其全部会话与 API token。
   - **并发配额三闸独立**：agent API ≤64、面板 overview ≤8、MCP ≤4——公网
     流量不挤占 agent 心跳通道。
   - **公网模式启动门槛**（实际绑定地址非 {127/8, ::1} 即公网形态，任一失败
     拒绝启动）：存在 ≥1 个启用用户；config 文件普通文件且 0600 且属运行
     用户；panel_allowed_hosts 合法。数据文件权限强制：data 目录 0700、
     DB/WAL/SHM 0600。
   - **证书信任与 pin 更新流程**：公网访问凭同一自建 CA。ca_cert 模式 agent
     零动作（链验证不依赖 SAN 变更）；fingerprint 模式 agent 在切换流量前
     先更新指纹（服务端重签证书 → 带外取新指纹 → 更新 agent 配置 → 切换）；
     浏览器首次访问经带外渠道核对指纹/导入 CA，验收一律禁用 -k。
   - **回滚路径**：撤防火墙放行 + 恢复 .bak 证书 + listen 改回 Tailnet IP
     绑定；账号体系与来源收敛在回环形态下原样生效，回滚不降级安全性。
   - 其余既有限额保留：MCP 端点校验 Origin 头；请求体 ≤1MB、连接总数
     LimitListener、handler 并发上限（M1a/M1b 已实现）。
5. **最小权限执行**：meshconsole 与 meshagent 均以专用非 root 用户运行；
   Linux 服务操作经固定动作的提权 helper（sudo 白名单仅限 `systemctl restart/stop/start <固定unit>`
   与 `docker restart/stop/start <固定容器名>`），不给代理任意 sudo；
   **Docker 权限边界**：agent 进程不持有 docker socket，容器状态采集走只读途径
   （`docker ps --format` 经 helper，或只读白名单端点的 socket-proxy），
   容器启停仅经上述固定动作 helper；编码任务进程不接触 Docker；
   L2 编码任务在独立低权限用户下运行（M3b 前置条件；macOS 该用户即孤儿扫描归属依据）
6. **结构化白名单**：服务操作传 `{node_id, service_id, action}`，agent 映射到配置内固定对象；
   可执行文件绝对路径固定；自发现路径不得自动成为可执行/可调用路径
7. **cwd 白名单**：cleaned 路径前缀 + 分隔符判断，不存在目录校验最近祖先后创建复验，
   默认 ~/coding/，外域拒绝；进程访问范围隔离作为 M3b 前置条件（seatbelt/AppContainer）
8. **审计可归因**：记录凭据身份、request_id、授权级别、目标、状态变化；
   飞书通知只发脱敏摘要；日志/错误/节点名在 Web 转义，MCP 返回视作不可信数据
9. **资源限额**：请求体 ≤1MB、task_input ≤32KB、单任务日志 ≤10MB、tasks 目录 ≤1GB、
   队列长度上限、探测响应体 ≤64KB、并发/轮询上限；超限拒绝或截断并显式标记
   （M1a 实现范围：请求体 ≤1MB 与 handler 并发 ≤64（注册+心跳共享配额）；
   连接总数限额随 TLS 于 M1b 实现）
10. SQLite 每日一致性备份（在线 backup API）到 /opt/meshconsole/backup（保留 14 份）
    + **异机副本**（经 Taildrop 投递到 Mac mini，保留 7 份）+ 恢复演练（M2 交付）

---

## 8. 部署方案

- 云服务器：`meshconsole` 二进制 + systemd unit；数据目录 /opt/meshconsole/
- 各节点：`meshagent` 二进制 + systemd（Linux）/ launchd（macOS）/ 计划任务或服务（Windows）
- 发布：GitHub 私有仓库 mtl802/meshconsole，gh workflow 交叉编译三平台 + 制品下载
- 代码位置：`~/coding/meshconsole/`（遵守项目存放规则）

### 仓库结构（单仓双二进制）

```
~/coding/meshconsole/
├── cmd/
│   ├── console/        # 控制台入口（meshconsole）
│   └── agent/          # 节点代理入口（meshagent）
├── internal/
│   ├── registry/       # 节点注册/心跳/指标接收
│   ├── probe/          # 探测引擎
│   ├── ops/            # 操作队列 + agent 调用通道（含 cwd 白名单校验）
│   ├── agentdisc/      # AI agent 发现逻辑（已知清单扫描 + 自定义声明）
│   ├── headscale/      # Headscale REST API 集成
│   ├── alert/          # 飞书 webhook 告警
│   ├── store/          # SQLite 访问层
│   └── mcpsrv/         # MCP server（streamable HTTP）
├── web/                # 面板静态文件（embed）
├── deploy/             # systemd unit / launchd plist / Windows 服务 / 配置样例
└── .github/workflows/  # 交叉编译 + 制品发布
```

- agent 调用产生的任务日志落在控制台侧 /opt/meshconsole/tasks/<id>.log，不污染代码目录
- meshagent 被远程调用时永远显式设定 cwd（来自任务白名单校验结果），进程级隔离于自身安装目录

### 账号/会话/token 运行条款（v0.8 新增，M1b-c2）

- **账号引导**：CLI（`meshconsole user add`）先跑 migration 再操作，bootstrap
  不依赖既有数据；交互输入口令（bcrypt cost 10，两遍确认），无明文口令参数
  （不走 shell 历史/进程列表）。
- **会话**：登录发 256bit token，服务端只存 SHA-256；Cookie `mc_session`
  （Path=/、host-only、HttpOnly、Secure、SameSite=Lax）；每次认证请求滑动
  续期（节流 ≤1 次/小时/会话），绝对期限 30 天不越过；过期/撤销行由后台
  循环低频回收。
- **API token**：`token create` 签发 mcp_ 前缀 256bit 明文一次，服务端只存
  哈希；可带到期与说明；`token revoke` 按 id 即时吊销；MCP HTTP 端点以
  `Authorization: Bearer` 使用。
- **撤销语义**：改密（passwd）与禁用（disable）在单事务内撤销该用户全部
  session 与 api_tokens——凭据撤销即时生效，无宽限窗口；禁用最后一个启用
  用户被拒绝（防自锁门外）。
- **数据面**：账号数据落在 data 目录（0700）/ SQLite（0600，WAL/SHM 同）；
  stdio MCP 只读打开不迁移，CLI 与服务模式共享同一 migration 序。

## 9. 分期计划

| 期 | 内容 | 验收标准 |
|----|------|----------|
| **M0 资源基线**（新增，半天） | 上线前实测：五套服务正常运行 + Nextcloud 定时任务/备份时的 MemAvailable、峰值 RSS、swap、CPU、I/O、磁盘余量 | 报告确认控制台 ≤80MB + agent 各 ≤30MB 可容纳；否则先瘦身再动工 |
| **M1 骨架** | 节点注册/心跳/指标（含 unavailable 语义）、服务清单、**全端点认证**、AI agent 发现（显式配置为主+PATH 探测登记）、SQLite（WAL 参数齐备）、Web 只读面板、MCP 只读工具（list_nodes/get_node/list_services/get_mesh_status/list_agents）、Headscale 集成 | 三平台上线；未认证/跨节点访问被拒；睡眠与断线恢复后无重复注册；采集失败显示 unknown；HanaAgent 完成 MCP 协商；连续运行 24h 记录整机资源 |
| **M2 探测告警** | 探测引擎（防抖/超时/并发上限/响应体限额）、飞书 webhook（firing/resolved 成对+维护窗口+重试）、get_probe_history、Mac 侧看门狗、**备份链路**（一致性备份+异机副本+恢复演练）、**磁盘水位告警/拒收** | 探测 down 后按 SLA（检测 ≤60s，投递正常秒级）飞书收到告警；误报防抖与恢复通知验证；看门狗在停掉控制台 5 分钟内飞书报告；备份恢复演练通过；模拟磁盘 90% 水位拒收生效 |
| **M3a 服务操作** | 可靠执行协议（command_id/提交幂等键/租约/unknown 态/agent 去重持久化）、结构化白名单、restart_service、提权 helper（systemctl+docker 固定动作）、审计归因、飞书播报、任务队列限额与拒收 | 白名单内操作成功且审计可查；同 idempotency_key 重试不双发；结果丢失进 unknown 且不重发、迟交回执正确恢复终态；非白名单被拒；最小权限验证（非 root、helper 白名单外拒绝、agent 无 docker socket） |
| **M3b 编码调度** | 前置条件：独立低权限用户 + 文件系统隔离（seatbelt/AppContainer）+ 云节点 L2 关闭确认落地。内容：invoke_agent/get_agent_task/cancel_task、cwd 白名单强化、受信绝对路径、TOCTOU 句柄绑定、进程树终止（cgroup/进程组+按用户孤儿扫描/Job Object）与对账、日志分块上传链路、三层并发上限（macOS L2 串行）/同 cwd 锁/执行资源上限 | **差异化验收**：POSIX（Linux/macOS）按 TOCTOU 全强度验收（逐级 O_NOFOLLOW、句柄绑定、创建→启动全程防替换）；Windows 按缓解后标准验收（祖先只读 ACL + reparse 拒绝 + 启动窗口极短），「校验后替换」残余风险列为已知接受项并记录在案；恶意任务测试不越权；超时杀整树无孤儿（含 cgroup/孤儿扫描验证，macOS 验证串行不误伤）；断线日志恢复续传；任务队列与磁盘限额生效；执行资源上限生效；MCP 派 zcode 写码任务完整取回输出 |
| **M4 打磨** | 历史图表、面板聚合页 | 长时间范围查询限额生效；升级迁移验证 |

## 10. 开源参考清单（"通过他的功能实现细节"）

| 项目 | 借鉴点 |
|------|--------|
| Beszel (henrygd/beszel) | gopsutil+Docker SDK 采集细节、agent 轻量化实践、指标数据结构 |
| Uptime Kuma | 探测类型设计、失败判定防抖（重试次数/静默窗口）、心跳表结构 |
| headscale-admin | Headscale REST API 认证头、分页与字段用法 |
| Homepage | 面板信息架构（节点/服务/探测三区布局） |

---

## 11. 风险与待确认

1. **Windows agent 的服务采集**：Windows 无 systemd，M1 先做指标上报，服务清单按进程检测，后续按需加 NSSM/Windows Service 支持；junction/reparse 处理在 M3b 验收
2. **macOS agent 常驻**：launchd 保持在线即可，服务清单可先只上报本机关键进程
3. **2核2G 余量**：以 M0 实测基线为准，不凭估算开工；超标则先降采样/瘦身
4. **API key 管理**：Headscale API key、飞书 webhook token、MCP API key 存放于云服务器 config.yaml（600 权限），不入 git；支持轮换
5. **agent 调用的授权范围**：白名单收录 zcode / codex（Mac mini）；其他 CLI 默认只登记不开放调用；L2 整体待 M3b 前置条件落地后由伦哥手动开启
6. **CLI 运行环境差异**：launchd/Windows 服务下的 PATH/HOME、CLI 凭据与交互终端不同，认证失效/额度耗尽/等待输入等失败需分类返回（M3b 验收项）
