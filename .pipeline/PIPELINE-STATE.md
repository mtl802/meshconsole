# MeshConsole 流水线架构（2026-10-08 起）

推进职责已从 automation 值班员移交 tmux 常驻 worker，本文件仅作架构说明，不再是执行指令源。

## 三层职责

1. **tmux worker**（会话 mesh-worker，脚本 /Users/tl.m/Documents/HanaAgent/workSpace/tools2/pipeline-worker.sh）
   - 状态机文件：.pipeline/worker-state（idle/verify/review/wait_adj/commit/verify_failed）
   - 日志：.pipeline/worker.log；心跳：tools2/pipeline-events/worker.heartbeat（30s）
   - 流程：events.log 出现 done 且源静止 → 验证（verify-*.out）→ codex 审查（review-*.out）→ 置 NEED-ADJUDICATION 停等 → 收到 ADJUDICATED → 按 COMMIT_OK 决定 commit → 回 idle
2. **automation 值班员**（studio_job_8，每 8 分钟）
   - 保活（心跳 >180s 重启 worker）、裁决（读 review-*.out 按 REVIEW.md 口径判）、巡检补漏、升级 notify
3. **Hana 主会话**
   - 任务书编写、部署上机验证（真 headscale/浏览器人眼）、兜底推进、验收答复

## 智能节点约定

worker 遇到需要判断的事（审查意见采纳、commit 放行、失败处置）一律停下置标志文件，等值班员/主会话裁决后继续。裁决产物：REVIEW.md 裁决节 + ADJUDICATED 标志（+ COMMIT_OK 视情况）。

## 历史注记

- 旧"值班员单步协议"（步骤队列）已废弃：单轮会话扛不住长活，由 worker 状态机替代
- R12 教训（审移动工作区）已固化为 worker 的 source_still 检查；R5 教训（手写 listener）在 DESIGN.md
- 云上部署验证（真 headscale、浏览器人眼面板）固定由主会话在 commit 后执行
