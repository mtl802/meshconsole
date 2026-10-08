# SPEC-M1b-c · 亮色主题 + Agent 中台（agent 任务监测）

版本：v1（2026-10-08） · 前置：M1b-b 已交付（da1e1ad）
定位升级（伦哥 13:36 指示）：MeshConsole = **agent 中台**，不止展示终端，还要监测组网内各终端上的 agent 与其正在执行的任务。

## 1. 亮色主题（伦哥 13:36 反馈 #1）

- 面板切换为**亮色为主**：light glass 落地（技能 theming/liquid-glass references 口径：亮色下 frost 要**更薄、更饱和**，背景 atmosphere 换亮色 mesh）。
- token 系统保持"三 RGB 三元组派生全站色板"，换亮色三元组；深色实现保留为暗色变量块（TODO 注释标 M1c+ 可切换），默认亮色。
- 亮色下验证文字对比度（等宽字数据、状态点、offline 红在白底可读）。

## 2. Agent 任务监测（数据链路，本次核心）

### 2.1 agent 侧采集（internal/agent/collect 新增 agenttasks collector）

- 进程扫描：按 agentdisc 的已知清单（known 默认五项 + config custom 并集）匹配本机进程（unix：`ps -eo pid,etime,pcpu,pmem,command`；Windows 顺带 `tasklist` 兜底，失败置 collect_errors 不致命）。
- 每个匹配进程抓取：pid / agent_name（匹配到的 CLI 名）/ cmd 截断 200 字符 / elapsed_s / cpu_pct / mem_pct / started_at（由 etime 反推）。
- 最近活动（轻量辅证）：对 known/custom 各 agent 的会话目录（如 zcode 的 `~/.zcode/cli/rollout/`、codex 的 `~/.codex/sessions/`）做 stat，取最近 mtime 与文件数 → `last_activity`，**不读取不解析内容**（格式耦合禁止）。目录缺失置 null。
- 随心跳上报（15s），数组装进 heartbeat 新顶层可选字段 `agent_tasks`（缺省 = 无变化，沿用三态语义；空数组 = 清空该节点任务）。

### 2.2 console 侧

- migration v4：新表 `agent_tasks`（node_id/pid/agent_name/cmd/elapsed_s/cpu_pct/mem_pct/started_at/updated_at），心跳按节点全量替换（进程消失 = 任务结束，不留历史行；历史留存属 M1c+ 任务档案，本期不做）。
- `/api/panel/overview` 聚合扩展：每节点 `running_tasks`（agent_tasks）+ `agents`（发现清单带 last_activity）。
- MCP 新增只读工具 `list_agent_tasks(node?)`。

### 2.3 面板（agent 中台信息架构）

按中台重构（保持 Liquid Glass、换亮色）：

- **顶栏**：mesh 总览（在线终端/运行中任务总数/服务异常/tailnet 在线）大数字。
- **运行中的 agent 任务**（核心区，大卡优先）：跨终端聚合的任务卡流——每卡显示 终端名 / agent 名 / cmd 摘要 / elapsed（活动计时）/ cpu；无任务时显示"全部安静"空态（这本身是信息）。
- **终端区**：每终端一张卡（状态、角色、cpu/mem sparkline、该终端 agent 清单 + last_activity）。
- **服务与 tailnet**：保留 M1b-b 两区块（亮色化）。
- 交付自证五要素（frost/atmosphere/token/排版/动效）在亮色下的适配说明。

## 3. 非目标

任务历史档案与统计（M1c+）、agent 调用/编排（M3）、告警（M2）、Windows 任务采集精细化（先 tasklist 兜底）、tailnet ips=null 修复（转本批顺手修，见 §4）。

## 4. 顺手修（M1b-b 遗留观察点）

- tailnet ips=null：查 headscale `/api/v1/node` 响应中可用地址字段（available_routes/ips 命名差异），正确解析入库。
- 已知：无。

## 5. 验收清单

- [ ] vet/gofmt/go test 全绿（新 collector 单测：进程匹配解析/etime 反推/三态语义；migration v4；list_agent_tasks）+ cross 三平台
- [ ] 真机：cloud-agent 上报 agent_tasks；本机手动起一个 `sleep 300` 伪装进程验证匹配与消失清行
- [ ] 面板亮色 + 中台三区块，人眼验收（伦哥）
- [ ] MCP list_agent_tasks 返回真数据
- [ ] REVIEW.md 自测节 + DELIVERY.md 更新；禁止 git commit
