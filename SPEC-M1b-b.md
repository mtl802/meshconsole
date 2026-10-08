# SPEC-M1b-b · MCP Server + Headscale 集成 + Web 只读面板

版本：v1（2026-10-08） · 前置：M1b-a 已交付（commit 19f3a33）
范围基线：DESIGN.md v0.7 §6（MCP）、§2/§3（Headscale）、§8（Web）；本任务书为准。

## 1. 目标

三个交付物 + 三个观察点修复，全部只读，不引入任何写操作：

1. **MCP Server**（stdio 传输）：把 mesh 状态暴露为 MCP 工具，供 AI agent（Hana/zcode/codex）实时查询，替代凭记忆回答。
2. **Headscale 集成**（只读拉取）：console 定期拉取 headscale REST API，把 tailnet 节点状态并入 mesh 视图。
3. **Web 只读面板**：替换临时 status.py，单二进制内嵌，Liquid Glass 设计语言。

## 2. MCP Server（新增 `cmd/meshconsole-mcp` 或 `meshconsole mcp` 子命令，你定，倾向子命令）

- 传输：**stdio**（JSON-RPC 2.0，MCP 2025-06-18 规范语义）。不监听任何端口（网络暴露为零，M3 再议 HTTP+SSE）。
- 库允许引入官方 `github.com/modelcontextprotocol/go-sdk`（优先）或 `mark3labs/mcp-go`；版本锁定进 go.mod；若依赖拉取失败则手写最小 JSON-RPC 帧（initialize/tools/list/tools/call 三个方法即可）。
- 只读工具五个：
  - `list_nodes()` → nodes 表全行（id/name/status/role/agent_version/最近心跳时间）
  - `get_node(name)` → 单节点详情 + 最近 24h metrics 摘要（cpu/mem 均值峰值、磁盘占比）+ 该节点 services + agents
  - `list_services(node?)` → services 表（可按节点过滤）
  - `list_agents(node?)` → ai_agents 表
  - `get_mesh_status()` → 汇总：节点数/在线数/offline 名单、服务异常名单（status != active 且未过宽限）、headscale tailnet 概况、数据新鲜度（库最新 metrics 时间）
- 配置：`-config console.yaml` 复用（读 db_path；MCP 不需要 TLS/token——stdio 本机进程模型）。
- 明确禁止：任何 INSERT/UPDATE/DELETE 路径；db 打开用只读模式（`?mode=ro`）。

## 3. Headscale 集成（console 内嵌 goroutine）

- 数据源：headscale REST `GET /api/v1/node`（Authorization: Bearer <api_key>）。API key 从 console.yaml 新增字段 `headscale:` 段读取（`url` 默认 `http://127.0.0.1:8080`、`api_key`、`interval_s` 默认 300s）。config 无该段 → 集成禁用，日志 INFO 一次，不报错。
- 存储：新表 `tailnet_nodes`（migration v3：id/machine_name/ips/online/last_seen/updated_at），每次拉取全量替换（事务）。拉取失败保持旧数据 + 集成连续失败计数进日志（WARN，节流 1/h）。
- `get_mesh_status` MCP 工具与 Web 面板合并展示 tailnet 数据。
- 复用性约束：HTTP client 超时 ≤10s；响应解析只取需要的字段，未知字段忽略（headscale 版本演进不炸）。

## 4. Web 只读面板（替换 status.py）

### 4.1 技术基线（硬约束）

- **纯静态 HTML/CSS/原生 JS，Go `embed` 进二进制**（`web/` 目录 → `//go:embed`）。**禁止** Node/npm 构建链、React/Next.js、外部 CDN 依赖（面板要在内网/离线可用）。图表用内联 SVG/canvas 手绘（metrics sparkline 足够）。
- 数据获取：页面 JS `fetch('/api/panel/overview')`（新增只读 JSON 端点，返回 nodes/services/agents/tailnet/metrics 摘要聚合），**15s 轮询**。该端点与 MCP 同源只读逻辑（service 层复用，别复制 SQL）。

### 4.2 安全（DESIGN §8，全部必做）

- 面板与 agent API 同端口（console.yaml `listen` 不变，仍 127.0.0.1:7700）。Go 1.22 mux：`GET /` 与 `GET /api/panel/*` 走面板，`/api/agent/*` 不变。
- **Origin/Host 校验**：非 localhost 的 Host 头（含 DNS rebinding 变体如 `attacker.com:127.0.0.1` 解析到 127.0.0.1）→ 403。允许名单：localhost/127.0.0.1/[::1]（带或不带端口）。Origin 头存在时校验同名单。
- 响应头：`X-Content-Type-Options: nosniff`、`Cache-Control: no-store`。无 Cookie/无 CSRF 面（无状态只读）。
- 日志：面板访问打 DEBUG（不打 INFO 刷屏）。

### 4.3 设计（Liquid Glass，硬要求）

**先完整读 `docs/skills/liquid-glass-frontend/SKILL.md`，再读 `references/liquid-glass.md`、`references/creative-direction.md`、`references/typography-color.md`、`references/theming.md`、`references/motion.md`，照其原则执行。** 该技能技术基线是 Next.js+Tailwind——**只取设计系统与原则，丢弃其构建链**：`.glass-surface` 等 utilities 用纯 CSS 手写进 `web/glass.css`。

设计要求（技能原则 → 本面板落地）：

- **气质**：基础设施控制台 ≠ SaaS 仪表盘。选定一个明确 aesthetic direction（建议：深色底 + 玻璃浮层 + 等宽字数据的"控制室"气质，或你自己定的更优方向，写进交付说明），拒绝"三张等宽卡片居中 hero"的模板脸。
- **真实 frost**：重 `backdrop-filter: blur()+saturate()` + specular 顶边 + 背后色晕；不透明度接近玻璃而非 `rgba` 透明盒。玻璃底下必须有 atmosphere（渐变 mesh/色斑背景），否则玻璃不可读。
- **色彩单一源**：全站色板从三个 RGB 三元组派生（技能 theming.md 方法），深色为主 + 浅色可切（M1b-b 只做深色即可，浅色留 TODO 注释）。
- **排版**：数据用等宽字体（JetBrains Mono / IBM Plex Mono 系，考虑离线：用系统等宽栈也行但要在交付说明里说明取舍）；标题字体有性格；状态用色点+文字不依赖颜色单通道。
- **动效**：页面载入一次 stagger 编排 + 数据刷新时数字/状态点的过渡（一条统一 easing 签名）；**禁止**全元素 uniform fade-in-up。offline/异常状态有可注意的视觉处理（如呼吸红点）。
- **布局**：非对称网格打破（技能 layout-composition.md），节点卡不等宽（主节点大卡 + 次节点小卡）；服务清单与 agent 清单用 glass 内嵌层（`.glass-inset` 语义）。
- **内容区块**（最小集）：① mesh 总览头部（在线/离线/服务异常/tailnet 概况，大数字等宽）；② 节点卡（状态、角色、最近心跳、cpu/mem sparkline）；③ 受管服务清单（状态色点）；④ AI agent 清单（含 invokable 徽标恒 false 说明）；⑤ 页脚：数据新鲜度 + "read-only" 标识。
- 诚实检查：交付前按技能 creative-direction.md 的标准自问"这是不是又一个 SaaS 模板"，是则重做。

### 4.4 面板验收

`curl -s localhost:7700/ | grep glass` 有料；浏览器打开（隧道）人眼过一遍布局/动效/深色玻璃质感；`fetch('/api/panel/overview')` 返回聚合 JSON；非法 Host 403（`curl -H 'Host: evil.com' localhost:7700/` 验证）。

## 5. 观察点修复（部署验证转来）

1. `meshconsole --version` / `meshagent --version` 子命令（输出语义版本 + commit）；`meshconsole pki -h` 风格统一。
2. ldflags 版本注入修正：Makefile 把当前 commit/版本注入两二进制（部署验证时显示 `31eafac-dirty` 是 M1a 旧号），`-ldflags "-X main.version=..."`。
3. process 型采集对 agent 自身判 inactive：先读 `internal/agent/collect/services.go` 确认是否 exclude-self 设计——若是有意设计，代码注释写明 + DELIVERY 说明；若是缺陷，修复为能正确探测自进程（用 `/proc` 自 PID 排除而非名字排除，或等效方案），补单测。

## 6. 非目标

写操作/节点控制（M3a）、agent 调用（M3b）、告警通知（M2）、metrics 长期图表、认证登录（面板仅 localhost/隧道可达）、HTTP+SSE 传输 MCP（M3）。

## 7. 验收清单

- [ ] `go vet`/`gofmt`/`go test -count=1 ./...` 全绿；新增模块单测覆盖（MCP 三方法握手、tailnet 全量替换事务、Origin 校验拒绝/放行、panel overview 聚合）
- [ ] `make cross` 三平台过
- [ ] MCP：`echo '{"jsonrpc":"2.0","id":1,"method":"initialize",...}' | meshconsole mcp` 握手成功；tools/call list_nodes 返回部署库真数据
- [ ] Headscale：配真 API key 拉到节点（服务器上现成 headscale），断 key 路径优雅降级
- [ ] 面板：4.4 验收全过 + liquid glass 五要素（frost/atmosphere/token/排版/动效）在交付说明逐条自证
- [ ] 观察点三条闭环（代码或注释 + DELIVERY 说明）
- [ ] REVIEW.md 追加自测报告；DELIVERY.md 更新；**禁止 git commit**（审查通过后由流水线执行）
- [ ] 飞书同步由值班员按 STATE.md 执行
