// usercmd.go 实现 `meshconsole user` 与 `meshconsole token` 子命令
// （SPEC-M1b-c2 §1）。CLI 先跑 migration 再操作（store.Open 即执行——
// bootstrap 不依赖既有数据）；stdio MCP 仍走 OpenReadOnly 不迁移。
//
// 用法：
//
//	meshconsole user add <用户名>                        交互输入口令（两遍）
//	meshconsole user passwd <用户名>                     改密（撤销该用户全部会话与 token）
//	meshconsole user disable <用户名>                    禁用（最后一个启用用户拒绝；撤销会话与 token）
//	meshconsole user enable <用户名>                     启用
//	meshconsole token create <用户名> [-desc 文本] [-expires RFC3339] [-scope readonly|operator]  签发 API token（明文只显示一次）
//	meshconsole token list                              列出全部 API token
//	meshconsole token revoke <id>                       吊销指定 API token
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/mtl802/meshconsole/internal/auth"
	"github.com/mtl802/meshconsole/internal/config"
	"github.com/mtl802/meshconsole/internal/store"
)

// userUsage 为 user/token 子命令的帮助文本（stderr，退出码 2）。
const userUsage = `用法:
  meshconsole user add <用户名>                    创建用户（交互输入口令，两遍确认）
  meshconsole user passwd <用户名>                 修改口令（撤销该用户全部会话与 API token）
  meshconsole user disable <用户名>                禁用用户（最后一个启用用户拒绝；撤销全部会话与 API token）
  meshconsole user enable <用户名>                 启用用户
  meshconsole token create <用户名> [-desc 文本] [-expires RFC3339] [-scope readonly|operator]
                                                   签发 MCP API token（明文仅显示一次，请立即保存；scope 缺省 readonly）
  meshconsole token list                           列出全部 API token（不含明文）
  meshconsole token revoke <id>                    按 id 吊销 API token
`

// openStoreForAdmin 载入配置并打开可写库（CLI 管理动作共用入口）：
// store.Open 内先跑 migration（SPEC §1：CLI 先跑 migration 再操作），并执行
// 数据文件权限收紧（0700/0600）。
func openStoreForAdmin(cfgPath string) (*store.Store, error) {
	cfg, err := config.LoadConsoleForPKI(cfgPath)
	if err != nil {
		return nil, err
	}
	return store.Open(cfg.DBPath)
}

// readPasswordTwice 交互读取口令并要求两遍一致；stdin 非终端时报错
// （runbook 口径：user add/passwd 均为交互密码）。
func readPasswordTwice(prompt string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("需要交互终端输入口令（本命令不支持管道/参数传口令）")
	}
	fmt.Fprint(os.Stderr, prompt)
	raw1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	fmt.Fprint(os.Stderr, "再次输入口令: ")
	raw2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	pw1, pw2 := string(raw1), string(raw2)
	if pw1 != pw2 {
		return "", fmt.Errorf("两次输入的口令不一致")
	}
	return pw1, nil
}

// cmdUser 处理 `meshconsole user <add|passwd|disable|enable>`。
func cmdUser(args []string, cfgPath string, readPw func(string) (string, error)) int {
	// R35-#3：位置参数数复核——各 user 子命令只接受 <用户名> 一个位置参数，
	// 多余即用法错误退出 2。-config 由入口层提取（值不属于子命令位置参数），
	// 复核前剥离。校验在打开库之前完成：零副作用退出。
	rest := stripConfigFlag(args)
	if len(rest) < 2 {
		fmt.Fprint(os.Stderr, userUsage)
		return 2
	}
	if len(rest) > 2 {
		fmt.Fprintf(os.Stderr, "错误：user %s 仅接受一个位置参数 <用户名>，收到多余参数 %v；未执行任何变更\n", rest[0], rest[2:])
		return 2
	}
	action, name := rest[0], rest[1]
	if strings.TrimSpace(name) == "" || len(name) > 64 {
		fmt.Fprintln(os.Stderr, "错误：用户名不得为空且 ≤64 字节")
		return 1
	}
	st, err := openStoreForAdmin(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()

	switch action {
	case "add":
		// 先查重再提示输入口令：不让用户白输一遍。
		if existing, err := st.GetUserByName(ctx, name); err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		} else if existing != nil {
			fmt.Fprintf(os.Stderr, "错误：用户 %q 已存在\n", name)
			return 1
		}
		pw, err := readPw("设置口令: ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		if _, err := st.CreateUser(ctx, name, hash); err != nil {
			fmt.Fprintf(os.Stderr, "错误：创建用户失败：%v\n", err)
			return 1
		}
		fmt.Printf("用户 %q 已创建（enabled）。接下来执行 `meshconsole token create %s` 签发 MCP API token。\n", name, name)
	case "passwd":
		u, err := st.GetUserByName(ctx, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		if u == nil {
			fmt.Fprintf(os.Stderr, "错误：用户 %q 不存在\n", name)
			return 1
		}
		pw, err := readPw("新口令: ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		if err := st.ChangeUserPassword(ctx, u.ID, hash); err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		fmt.Printf("用户 %q 口令已更新；该用户全部会话与 API token 已撤销（需重新登录、重新签发 token）。\n", name)
	case "disable", "enable":
		u, err := st.GetUserByName(ctx, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		if u == nil {
			fmt.Fprintf(os.Stderr, "错误：用户 %q 不存在\n", name)
			return 1
		}
		enable := action == "enable"
		if u.Enabled == enable {
			fmt.Printf("用户 %q 已经是%s状态，未变更。\n", name, map[bool]string{true: "启用", false: "禁用"}[enable])
			return 0
		}
		if err := st.SetUserEnabled(ctx, u.ID, enable); err != nil {
			if err == store.ErrLastEnabledUser {
				fmt.Fprintln(os.Stderr, "错误：不能禁用最后一个启用用户（公网形态下将无人可登录）；请先创建并启用其他用户")
			} else {
				fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			}
			return 1
		}
		if enable {
			fmt.Printf("用户 %q 已启用。\n", name)
		} else {
			fmt.Printf("用户 %q 已禁用；该用户全部会话与 API token 已撤销。\n", name)
		}
	default:
		fmt.Fprint(os.Stderr, userUsage)
		return 2
	}
	return 0
}

// cmdToken 处理 `meshconsole token <create|list|revoke>`。
func cmdToken(args []string, cfgPath string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, userUsage)
		return 2
	}
	st, err := openStoreForAdmin(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()

	switch args[0] {
	case "create":
		// -config 已由 configFlag 提取（cfgPath 参数），传给自己的 flag 集合
		// 前剥离——两套 flag 集合字段不同，混传会报 unknown flag。剥离后的
		// rest：[create, <用户名>, flags...]。
		rest := stripConfigFlag(args)
		if len(rest) < 2 {
			fmt.Fprint(os.Stderr, "用法: meshconsole token create <用户名> [-desc 文本] [-expires RFC3339] [-scope readonly|operator]\n")
			return 2
		}
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		desc := fs.String("desc", "", "token 用途说明（如：HanaAgent Mac）")
		expires := fs.String("expires", "", "到期时刻 RFC3339（如 2026-12-31T23:59:59Z；缺省长期有效）")
		scope := fs.String("scope", "readonly", "token 权限：readonly（只读查询）| operator（+ submit_command 等写工具，SPEC-M1d §4；缺省 readonly）")
		// 解析失败必须报错退出（R33-#5）：-expires 缺值/拼错若被忽略，
		// 会把「本应有期」的 token 签成长期有效。
		if err := fs.Parse(rest[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "错误：参数解析失败，未签发任何 token（-expires 需要形如 2026-12-31T23:59:59Z 的值；-desc 需要文本值；-scope 取 readonly|operator）")
			return 2
		}
		switch *scope {
		case "readonly", "operator":
		default:
			fmt.Fprintf(os.Stderr, "错误：-scope 须为 readonly|operator，收到 %q；未签发任何 token\n", *scope)
			return 2
		}
		// R35-#3：解析成功后复核剩余位置参数——flag 包遇首个位置参数即停，
		// `token create <用户名> extra -expires …` 会把 -expires 连同 extra
		// 一起当位置参数静默忽略，随后照签长期 token。多余即退出 2、零签发。
		if fs.NArg() != 0 {
			fmt.Fprintf(os.Stderr, "错误：token create 仅接受一个位置参数 <用户名>，收到多余参数 %v；未签发任何 token\n", fs.Args())
			return 2
		}
		username := rest[1]
		u, err := st.GetUserByName(ctx, username)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		if u == nil {
			fmt.Fprintf(os.Stderr, "错误：用户 %q 不存在（先 `meshconsole user add %s`）\n", username, username)
			return 1
		}
		if !u.Enabled {
			fmt.Fprintf(os.Stderr, "错误：用户 %q 处于禁用状态，请先 enable\n", username)
			return 1
		}
		var expAt *int64
		if *expires != "" {
			t, perr := time.Parse(time.RFC3339, *expires)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "错误：-expires 须为 RFC3339（如 2026-12-31T23:59:59Z）\n")
				return 1
			}
			if !time.Now().Before(t) {
				fmt.Fprintln(os.Stderr, "错误：-expires 时刻已过去")
				return 1
			}
			v := t.Unix()
			expAt = &v
		}
		plain, err := auth.NewAPIToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：生成 token 失败：%v\n", err)
			return 1
		}
		id, err := st.CreateAPIToken(ctx, apiTokenHash(plain), u.ID, *desc, expAt, *scope)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：写入 token 失败：%v\n", err)
			return 1
		}
		fmt.Printf("API token #%d 已签发给用户 %q（scope=%s，明文仅显示一次，请立即保存）：\n\n  %s\n\n", id, username, *scope, plain)
		if expAt != nil {
			fmt.Printf("到期时刻：%s\n", time.Unix(*expAt, 0).Format(time.RFC3339))
		} else {
			fmt.Println("到期时刻：长期有效（可用 token revoke 吊销）")
		}
		fmt.Println("MCP 客户端用法：Authorization: Bearer <token>，端点 https://<console>:7700/mcp")
	case "list":
		// R35-#3：token list 不接受位置参数，多余即用法错误（args[0] 恒为
		// "list"，剥离 -config 后至少剩 1 个元素，len != 1 即存在多余参数）。
		if rest := stripConfigFlag(args); len(rest) != 1 {
			fmt.Fprintf(os.Stderr, "错误：token list 不接受位置参数，收到多余参数 %v\n", rest[1:])
			return 2
		}
		rows, err := st.ListAPITokens(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		if len(rows) == 0 {
			fmt.Println("当前没有任何 API token。")
			return 0
		}
		fmt.Printf("%-6s %-16s %-10s %-24s %-20s %s\n", "ID", "用户", "SCOPE", "说明", "创建时刻", "到期时刻")
		for _, r := range rows {
			exp := "长期"
			if r.ExpiresAt.Valid {
				exp = time.Unix(r.ExpiresAt.Int64, 0).Format(time.RFC3339)
			}
			fmt.Printf("%-6d %-16s %-10s %-24s %-20s %s\n", r.ID, r.Username, r.Scope, truncateDesc(r.Description, 24),
				time.Unix(r.CreatedAt, 0).Format("2006-01-02 15:04"), exp)
		}
	case "revoke":
		// R35-#3：token revoke 只接受 <id> 一个位置参数，多余即用法错误
		// （与 <2 缺参同口径退出 2）。
		rest := stripConfigFlag(args)
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "用法: meshconsole token revoke <id>（id 见 token list）")
			return 2
		}
		if len(rest) > 2 {
			fmt.Fprintf(os.Stderr, "错误：token revoke 仅接受一个位置参数 <id>，收到多余参数 %v；未吊销任何 token\n", rest[2:])
			return 2
		}
		var id int64
		if _, err := fmt.Sscanf(rest[1], "%d", &id); err != nil || id <= 0 {
			fmt.Fprintln(os.Stderr, "错误：id 须为正整数（token list 查看）")
			return 1
		}
		ok, err := st.RevokeAPIToken(ctx, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 1
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "错误：API token #%d 不存在（可能已吊销）\n", id)
			return 1
		}
		fmt.Printf("API token #%d 已吊销，持有方将立即 401。\n", id)
	default:
		fmt.Fprint(os.Stderr, userUsage)
		return 2
	}
	return 0
}

// configFlagPos 返回 args 中 -config/--config 项的下标与其值的下标：分离值形态
// （-config 值）值在 i+1；**等号形态（-config=值）值内嵌于该项本身，返回 (i, i)**
// ——stripConfigFlag 据此只剥离该项本身、保留其后全部参数（R37 候选/R39-#5：
// 旧写法对等号形态返回 (i,-1)，把 -config= 之后的 -expires、-scope 与多余位置
// 参数一并静默丢弃，NArg 复核与 flag 解析全被绕过）。值缺失（末尾孤 -config）
// 返回 (i,-1)：其后已无参数可保留。无 -config 返回 (-1,-1)。
func configFlagPos(args []string) (int, int) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-config" || args[i] == "--config":
			if i+1 < len(args) {
				return i, i + 1
			}
			return i, -1
		case strings.HasPrefix(args[i], "-config="), strings.HasPrefix(args[i], "--config="):
			return i, i
		}
	}
	return -1, -1
}

// stripConfigFlag 剥离 -config 项（含值），供子命令自有 flag 集合解析余参。
func stripConfigFlag(args []string) []string {
	a, b := configFlagPos(args)
	if a < 0 {
		return args
	}
	if b < 0 {
		out := append([]string{}, args[:a]...)
		return out
	}
	out := append([]string{}, args[:a]...)
	return append(out, args[b+1:]...)
}

// truncateDesc 截断说明列宽（CLI 表格展示用）。
func truncateDesc(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

// apiTokenHash 与 auth.Manager 落库同口径（SHA-256 hex）。
func apiTokenHash(token string) string {
	return auth.HashToken(token)
}
