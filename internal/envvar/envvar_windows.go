//go:build windows

// 本文件负责：Windows 上读写用户环境变量（HKCU\Environment），并通过广播通知系统刷新。
// 边界：只碰用户级键；不提权、不碰 HKLM、不碰系统目录。
//
// 为什么不用 setx：setx 会把 PATH 截断到 1024 字符，而且会把 REG_EXPAND_SZ
// 展开成字面值——一旦展开，用户 PATH 里的 %USERPROFILE% 之类引用就永久变成死路径。
//
// 为什么不用 x/sys/windows/registry：本项目零第三方依赖，这里直接用 syscall 调 advapi32。

package envvar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// environmentKeyPath 是相对 HKCU 的键路径。测试会把它换成临时键，
// 从而在不触碰真实用户环境的前提下验证读写与回滚。
var environmentKeyPath = `Environment`

const (
	regSZ       = 1
	regExpandSZ = 2

	keyQueryValue = 0x0001
	keySetValue   = 0x0002
	keyAllAccess  = 0x000F003F

	wmSettingChange   = 0x001A
	smtoAbortIfHung   = 0x0002
	hwndBroadcast     = 0xFFFF
	broadcastTimeoutM = 5000
)

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")

	procRegOpenKeyEx   = advapi32.NewProc("RegOpenKeyExW")
	procRegCreateKeyEx = advapi32.NewProc("RegCreateKeyExW")
	procRegQueryValue  = advapi32.NewProc("RegQueryValueExW")
	procRegSetValue    = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValue = advapi32.NewProc("RegDeleteValueW")
	procRegDeleteTree  = advapi32.NewProc("RegDeleteTreeW")
	procRegCloseKey    = advapi32.NewProc("RegCloseKey")

	procSendMessageTimeout = user32.NewProc("SendMessageTimeoutW")
)

// openKey 打开（或创建）目标键。
func openKey(path string, access uint32) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var h syscall.Handle
	r, _, _ := procRegOpenKeyEx.Call(
		uintptr(syscall.HKEY_CURRENT_USER),
		uintptr(unsafe.Pointer(p)),
		0,
		uintptr(access),
		uintptr(unsafe.Pointer(&h)),
	)
	if r != 0 {
		return 0, fmt.Errorf("打开注册表键失败: %w", syscall.Errno(r))
	}
	return h, nil
}

// createKey 创建（或打开）目标键，主要用于测试与首次写入。
func createKey(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var h syscall.Handle
	var disp uint32
	r, _, _ := procRegCreateKeyEx.Call(
		uintptr(syscall.HKEY_CURRENT_USER),
		uintptr(unsafe.Pointer(p)),
		0, 0, 0,
		uintptr(keyAllAccess),
		0,
		uintptr(unsafe.Pointer(&h)),
		uintptr(unsafe.Pointer(&disp)),
	)
	if r != 0 {
		return 0, fmt.Errorf("创建注册表键失败: %w", syscall.Errno(r))
	}
	return h, nil
}

// closeKey 关闭键句柄。
func closeKey(h syscall.Handle) {
	if h != 0 {
		_, _, _ = procRegCloseKey.Call(uintptr(h))
	}
}

// queryValue 读取一个值及其类型。不存在时返回 (\"\", 0, false, nil)。
func queryValue(h syscall.Handle, name string) (string, uint32, bool, error) {
	np, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", 0, false, err
	}
	var typ uint32
	var size uint32
	r, _, _ := procRegQueryValue.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(np)),
		0,
		uintptr(unsafe.Pointer(&typ)),
		0,
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 {
		if syscall.Errno(r) == syscall.ERROR_FILE_NOT_FOUND {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("读取注册表值 %s 失败: %w", name, syscall.Errno(r))
	}
	if size == 0 {
		return "", typ, true, nil
	}
	buf := make([]byte, size)
	r, _, _ = procRegQueryValue.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(np)),
		0,
		uintptr(unsafe.Pointer(&typ)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 {
		return "", 0, false, fmt.Errorf("读取注册表值 %s 失败: %w", name, syscall.Errno(r))
	}
	return decodeUTF16(buf), typ, true, nil
}

// setValue 写入一个字符串值。
func setValue(h syscall.Handle, name, value string, typ uint32) error {
	np, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	u, uerr := syscall.UTF16FromString(value) // 末尾带 NUL
	if uerr != nil {
		return fmt.Errorf("环境变量 %s 的值无法转换为 UTF-16: %w", name, uerr)
	}
	byteLen := len(u) * 2
	r, _, _ := procRegSetValue.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(np)),
		0,
		uintptr(typ),
		uintptr(unsafe.Pointer(&u[0])),
		uintptr(byteLen),
	)
	if r != 0 {
		return fmt.Errorf("写入注册表值 %s 失败: %w", name, syscall.Errno(r))
	}
	return nil
}

// deleteValue 删除一个值；不存在视为成功（回滚要幂等）。
func deleteValue(h syscall.Handle, name string) error {
	np, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r, _, _ := procRegDeleteValue.Call(uintptr(h), uintptr(unsafe.Pointer(np)))
	if r != 0 && syscall.Errno(r) != syscall.ERROR_FILE_NOT_FOUND {
		return fmt.Errorf("删除注册表值 %s 失败: %w", name, syscall.Errno(r))
	}
	return nil
}

// deleteKeyTree 删除整个键（仅测试清理用）。
func deleteKeyTree(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r, _, _ := procRegDeleteTree.Call(
		uintptr(syscall.HKEY_CURRENT_USER),
		uintptr(unsafe.Pointer(p)),
	)
	if r != 0 && syscall.Errno(r) != syscall.ERROR_FILE_NOT_FOUND {
		return fmt.Errorf("删除注册表键 %s 失败: %w", path, syscall.Errno(r))
	}
	return nil
}

// decodeUTF16 把 UTF-16LE 字节串解码成 Go 字符串。
func decodeUTF16(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return syscall.UTF16ToString(u)
}

// broadcaster 便于测试替换（测试不应向系统广播）。
var broadcaster = broadcastChange

// broadcastChange 通知系统"环境变量变了"，让新开的程序能看到。
//
// 必须做这一步：只写注册表不广播，已经打开的"资源管理器"不会刷新环境，
// 用户从开始菜单启动的程序仍旧拿到旧 PATH，看起来就像"装了个假的"。
func broadcastChange() {
	env, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	var result uintptr
	_, _, _ = procSendMessageTimeout.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(env)),
		uintptr(smtoAbortIfHung),
		uintptr(broadcastTimeoutM),
		uintptr(unsafe.Pointer(&result)),
	)
}

// backupRecord 是写入前的快照，落到磁盘上供"一键还原"使用。
type backupRecord struct {
	Component string    `json:"component"`
	KeyPath   string    `json:"key_path"`
	HomeVar   string    `json:"home_var"`
	OldHome   string    `json:"old_home"`
	HadHome   bool      `json:"had_home"`
	HomeType  uint32    `json:"home_type"`
	OldPath   string    `json:"old_path"`
	HadPath   bool      `json:"had_path"`
	PathType  uint32    `json:"path_type"`
	CreatedAt time.Time `json:"created_at"`
}

// Apply 应用一次环境变量变更。
//
// 失败：打开/创建键失败、读取失败、写盘失败、回读不一致。
// 副作用：修改 HKCU 下目标键的值；在 BackupDir 写一份快照；向系统广播环境变更。
func Apply(p Plan) (Change, error) {
	if strings.TrimSpace(p.Component) == "" {
		return Change{}, fmt.Errorf("环境变量变更缺少组件标识")
	}
	if len(p.Dirs) == 0 && strings.TrimSpace(p.HomeVar) == "" {
		return Change{}, fmt.Errorf("环境变量变更没有任何目标（既无 PATH 目录也无 home 变量）")
	}
	if err := os.MkdirAll(p.BackupDir, 0o755); err != nil {
		return Change{}, fmt.Errorf("创建备份目录失败: %w", err)
	}

	h, err := createKey(environmentKeyPath)
	if err != nil {
		return Change{}, err
	}
	defer closeKey(h)

	oldPath, pathType, hadPath, err := queryValue(h, "Path")
	if err != nil {
		return Change{}, err
	}
	change := Change{
		Component: p.Component,
		HomeVar:   p.HomeVar,
		OldPath:   oldPath,
		HadPath:   hadPath,
	}

	// 备份"原始未展开值"：这是回滚的唯一依据，必须先落盘再动手
	rec := backupRecord{
		Component: p.Component,
		KeyPath:   environmentKeyPath,
		HomeVar:   p.HomeVar,
		OldPath:   oldPath,
		HadPath:   hadPath,
		PathType:  pathType,
		CreatedAt: time.Now().UTC(),
	}
	if p.HomeVar != "" {
		oldHome, homeType, hadHome, herr := queryValue(h, p.HomeVar)
		if herr != nil {
			return Change{}, herr
		}
		rec.OldHome, rec.HadHome, rec.HomeType = oldHome, hadHome, homeType
		change.OldHome, change.HadHome = oldHome, hadHome
	}

	backupPath := filepath.Join(p.BackupDir,
		fmt.Sprintf("env-%s-%s.json", p.Component, time.Now().UTC().Format("20060102-150405")))
	raw, merr := json.MarshalIndent(rec, "", "  ")
	if merr != nil {
		return Change{}, fmt.Errorf("序列化备份失败: %w", merr)
	}
	if werr := os.WriteFile(backupPath, raw, 0o644); werr != nil {
		return Change{}, fmt.Errorf("写备份失败（未做任何修改）: %w", werr)
	}
	change.BackupFile = backupPath

	// 写 PATH：保持原有类型；原来没有则用 REG_EXPAND_SZ（PATH 通常含 %USERPROFILE% 之类的引用）
	newPath := mergePath(oldPath, p.Dirs, p.Position, ";", true)
	if newPath != oldPath {
		writeType := pathType
		if !hadPath || (writeType != regSZ && writeType != regExpandSZ) {
			writeType = regExpandSZ
		}
		if err := setValue(h, "Path", newPath, writeType); err != nil {
			return change, err
		}
	}
	change.NewPath = newPath

	// 写 home 变量
	if p.HomeVar != "" {
		if err := setValue(h, p.HomeVar, p.HomeValue, regExpandSZ); err != nil {
			return change, err
		}
	}

	// 回读比对：读回来不等于写进去，就算失败
	gotPath, _, _, err := queryValue(h, "Path")
	if err != nil {
		return change, err
	}
	if gotPath != newPath {
		return change, fmt.Errorf("PATH 回读不一致（写入 %q，读回 %q）", newPath, gotPath)
	}
	if p.HomeVar != "" {
		gotHome, _, _, herr := queryValue(h, p.HomeVar)
		if herr != nil {
			return change, herr
		}
		if gotHome != p.HomeValue {
			return change, fmt.Errorf("%s 回读不一致（写入 %q，读回 %q）", p.HomeVar, p.HomeValue, gotHome)
		}
	}

	broadcaster()
	return change, nil
}

// Revert 按变更凭据回滚。
//
// 失败：打开键失败、写回失败、回读不一致。
// 副作用：把 PATH 与 home 变量恢复到变更前的原始未展开值；广播环境变更。
func Revert(c Change) error {
	h, err := openKey(environmentKeyPath, keyQueryValue|keySetValue)
	if err != nil {
		return err
	}
	defer closeKey(h)

	if c.HadPath {
		typ := uint32(regSZ)
		if strings.Contains(c.OldPath, "%") {
			// 含 %VAR% 引用必须写回 REG_EXPAND_SZ，否则变量会被当成字面量
			typ = regExpandSZ
		}
		if err := setValue(h, "Path", c.OldPath, typ); err != nil {
			return err
		}
	} else if err := deleteValue(h, "Path"); err != nil {
		return err
	}

	if c.HomeVar != "" {
		if c.HadHome {
			if err := setValue(h, c.HomeVar, c.OldHome, regExpandSZ); err != nil {
				return err
			}
		} else if err := deleteValue(h, c.HomeVar); err != nil {
			return err
		}
	}

	// 回读校验
	if c.HadPath {
		got, _, _, gerr := queryValue(h, "Path")
		if gerr != nil {
			return gerr
		}
		if got != c.OldPath {
			return fmt.Errorf("回滚后 PATH 与原始值不一致（原始 %q，当前 %q）", c.OldPath, got)
		}
	}

	broadcaster()
	return nil
}

// Remove 从 PATH 中移除指定目录（卸载用），其它值保持不变。
//
// Windows 按目录匹配（不同组件可能写同一个目录的可能性极低），
// 因此 component 参数在这里不参与判断。
func Remove(_ string, dirs []string) error {
	h, err := openKey(environmentKeyPath, keyQueryValue|keySetValue)
	if err != nil {
		return err
	}
	defer closeKey(h)

	oldPath, typ, hadPath, err := queryValue(h, "Path")
	if err != nil {
		return err
	}
	if !hadPath {
		return nil
	}
	newPath := removeFromPath(oldPath, dirs, ";", true)
	if newPath == oldPath {
		return nil
	}
	if typ != regSZ && typ != regExpandSZ {
		typ = regExpandSZ
	}
	if err := setValue(h, "Path", newPath, typ); err != nil {
		return err
	}
	broadcaster()
	return nil
}

// ReadPath 返回当前用户 PATH 的原始未展开值（诊断用）。
func ReadPath() (string, error) {
	h, err := openKey(environmentKeyPath, keyQueryValue)
	if err != nil {
		return "", err
	}
	defer closeKey(h)
	v, _, _, err := queryValue(h, "Path")
	return v, err
}
