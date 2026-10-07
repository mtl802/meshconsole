# MeshConsole M1b-a 任务书（SPEC-M1b-a）

> 依据 DESIGN.md v0.7。M1b 拆两批：本批（M1b-a）= TLS 传输加密 + 服务清单 + AI agent 发现；
> MCP Server / Headscale 集成 / Web 面板在 M1b-b。M1a 代码已入库（commit 31eafac）为基线。

## 目标

传输层收尾（HTTPS 全覆盖）+ agent 侧两大采集能力（受管服务状态、AI agent 发现），heartbeat 协议扩展落库。

## 范围（本批交付）

### 1. TLS 全覆盖（DESIGN §7-3）

- **CA 生命周期**：`make pki` 生成持久化 CA + 服务端证书（输出到配置目录 `pki/`，私钥 600）；
  SAN 必须包含：localhost、127.0.0.1、配置的 tailnet IP、主机名；**已存在则不重新生成**（幂等）
- console 启动改 HTTPS（listen 不变）；启动仅加载 pki，不重生
- agent 侧证书固定：配置 `ca_cert` 路径，TLS 握手验证服务端证书链 + 指纹（`fingerprint` 配置项，SHA-256 hex）；
  两者任一配置即启用验证；**不提供跳过验证的选项**
- 客户端证书（mTLS）本批不做，M3 再议
- **连接总数限额随 TLS 落地**（R5 裁决挪入本批）：使用 `golang.org/x/net/netutil.LimitListener`
  （现成正确实现，默认 256，可配；禁止手写 semaphore 包 listener——R5 已证明与 Shutdown 互锁）；
  配置项 `max_connections`
- 协议回归：既有 401/403/400 语义在 HTTPS 上全部保持；`drainHTTP` 优雅退出在 TLS listener 上工作

### 2. 受管服务清单（DESIGN §4.3、§7-5）

- agent 配置声明：
  ```yaml
  services:
    - name: rustdesk
      type: systemd      # systemd | docker | process
      target: rustdesk
  ```
- 状态查询实现：
  - systemd：`systemctl is-active <target>`（exec，固定 argv，target 白名单字符集 [a-zA-Z0-9_@.-]）
  - docker：`docker ps --filter name=<target> --format '{{.State}}'`（**只读 CLI 查询，不经 SDK 直连 socket**，
    DESIGN §7-5；docker 不存在/无 daemon → status=unavailable + 错误说明）
  - process：pgrep -f 计数（macOS/Windows 通用兜底）
- 每 15s 随心跳上报 `services: [{name, type, target, status, detail}]`；
  status ∈ active/inactive/failed/unavailable/unknown；查询失败报 unknown + 说明，**禁止编造**
- console 落库 `services` 表（DESIGN §5：node_id/name/type/target/status/detail/updated_at），
  同节点同名 UPSERT；服务消失于上报 → status=stale（不删行，保留历史）

### 3. AI agent 发现（DESIGN §4.2-A/B，登记为主）

- **内置已知清单** PATH 探测（默认：zcode, codex, claude, gemini, aider；可配置增删）：
  `exec.LookPath` 存在性 + `--version`（5s 超时）取版本；扫描周期 5 分钟（低频）
- **配置显式声明**（与 PATH 探测结果合并，声明优先）：
  ```yaml
  agent_scan:
    custom:
      - name: my-agent
        type: cli
        command: /abs/path/to/agent     # 必须绝对路径
        version_flag: "--version"
    services:                           # 服务型：端口探测登记（不可 invoke，DESIGN §1 v1 边界）
        - name: hana-agent
          port: 5800
  ```
- 上报 `agents: [{name, type, version, path, status}]`；console 落库 `ai_agents` 表
  （node_id/name/type/version/path/status/invokable/updated_at），invokable 恒 false（M1 无调用）
- 探测失败（version 超时/无执行权限）→ status=unavailable + detail，**不得把发现路径当可调用路径**

### 4. 协议与存储扩展

- heartbeat 请求体扩展 `services`/`agents` 数组（可选字段，缺省视为无变化不覆盖）；
  严格 JSON 与值域校验语义保持；全量替换语义：以本次上报为准 UPSERT
- 新表：`services`、`ai_agents`（migration 版本递增）；时间/节点索引
- config 校验：services.type 非法值拒绝启动；custom.command 相对路径拒绝启动

## 明确不做（本批）

MCP、Headscale 集成、Web 面板、探测引擎、告警、任何写操作/调用、mTLS、Windows 真机验证（列 M1b-b 上机项）。

## 验收标准

1. `make pki` 幂等生成 pki；二次运行不覆盖已有私钥
2. console HTTPS 启动，`curl --cacert pki/ca.crt https://127.0.0.1:7799/healthz` 通；
   不带 cacert 的验证失败（自签不被默认信任）
3. agent 配 ca_cert+fingerprint 上报成功；篡改 fingerprint 后 401/TLS 错误且日志不含密钥材料
4. LimitListener 生效：配置 max_connections: 3 时第 4 条并发连接排队（单测或实测说明）
5. macOS 本机声明 2 个 process 型服务 + 1 个不存在 systemd 服务：状态分别为 active/unavailable/unknown+说明
6. 本机 PATH 的 zcode、codex 被自动发现登记（版本号正确）；一个不存在的 CLI 不出现
7. 既有 M1a 验收不回退：43 用例全绿 + 新增用例；go vet/gofmt 干净；make cross 三平台
8. DELIVERY.md 更新（含 TLS 握手实测、services/agents 落库截图式数据）

## 交付物

代码 + Makefile pki 目标 + deploy 样例更新 + DELIVERY.md（验收逐条自测 + 已知限制）
