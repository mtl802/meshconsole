// config_acl_test.go 为 Windows ACL 判定核心的测试（R33-#2、R35-#1）：判定
// 函数 aclVerdict 与 ACE 分类器 classifyDACLACE/daclAllowTrustees 均为平台
// 无关纯函数，错误路径在任意平台可测。Windows 侧的 GetNamedSecurityInfo/ACE
// 枚举属薄封装，真机核验列部署上机项。
package config

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

const (
	sidSelf  = "S-1-5-21-1-1000"
	sidAdmin = "S-1-5-32-544"
	sidSys   = "S-1-5-18"
	sidAll   = "S-1-1-0"      // Everyone
	sidUsers = "S-1-5-32-545" // BUILTIN\Users
)

func trustedSIDs() []string { return []string{sidSelf, sidAdmin, sidSys} }

// TestACLVerdictPass 属主合规 + 全部 allow ACE trustee 落在可信集 → 放行
// （典型安全形态：继承已裁剪，仅运行用户/SYSTEM/Administrators 三条 ACE）。
func TestACLVerdictPass(t *testing.T) {
	for _, trustees := range [][]string{
		{sidSelf, sidSys, sidAdmin},
		{sidSelf},                    // 仅运行用户
		{sidSys, sidAdmin},           // 属主为管理员组时的典型形态
		{},                           // 零 allow ACE：无人有访问权，不构成放行缺口
		{sidSelf, sidSelf, sidAdmin}, // 重复条目同样放行
	} {
		if err := aclVerdict("console.yaml", true, trustees, trustedSIDs()); err != nil {
			t.Fatalf("trustees %v: verdict = %v, want nil", trustees, err)
		}
	}
}

// TestACLVerdictRejectsOwner 属主既非当前用户也非管理员组 → 拒绝。
func TestACLVerdictRejectsOwner(t *testing.T) {
	err := aclVerdict("console.yaml", false, []string{sidSelf}, trustedSIDs())
	if err == nil || !strings.Contains(err.Error(), "属主") {
		t.Fatalf("bad owner verdict = %v, want 属主 rejection", err)
	}
}

// TestACLVerdictRejectsOutsideTrustee 受信账户之外的 ACE（Everyone/Users 等典型
// 过宽授权）→ 拒绝且 remediation 含 icacls 指引。
func TestACLVerdictRejectsOutsideTrustee(t *testing.T) {
	for _, trustee := range []string{sidAll, sidUsers, "S-1-5-21-1-1001"} {
		err := aclVerdict("console.yaml", true, []string{sidSelf, trustee}, trustedSIDs())
		if err == nil {
			t.Fatalf("trustee %s: verdict = nil, want rejection", trustee)
		}
		if !strings.Contains(err.Error(), "icacls") {
			t.Fatalf("trustee %s: no remediation in %v", trustee, err)
		}
	}
}

// encodeSID 把 "S-1-5-18-…" 形态的 SID 字符串编码为 NT 线格式字节（测试辅助：
// Revision 取第二段，IdentifierAuthority 取第三段，SubAuthority 小端 u32）。
func encodeSID(t *testing.T, s string) []byte {
	t.Helper()
	parts := strings.Split(s, "-")
	if len(parts) < 3 || parts[0] != "S" {
		t.Fatalf("bad sid string %q", s)
	}
	rev, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("bad revision in %q: %v", s, err)
	}
	auth, err := strconv.ParseUint(parts[2], 10, 48)
	if err != nil {
		t.Fatalf("bad identifier authority in %q: %v", s, err)
	}
	b := []byte{byte(rev), byte(len(parts) - 3)}
	for shift := 40; shift >= 0; shift -= 8 {
		b = append(b, byte(auth>>shift))
	}
	for _, p := range parts[3:] {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			t.Fatalf("bad subauthority in %q: %v", s, err)
		}
		var w [4]byte
		binary.LittleEndian.PutUint32(w[:], uint32(v))
		b = append(b, w[:]...)
	}
	return b
}

// decodeSID 把 NT 线格式 SID 字节解回 "S-1-…" 字符串（测试辅助，与
// classifyDACLACE 的提取区间配对使用）。
func decodeSID(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 8 || len(b)%4 != 0 {
		t.Fatalf("bad raw sid bytes %x", b)
	}
	auth := uint64(0)
	for i := 2; i < 8; i++ {
		auth = auth<<8 | uint64(b[i])
	}
	out := fmt.Sprintf("S-%d-%d", b[0], auth)
	for i := 8; i+4 <= len(b); i += 4 {
		out += fmt.Sprintf("-%d", binary.LittleEndian.Uint32(b[i:i+4]))
	}
	return out
}

// buildACE 按 winnt.h 布局组装一条 ACE 原始字节（测试辅助）：header + mask
// （GENERIC_ALL）+ OBJECT 型的 Flags/GUID 体 + trustee SID + 尾随 callback
// 数据（extra 字节，模拟 ACCESS_ALLOWED_CALLBACK_ACE 的回调段）。
func buildACE(t *testing.T, aceType, aceFlags byte, sid []byte, objectFlags uint32, extra int) []byte {
	t.Helper()
	isObject := aceType == aceTypeAccessAllowedObject || aceType == aceTypeAccessAllowedCallbackObject ||
		aceType == aceTypeAccessDeniedObject || aceType == aceTypeAccessDeniedCallbackObject
	total := aceHeaderLen + aceMaskLen + len(sid) + extra
	if isObject {
		total += aceObjectFlagsLen
		if objectFlags&aceObjectFlagObjectType != 0 {
			total += aceObjectGUIDLen
		}
		if objectFlags&aceObjectFlagInheritedObjectType != 0 {
			total += aceObjectGUIDLen
		}
	}
	ace := make([]byte, total)
	ace[0], ace[1] = aceType, aceFlags
	binary.LittleEndian.PutUint16(ace[2:4], uint16(total))
	binary.LittleEndian.PutUint32(ace[4:8], 0x10000000) // GENERIC_ALL
	if isObject {
		binary.LittleEndian.PutUint32(ace[8:12], objectFlags)
		off := aceHeaderLen + aceMaskLen + aceObjectFlagsLen
		if objectFlags&aceObjectFlagObjectType != 0 {
			off += aceObjectGUIDLen // GUID 字节内容对判定无关，留零
		}
		if objectFlags&aceObjectFlagInheritedObjectType != 0 {
			off += aceObjectGUIDLen
		}
		copy(ace[off:], sid)
	} else {
		copy(ace[aceHeaderLen+aceMaskLen:], sid)
	}
	return ace
}

// TestClassifyDACLACEAllowTypes 四类允许型 ACE（普通/callback/object/
// callback-object；OBJECT 型含 0–2 个 GUID 形态；callback 型含/不含尾随回调
// 数据）trustee 全部可解析且与原始 SID 一致；藏身 callback/object 允许型的
// 不可信 trustee（Everyone）必须进入可信集判定并被拒——R35-#1 阻塞场景在
// 纯函数层钉死。
func TestClassifyDACLACEAllowTypes(t *testing.T) {
	sidSelfB := encodeSID(t, sidSelf)
	sidAllB := encodeSID(t, sidAll)
	objFlagForms := []uint32{0, aceObjectFlagObjectType,
		aceObjectFlagInheritedObjectType, aceObjectFlagObjectType | aceObjectFlagInheritedObjectType}
	for _, tc := range []struct {
		name    string
		aceType byte
	}{
		{"plain", aceTypeAccessAllowed},
		{"callback", aceTypeAccessAllowedCallback},
		{"object", aceTypeAccessAllowedObject},
		{"callback-object", aceTypeAccessAllowedCallbackObject},
	} {
		flagForms := []uint32{0}
		if tc.aceType == aceTypeAccessAllowedObject || tc.aceType == aceTypeAccessAllowedCallbackObject {
			flagForms = objFlagForms
		}
		for _, of := range flagForms {
			for _, trailer := range []int{0, 7} {
				ace := buildACE(t, tc.aceType, 0, sidSelfB, of, trailer)
				v, s, e := classifyDACLACE(ace)
				if v != aceTrusteeAllow {
					t.Fatalf("%s objFlags=0x%x trailer=%d: verdict = %d, want allow", tc.name, of, trailer, v)
				}
				if got := decodeSID(t, ace[s:e]); got != sidSelf {
					t.Fatalf("%s objFlags=0x%x: trustee = %s, want %s", tc.name, of, got, sidSelf)
				}
			}
		}
	}
	// 阻塞场景：callback/object 允许型藏 Everyone——此前被跳过即绕过启动检查。
	for _, tc := range []struct {
		name    string
		aceType byte
	}{
		{"callback", aceTypeAccessAllowedCallback},
		{"object", aceTypeAccessAllowedObject},
		{"callback-object", aceTypeAccessAllowedCallbackObject},
	} {
		ace := buildACE(t, tc.aceType, 0, sidAllB, objFlagForms[3], 0)
		v, s, e := classifyDACLACE(ace)
		if v != aceTrusteeAllow {
			t.Fatalf("hostile %s: verdict = %d, want allow(then reject)", tc.name, v)
		}
		trustee := decodeSID(t, ace[s:e])
		if err := aclVerdict("console.yaml", true, []string{sidSelf, trustee}, trustedSIDs()); err == nil {
			t.Fatalf("hostile %s trustee %s bypassed aclVerdict", tc.name, trustee)
		}
	}
}

// TestClassifyDACLACEDenyIgnored 四类拒绝型一律 aceDenyIgnore：拒绝型只能收权、
// 不能授权，不参与放行判定——trustee 是谁都一样（含 Everyone）。
func TestClassifyDACLACEDenyIgnored(t *testing.T) {
	sidAllB := encodeSID(t, sidAll)
	for _, tc := range []struct {
		name    string
		aceType byte
	}{
		{"plain", aceTypeAccessDenied},
		{"object", aceTypeAccessDeniedObject},
		{"callback", aceTypeAccessDeniedCallback},
		{"callback-object", aceTypeAccessDeniedCallbackObject},
	} {
		of := uint32(0)
		if tc.aceType == aceTypeAccessDeniedObject || tc.aceType == aceTypeAccessDeniedCallbackObject {
			of = aceObjectFlagObjectType | aceObjectFlagInheritedObjectType
		}
		ace := buildACE(t, tc.aceType, 0, sidAllB, of, 0)
		if v, _, _ := classifyDACLACE(ace); v != aceDenyIgnore {
			t.Fatalf("deny %s: verdict = %d, want deny-ignore", tc.name, v)
		}
	}
}

// TestClassifyDACLACEInheritOnly INHERIT_ONLY 的 ACE 不作用于本对象，与类型
// 无关跳过（含携带不可信 trustee 的允许型——它只遗传给子项、不构成本对象的
// 访问面）。
func TestClassifyDACLACEInheritOnly(t *testing.T) {
	sidAllB := encodeSID(t, sidAll)
	for _, aceType := range []byte{aceTypeAccessAllowed, aceTypeAccessAllowedCallback,
		aceTypeAccessAllowedObject, aceTypeAccessAllowedCallbackObject} {
		ace := buildACE(t, aceType, aceFlagInheritOnly, sidAllB, aceObjectFlagObjectType, 0)
		if v, _, _ := classifyDACLACE(ace); v != aceInheritOnlyIgnore {
			t.Fatalf("type=0x%02x inherit-only: verdict = %d, want inherit-only-ignore", aceType, v)
		}
	}
}

// TestClassifyDACLACEUnparsable 未知类型（系统 SACL 类/保留/未来类型）与各类
// 解析失败（尺寸撒谎、SID 区间越界）一律 aceUnparsableFail——由封装转为拒绝
// 启动，不存在「看不懂就跳过」的路径。
func TestClassifyDACLACEUnparsable(t *testing.T) {
	sidSelfB := encodeSID(t, sidSelf)
	// 未知/不可判定类型：系统审计(0x02)、系统 Mandatory Label(0x11)、保留 0x04、
	// 未来类型 0x77。
	for _, aceType := range []byte{0x02, 0x11, 0x04, 0x77} {
		ace := buildACE(t, aceType, 0, sidSelfB, 0, 0)
		if v, _, _ := classifyDACLACE(ace); v != aceUnparsableFail {
			t.Fatalf("type=0x%02x: verdict = %d, want fail", aceType, v)
		}
	}
	mustFail := func(name string, ace []byte) {
		t.Helper()
		if v, _, _ := classifyDACLACE(ace); v != aceUnparsableFail {
			t.Fatalf("%s: verdict = %d, want fail", name, v)
		}
	}
	// 短于 header+mask；AceSize 撒谎（小于下限 / 大于给定字节）。
	mustFail("too short", buildACE(t, aceTypeAccessAllowed, 0, sidSelfB, 0, 0)[:4])
	bad := buildACE(t, aceTypeAccessAllowed, 0, sidSelfB, 0, 0)
	binary.LittleEndian.PutUint16(bad[2:4], 4)
	mustFail("ace size below floor", bad)
	binary.LittleEndian.PutUint16(bad[2:4], uint16(len(bad)+1))
	mustFail("ace size beyond bytes", bad)
	// OBJECT 型缺 Flags 字段。
	obj := buildACE(t, aceTypeAccessAllowedObject, 0, sidSelfB, aceObjectFlagObjectType, 0)
	binary.LittleEndian.PutUint16(obj[2:4], 8)
	mustFail("object without flags", obj)
	// SID 头越界与 SubAuthorityCount 越界（解析失败 → 检查失败）。
	trunc := buildACE(t, aceTypeAccessAllowedCallback, 0, sidSelfB, 0, 0)
	binary.LittleEndian.PutUint16(trunc[2:4], 10) // SidStart 只剩 2 字节
	mustFail("sid header out of range", trunc)
	huge := buildACE(t, aceTypeAccessAllowed, 0, sidSelfB, 0, 0)
	huge[8+1] = 0xFF // SubAuthorityCount 谎报 255
	mustFail("subauthority count out of range", huge)
}

// TestDaclAllowTrustees 整链分类（R35-#1）：可信普通 ACE + callback/object
// 允许型混排 → 三者 trustee 全部进入判定集；混入未知类型 → 报错且带下标。
func TestDaclAllowTrustees(t *testing.T) {
	sidSelfB := encodeSID(t, sidSelf)
	sidSysB := encodeSID(t, sidSys)
	sidAllB := encodeSID(t, sidAll)
	aces := [][]byte{
		buildACE(t, aceTypeAccessAllowed, 0, sidSelfB, 0, 0),
		buildACE(t, aceTypeAccessDenied, 0, sidAllB, 0, 0),                                               // 拒绝型：忽略
		buildACE(t, aceTypeAccessAllowedCallback, 0, encodeSID(t, sidAdmin), 0, 5),                       // callback 允许型：解析
		buildACE(t, aceTypeAccessAllowedObject, aceFlagInheritOnly, sidAllB, aceObjectFlagObjectType, 0), // INHERIT_ONLY：跳过
		buildACE(t, aceTypeAccessAllowedCallbackObject, 0, sidSysB, aceObjectFlagObjectType|aceObjectFlagInheritedObjectType, 0),
	}
	raw, err := daclAllowTrustees(aces)
	if err != nil {
		t.Fatalf("daclAllowTrustees = %v, want nil", err)
	}
	var got []string
	for _, r := range raw {
		got = append(got, decodeSID(t, r))
	}
	want := []string{sidSelf, sidAdmin, sidSys}
	if len(got) != len(want) {
		t.Fatalf("trustees = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("trustees[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	// 混入未知类型（藏身即绕过的最后通道）：整链按检查失败处理。
	aces = append(aces, buildACE(t, 0x77, 0, sidAllB, 0, 0))
	if _, err := daclAllowTrustees(aces); err == nil || !strings.Contains(err.Error(), "ACE #5") {
		t.Fatalf("unknown-type chain err = %v, want ACE #5 failure", err)
	}
	// 整链放行形态回归：三条 trustee 全在可信集 → aclVerdict 放行。
	if err := aclVerdict("console.yaml", true, want, trustedSIDs()); err != nil {
		t.Fatalf("verdict = %v, want nil", err)
	}
}
