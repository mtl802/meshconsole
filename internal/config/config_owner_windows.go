//go:build windows

package config

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// checkConfigPlatformSecurity 为 CheckConfigFileSecurity 的 Windows 平台钩子
// （SPEC §4、R33-#2 实装；R35-#1 收紧）：NTFS 权限语义在 ACL 而非 POSIX uid，
// 校验①属主为当前用户或 BUILTIN\Administrators；②DACL 中每条生效的允许型
// ACE 的 trustee 都 ∈ {当前用户, SYSTEM, Administrators}——即 config 仅这三类
// 账户持有访问权。允许型含普通/callback/object/callback-object 四型（callback
// 与 object 型同样可授予访问权，trustee 按各型布局解析，R35-#1）；拒绝型只收
// 权不授权、不参与放行判定（见 aceDenyIgnore）；无法判定/解析失败的 ACE 按
// 检查失败处理。无法取得 ACL/属主同按检查失败处理（公网形态拒绝启动），
// 不允许静默放行。
func checkConfigPlatformSecurity(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("公网模式启动被拒绝：无法读取 config 文件 %s 的安全描述符（%v）；请确认文件位于 NTFS 卷，并以 `icacls %s /inheritance:r` 后仅授予运行用户与 SYSTEM、Administrators 权限", path, err, path)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("公网模式启动被拒绝：无法取得 config 文件 %s 的属主（%v）；请以资源管理器/icacls 将属主改为运行用户后重试", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		// DACL 缺失 = 所有人对该对象拥有全部访问权（Windows 语义），必须拒绝。
		return fmt.Errorf("公网模式启动被拒绝：config 文件 %s 没有 DACL（访问不受限）；请执行 `icacls %s /inheritance:r` 后仅授予运行用户与 SYSTEM、Administrators 权限", path, path)
	}

	self, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("公网模式启动被拒绝：无法取得当前进程用户 SID（%v）", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("公网模式启动被拒绝：无法构造 Administrators SID（%v）", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("公网模式启动被拒绝：无法构造 SYSTEM SID（%v）", err)
	}

	ownerSelfOK := owner.Equals(self) || owner.Equals(admins)
	var allowTrustees, trusted []string
	trusted = append(trusted, self.String(), admins.String(), system.String())
	aces := make([][]byte, 0, dacl.AceCount)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("公网模式启动被拒绝：读取 config 文件 %s 的 ACE #%d 失败（%v）", path, i, err)
		}
		// ACE 自 ACE_HEADER 起共 AceSize 字节（GetAce 契约：指针指向 ACL 缓冲
		// 内该 ACE 起始；AceSize 为内核校验过的 ACL 布局字段）。解析下限与
		// trustee 越界在判定层强制，异常形态按检查失败处理（R35-#1）。
		aceSize := int(ace.Header.AceSize)
		if aceSize < aceHeaderLen+aceMaskLen {
			return fmt.Errorf("公网模式启动被拒绝：config 文件 %s 的 ACE #%d 尺寸非法（%d 字节），按检查失败处理；请执行 `icacls %s /inheritance:r` 后仅授予运行用户与 SYSTEM、Administrators 权限", path, i, aceSize, path)
		}
		aces = append(aces, unsafe.Slice((*byte)(unsafe.Pointer(ace)), aceSize))
	}
	rawSIDs, err := daclAllowTrustees(aces)
	if err != nil {
		return fmt.Errorf("公网模式启动被拒绝：config 文件 %s 的 DACL 含无法判定的条目（%v）——callback/object 等允许型 ACE 同样可授予访问权，为防不可信账户藏身其上，按检查失败处理；请执行 `icacls %s /inheritance:r` 后仅授予运行用户与 SYSTEM、Administrators 权限", path, err, path)
	}
	for _, raw := range rawSIDs {
		sid := (*windows.SID)(unsafe.Pointer(&raw[0]))
		// IsValid 校验 revision 与长度；无法解析的 trustee 按检查失败处理，
		// 不猜、不跳过（R35-#1 安全侧默认拒绝）。
		if !sid.IsValid() {
			return fmt.Errorf("公网模式启动被拒绝：config 文件 %s 的 DACL 含无法解析的 trustee SID，按检查失败处理；请执行 `icacls %s /inheritance:r` 后仅授予运行用户与 SYSTEM、Administrators 权限", path, path)
		}
		allowTrustees = append(allowTrustees, sid.String())
	}
	return aclVerdict(path, ownerSelfOK, allowTrustees, trusted)
}

// currentUserSID 取当前进程 token 的用户 SID。
func currentUserSID() (*windows.SID, error) {
	tk, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer tk.Close()
	tu, err := tk.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return tu.User.Sid, nil
}
