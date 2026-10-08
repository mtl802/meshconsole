# SPEC-M1d · Agent 任务下发通道（L2 MVP）· v1 定稿

版本：v1（2026-10-08，draft 经 codex 评审修订定稿） · 前置：M1b-c2（账号/scope 表）交付后执行
Gate（伦哥定案）：仅 Mac mini + Windows 可下发；白名单只读命令；云节点禁止；命令协议基线 = DESIGN §4.2-D（引用已更正）。

## 1. 数据模型（migration v6）

- `commands`：command_id（uuid，**永不复用**）/node_id/kind/args_json/status/timeout_s/result_text(≤64KB)/exit_code/created_by/created_by_token_scope/claimed_at/finished_at/submission_key/archived_key/created_at/updated_at。
- status：pending → claimed（含 lease）→ running → succeeded|failed|unknown(degraded 标记) ；archived 为生命周期属性非独立态。
- `submission_key`：提交端幂等键（客户端生成），唯一索引——同 key 重复提交返回原 command_id；archived_key 仅归档该键（DESIGN 模式），**不归档 command_id**。
- `nodes` 加 `l2_allowed`（config 声明：mac-mini/windows true，cloud-nanjing false）；提交与 agent 领取/执行前**双重复核**。
- `api_tokens.scope`（readonly/operator，M1b-c2 表加列；旧 token 默认 readonly）；面板单管理员默认 operator；stdio MCP = readonly。

## 2. 领取与回执协议（pull + 租约 + ACK）

- 心跳响应携带 `commands: []`（该节点 pending 命令，≤3/次）；**领取原子性由 console 保证**：写入响应前置 claimed + claimed_at（lease 起点回 pending，重发至终态）。
- agent 收到后交**独立执行 worker**（新 goroutine，禁止阻塞心跳循环——runner.go 现状是采集→HTTP→休眠且丢弃响应体，本批改造），执行完本地持久化结果（结果目录 .pipeline 或 state 同目录），下轮心跳带 `command_results`。
- 回执确认：console 收结果置终态，下轮心跳响应带 `ack_ids`；agent 收到 ACK 才删本地持久化结果，未 ACK 持续重发（防丢回执）。
- **unknown 语义**：lease×2 或 timeout×1.5 到期无回执 → unknown（degraded 标记），非 failed——迟到回执若内容匹配（command_id + claimed_at 校验）→ 修正为实际终态并保留 degraded 历史。
- 结果归属验证：只接受本节点、当前 claimed（或 unknown 修正路径）命令的回执；拒绝无归属回执。
- 离线 pending 24h → archived（过期未领）。
- 提交配额：每节点 pending+claimed+running ≤5（事务内计数），超出 429。

## 3. 命令白名单（结构化 kind，禁止自由文本）

- **kind 结构化**（无自由 shell 串）：每 kind = 固定绝对路径 argv 模板 + 类型化参数槽；console 与 agent **双重校验**，agent 拒绝未知 kind。
- v1 kinds（平台矩阵）：

| kind | 平台 | argv 模板（固定） | 参数槽 |
|------|------|-------------------|--------|
| systemctl_status | linux | /usr/bin/systemctl status <unit> | unit：[a-zA-Z0-9_@.-]+ 且存在于 config 声明的受管服务集合 |
| journalctl_tail | linux | /usr/bin/journalctl -u <unit> -n <n> --no-pager | unit 同上；n：50-500 |
| ps_snapshot | linux/mac | /bin/ps aux | 无 |
| df_report | linux/mac | /bin/df -h | 无 |
| launchctl_list | mac | /bin/launchctl list | 无 |
| mac_log_show | mac | /usr/bin/log show --last <n>m --style compact | n：10-120 |
| docker_ps | linux | <docker_bin 只读包装> ps -a | 无 |

- 执行环境：固定最小 env（PATH 白名单目录、SYSTEMD_PAGER=cat/禁 pager）、固定工作目录、固定 argv 直接 exec（不经 shell）；**单横线选项也是模板固定部分，参数槽只填占位**——参数全串匹配字符集且限长 128。
- l2_extra_commands config 可扩展，但只能是"已审核 kind 的参数化实例"，不能引入新可执行路径。
- agent 端执行：timeout（默认 30s 上限 300s）+ 进程组 kill；stdout/stderr 合并采集 64KB 硬上限；agent 本地并发执行 ≤2。

## 3.5 会话活跃视图（伦哥 20:35 需求：能看到当前 agent 会话在干嘛）

- 痛点：进程级采集把常驻 daemon（codex app-server，elapsed 数天）误当任务，真正的活跃会话反而不可见。
- agent 侧新增：扫描已知 agent 会话目录（zcode: ~/.zcode/cli/rollout/，codex: ~/.codex/sessions/，含自定义 agent 同类目录），mtime < 30min 的会话文件为活跃会话；每个活跃会话读首行用户消息（主题，截断 120 字）+ 末行消息文本（当前动作，截断 120 字）——轻解析（逐行 json try/except 只抽 text 字段，格式变更时显示空不报错）。
- 心跳新可选字段 `agent_sessions`（三态语义同 services）：console 同批 migration 新表 `agent_sessions`（node_id/agent_name/session_file/started_at/last_activity/topic/recent_action/updated_at，按节点全量替换）。
- daemon 过滤：agent_tasks 采集时 elapsed > 1h 的进程标记 `background=true`，面板任务区不显示（数据保留）。
- 面板：任务区改「agent 会话活跃」——每卡显示 终端/agent/会话主题/当前动作/持续时长；无活跃会话显示「全部安静」。
- MCP：新只读工具 `list_active_sessions(node?)`（含 topic/recent_action）。

## 4. scope 与授权

- readonly：既有只读工具；operator：+ submit_command/get_command/list_commands（description 中文）。
- 所有入口（panel/MCP HTTP/stdio）统一服务端授权检查；用户 disable/停用 token → 其未完成命令保留但新建拒绝；**领取与执行前复核 l2_allowed 与 node 在线状态**。
- CSRF：控制操作仅走 MCP（Bearer）与面板同源表单（SameSite=Lax 覆盖），命令结果输出经 HTML 转义（沿用 esc，覆盖新视图）。

## 5. 审计与保留

- 每命令 AUDIT 日志（command_id/node/kind/args/created_by/scope/终态）；commands 表保留 90 天（每小时清理任务，随 metrics retention 模式）；审计不可改写（无 UPDATE result 之外的路径）。
- 平台能力协商：agent 心跳上报 `caps: ["linux-systemd"]` / `["mac-launchd"]`，console 下发时校验目标支持该 kind，不支持 400。

## 6. 非目标

主动 cancel、结果分页、沙箱（seatbelt/AppContainer，届时云节点解禁复议）、非白名单命令、长连接推送、Windows kind 实装（caps 预留 windows-*）。

## 7. 验收清单

- [ ] 单测：状态机全迁移含异常路径（lease 过期重发/unknown 修正/迟到回执/双实例 claim 竞态）、幂等键重复提交、白名单注入向量全拒（单横线注入/路径遍历/超长/非法 unit）、caps 不匹配拒绝、ACK 前 agent 重启恢复重发
- [ ] 真机：面板向 mac-mini 发 ps_snapshot → claimed → 结果中文面板展示；向 cloud-nanjing 提交 → 拒绝提示；readonly token 调 submit → 403
- [ ] 混沌场景：kill agent 半路/杀 console/丢心跳各一轮，最终状态可对账
- [ ] REVIEW.md 自测 + DELIVERY.md + DESIGN §4.2-D 实现注记；禁止 git commit
