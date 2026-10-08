# MeshConsole 开发经验与陷阱（自动加载，每次开发/审查前必读）

本文件是项目错误模式的沉淀（来源：R10-R37 审查轮、部署验证、真实用户反馈）。
规则：动手前扫一遍相关条目；交付前跑一遍"自查命令"；发现新错误模式由流水线追加本文件。

## 一、面板 CSS/前端（历史事故：登录按钮透明、状态竖排）

1. **禁止 `var(--token, rgb(...))` 直接当颜色**：本项目 token 值是裸三元组（`--primary: 4 120 87`），
   直接当颜色是非法值 → 声明静默失效（按钮透明这种事故就是这么来的）。
   正确写法：`rgb(var(--primary, 4 120 87))`。
   自查：`grep -nE 'var\(--[a-z-]+, rgb' internal/panel/web/*.html` 应为 0。
2. **update 类函数会重写整个 className**（如 setDotText：`el.className = 'dot-text mono' + ...`）——
   构造时加的类会被每次心跳抹掉。需要常驻的样式约束：直接用 CSS 锁最终类
   （如 `.row-services > .dot-text { white-space: nowrap; grid-column: 5; }`），
   或改 setDotText 保留原类。禁止依赖"构造时加的类"存活。
3. **CJK 文本在窄 grid 列会逐字竖排**：状态/详情/标签列必须 `white-space: nowrap` +
   合理列宽（auto 或独占全宽行）。手机 390px 与桌面 1400px 两个宽度必须各自验证，
   禁止只在桌面宽度确认。
4. **静态资源引用必须带版本参数**：`glass.css?v=N` / `app.js?v=N`，每次改动内容 bump N
   （用户浏览器强缓存会让"改了没生效"变成事故）。当前版本见 index.html / login.html。
5. **内部开发注释不得出现在用户 UI**（panel-foot 等）——用户文案说人话。

## 二、Go 代码与 CLI

6. **CLI 子命令必须位于 argv[1]**：`meshconsole user add` 合法，
   `meshconsole -config x user add` 会静默回退 serve——子命令分发用 os.Args[1]，
   全局 flag 在子命令参数内提取（configFlag 模式）。
7. **ldflags 版本注入**：Makefile 的 VERSION 变量语义化（如 m1b-c2），bin 内 version =
   `<语义版本>+git_<短hash>`；make cross 后必须 `strings bin/xxx | grep <hash>` 确认注入生效
   （embed/编译缓存会静默产出旧内容，mtime 不变即未重编）。
8. **CLI 口令参数**：交互口令命令不支持管道/参数传口令（安全设计）——自动化部署用
   `printf 'pw\npw\n' | sudo script -qec './bin/meshconsole user add lunge' /dev/null`（script 分配真 pty）。

## 三、诚实性红线（历史事故：DELIVERY 夸大）

9. **禁止声明"全部通过"除非验证矩阵逐项跑完**；未验证项（真机连测、人眼验收、
   特定平台）必须显式列出"部署上机项"——夸大交付状态是比代码 bug 更严重的错误。
10. **测试掩蔽缺陷**（历史事故：services_test 用"退出码 0 的 mock"掩盖状态误报）——
    写测试时先问"这个 mock 会不会把 bug 洗白"；失败路径必须有断言。

## 四、已知设计决策（不要"修复"它们）

- agent 自进程排除是有意设计（selfMatches，观察点③）——agent 不能靠监测自己证明自己活着。
- cancel 是 L2-only；L1 命令走超时兜底。
- 云节点（config l2_allowed=false）禁止命令下发，提交与执行前双重复核。

## 五、审查侧注意（codex）

- 除安全/协议逻辑外，必须覆盖：CSS 变量用法、JS 类覆盖模式、窄屏断点、
  静态资源缓存策略、CLI 参数顺序、版本注入、错误文案语言。
- 对 DELIVERY 的"全部通过"保持怀疑：抽 2-3 项验证矩阵条目要求出示证据。
- 手机窄屏（390px）与桌面（1400px）是两个独立验收宽度。
