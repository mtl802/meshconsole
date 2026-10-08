// config_acl.go 为 Windows DACL 检查的平台无关判定层（R35-#1）：NT ACE/SID 的
// 字节布局是稳定 ABI（winnt.h：ACE_HEADER / ACCESS_ALLOWED_OBJECT_ACE / SID），
// ACE 分类与 trustee 定位在此实现为纯字节运算，任意平台可测；Windows 封装
// （config_owner_windows.go）只负责 GetAce 枚举、原始字节切片与 SID 字符串化。
package config

import (
	"encoding/binary"
	"fmt"
)

// DACL ACE 类型常量（winnt.h AceType；x/sys 仅定义 0x00/0x01，其余按文档值补齐）。
const (
	aceTypeAccessAllowed               = 0x00 // ACCESS_ALLOWED_ACE_TYPE
	aceTypeAccessDenied                = 0x01 // ACCESS_DENIED_ACE_TYPE
	aceTypeAccessAllowedObject         = 0x05 // ACCESS_ALLOWED_OBJECT_ACE_TYPE
	aceTypeAccessDeniedObject          = 0x06 // ACCESS_DENIED_OBJECT_ACE_TYPE
	aceTypeAccessAllowedCallback       = 0x09 // ACCESS_ALLOWED_CALLBACK_ACE_TYPE
	aceTypeAccessDeniedCallback        = 0x0A // ACCESS_DENIED_CALLBACK_ACE_TYPE
	aceTypeAccessAllowedCallbackObject = 0x0B // ACCESS_ALLOWED_CALLBACK_OBJECT_ACE_TYPE
	aceTypeAccessDeniedCallbackObject  = 0x0C // ACCESS_DENIED_CALLBACK_OBJECT_ACE_TYPE
)

// ACE 结构布局常量（winnt.h）。
const (
	aceObjectFlagObjectType          = 0x1 // ACE_OBJECT_TYPE_PRESENT（OBJECT 型 Flags 位）
	aceObjectFlagInheritedObjectType = 0x2 // ACE_INHERITED_OBJECT_TYPE_PRESENT（同上）
	aceFlagInheritOnly               = 0x8 // INHERIT_ONLY_ACE（AceFlags 位）
	aceHeaderLen                     = 4   // ACE_HEADER：type u8 + flags u8 + size u16
	aceMaskLen                       = 4   // ACCESS_MASK 紧随 header（各 ACE 型皆有）
	aceObjectFlagsLen                = 4   // OBJECT 型 ACE 的 Flags 字段
	aceObjectGUIDLen                 = 16  // ObjectType / InheritedObjectType GUID
	aceSIDHeaderLen                  = 8   // SID：Revision u8 + SubAuthorityCount u8 + IdentifierAuthority 6B
	aceSIDSubAuthorityLen            = 4   // 每个 SubAuthority u32
)

// aceVerdict 为单条 DACL ACE 的分类结论。
type aceVerdict int

const (
	// aceTrusteeAllow 允许型 ACE（普通/callback/object/callback-object 四型）：
	// sidStart–sidEnd 给出 trustee 的原始 SID 字节，须纳入可信集判定。
	aceTrusteeAllow aceVerdict = iota
	// aceDenyIgnore 拒绝型 ACE（ACCESS_DENIED_*）：拒绝型只能收权、不能授权，
	// 不构成「不可信账户获权」的通道，故不参与放行判定——启动判据是
	// 「每条生效的允许型 ACE 的 trustee ∈ 可信集」（aclVerdict），拒绝条目
	// 无论 trustee 是谁都不会扩大访问面。
	aceDenyIgnore
	// aceInheritOnlyIgnore INHERIT_ONLY 的 ACE 不作用于本对象（仅遗传给子项），
	// 与类型无关地跳过。
	aceInheritOnlyIgnore
	// aceUnparsableFail 无法判定/解析失败（未知类型、系统 SACL 类、尺寸非法、
	// SID 区间越界）：按检查失败处理——安全侧默认拒绝，不允许「看不懂就跳过」。
	aceUnparsableFail
)

// classifyDACLACE 按 ACE 结构布局分类单条 DACL ACE 并定位 trustee SID（R35-#1）：
// ace 为自 ACE_HEADER 起的完整 ACE 原始字节（长 Header.AceSize）。sid 区间仅在
// 返回 aceTrusteeAllow 时有效。callback/object 允许型与普通允许型同等解析，
// 不再有「非普通允许型即跳过」的路径。
func classifyDACLACE(ace []byte) (aceVerdict, int, int) {
	if len(ace) < aceHeaderLen+aceMaskLen {
		return aceUnparsableFail, 0, 0
	}
	aceType, aceFlags := ace[0], ace[1]
	aceSize := int(binary.LittleEndian.Uint16(ace[2:4]))
	// 尺寸自洽：AceSize 须覆盖 header+mask，且不越过调用方给出的字节。
	if aceSize < aceHeaderLen+aceMaskLen || aceSize > len(ace) {
		return aceUnparsableFail, 0, 0
	}
	// INHERIT_ONLY 不作用于本对象，与类型无关地跳过。
	if aceFlags&aceFlagInheritOnly != 0 {
		return aceInheritOnlyIgnore, 0, 0
	}
	var sidStart int
	switch aceType {
	case aceTypeAccessAllowed, aceTypeAccessAllowedCallback:
		// 普通与 callback 允许型同布局：header + mask 后即 SidStart（callback
		// 型仅多出位于 SID 之后的回调数据，不影响 trustee 定位）。
		sidStart = aceHeaderLen + aceMaskLen
	case aceTypeAccessAllowedObject, aceTypeAccessAllowedCallbackObject:
		// OBJECT 允许型：mask 后是 Flags 字段，按位携带 0–2 个 GUID（各 16
		// 字节），再后才是 SidStart；callback-object 仅尾部多回调数据，布局同。
		if aceSize < aceHeaderLen+aceMaskLen+aceObjectFlagsLen {
			return aceUnparsableFail, 0, 0
		}
		objFlags := binary.LittleEndian.Uint32(ace[aceHeaderLen+aceMaskLen:])
		sidStart = aceHeaderLen + aceMaskLen + aceObjectFlagsLen
		if objFlags&aceObjectFlagObjectType != 0 {
			sidStart += aceObjectGUIDLen
		}
		if objFlags&aceObjectFlagInheritedObjectType != 0 {
			sidStart += aceObjectGUIDLen
		}
	case aceTypeAccessDenied, aceTypeAccessDeniedObject,
		aceTypeAccessDeniedCallback, aceTypeAccessDeniedCallbackObject:
		return aceDenyIgnore, 0, 0
	default:
		// 系统 SACL 类（审计/标签/策略）、保留与未来类型：出现在 DACL 即无法
		// 判定其对访问面的影响，按检查失败处理（安全侧默认拒绝）。
		return aceUnparsableFail, 0, 0
	}
	// trustee SID 边界自洽：最小 8 字节 SID 头，SubAuthorityCount 定长展开。
	if sidStart+aceSIDHeaderLen > aceSize {
		return aceUnparsableFail, 0, 0
	}
	sidEnd := sidStart + aceSIDHeaderLen + aceSIDSubAuthorityLen*int(ace[sidStart+1])
	if sidEnd > aceSize {
		return aceUnparsableFail, 0, 0
	}
	return aceTrusteeAllow, sidStart, sidEnd
}

// daclAllowTrustees 枚举分类一组 DACL ACE（平台无关，供 Windows 封装复用）：
// 返回须纳入可信集判定的 trustee 原始 SID 字节；任一 ACE 无法判定即报错
// （错误带 ACE 下标与类型字节），由调用方转成拒绝启动的 remediation 文案。
func daclAllowTrustees(aces [][]byte) ([][]byte, error) {
	var out [][]byte
	for i, a := range aces {
		v, s, e := classifyDACLACE(a)
		switch v {
		case aceTrusteeAllow:
			out = append(out, a[s:e])
		case aceDenyIgnore, aceInheritOnlyIgnore:
			// 见 aceVerdict 常量注释：拒绝型不能授权；INHERIT_ONLY 不作用本对象。
		default:
			typeByte := uint32(0)
			if len(a) > 0 {
				typeByte = uint32(a[0])
			}
			return nil, fmt.Errorf("ACE #%d（type=0x%02x）无法判定或解析失败", i, typeByte)
		}
	}
	return out, nil
}
