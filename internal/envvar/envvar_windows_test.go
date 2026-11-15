//go:build windows

// 本文件负责：Windows 上环境变量写入的集成测试。
//
// 安全前提：测试把 environmentKeyPath 指向 HKCU\Software\EnvReady\envtest-<pid>，
// 绝不会读写真实的 HKCU\Environment；广播也被替换成空操作。

package envvar

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setupTestKey 准备一个临时注册表键，返回清理函数。
func setupTestKey(t *testing.T) string {
	t.Helper()
	origPath := environmentKeyPath
	origBroadcast := broadcaster
	keyPath := `Software\EnvReady\envtest-` + strconv.Itoa(os.Getpid())
	environmentKeyPath = keyPath
	broadcaster = func() {} // 测试不打扰系统

	t.Cleanup(func() {
		environmentKeyPath = origPath
		broadcaster = origBroadcast
		if err := deleteKeyTree(keyPath); err != nil {
			t.Logf("清理测试键失败（可忽略）: %v", err)
		}
	})
	return keyPath
}

// readValue 读测试键里的值。
func readValue(t *testing.T, name string) (string, uint32, bool) {
	t.Helper()
	h, err := openKey(environmentKeyPath, keyQueryValue)
	if err != nil {
		t.Fatalf("打开测试键失败: %v", err)
	}
	defer closeKey(h)
	v, typ, ok, err := queryValue(h, name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return v, typ, ok
}

// requireRegistry 在环境禁止写注册表时跳过测试。
//
// 受限沙箱会拒绝 RegCreateKeyEx（Access denied）；此时逻辑正确性无法在本机验证，
// 但 Windows CI 上会真实执行，因此跳过而不是让测试永远红。
func requireRegistry(t *testing.T) {
	t.Helper()
	h, err := createKey(environmentKeyPath)
	if err != nil {
		t.Skipf("当前环境不允许写注册表，跳过（Windows CI 会执行）: %v", err)
	}
	closeKey(h)
}

// TestApplyRevertOnTestKey 写入 → 回读 → 回滚，全程不碰真实环境变量。
func TestApplyRevertOnTestKey(t *testing.T) {
	setupTestKey(t)
	requireRegistry(t)

	// 预置一份"用户原有"的 PATH：含 %USERPROFILE% 引用，且是 REG_EXPAND_SZ
	h, err := createKey(environmentKeyPath)
	if err != nil {
		t.Fatalf("创建测试键失败: %v", err)
	}
	const originalPath = `C:\Windows;%USERPROFILE%\bin`
	if err := setValue(h, "Path", originalPath, regExpandSZ); err != nil {
		t.Fatalf("预置 PATH 失败: %v", err)
	}
	closeKey(h)

	newDir := filepath.Join(t.TempDir(), "mingw", "16.2.0", "bin")
	backupDir := t.TempDir()

	change, err := Apply(Plan{
		Component: "mingw",
		HomeVar:   "MINGW_TEST_HOME",
		HomeValue: `D:\envs\mingw\16.2.0`,
		Dirs:      []string{newDir},
		Position:  Prepend,
		BackupDir: backupDir,
	})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}

	// PATH：新目录在最前，原有条目与变量引用都保留，类型仍是 REG_EXPAND_SZ
	gotPath, pathType, ok := readValue(t, "Path")
	if !ok {
		t.Fatal("PATH 不存在")
	}
	if !strings.HasPrefix(gotPath, newDir) {
		t.Errorf("PATH 未把新目录放在最前: %q", gotPath)
	}
	if !strings.Contains(gotPath, `%USERPROFILE%\bin`) {
		t.Errorf("原有变量引用被展开或丢失: %q", gotPath)
	}
	if pathType != regExpandSZ {
		t.Errorf("PATH 类型被改成了 %d，应保持 REG_EXPAND_SZ(2)", pathType)
	}

	// home 变量
	homeVal, _, hasHome := readValue(t, "MINGW_TEST_HOME")
	if !hasHome || homeVal != `D:\envs\mingw\16.2.0` {
		t.Errorf("home 变量 = %q（存在=%v）", homeVal, hasHome)
	}

	// 备份文件必须已经落盘
	if change.BackupFile == "" {
		t.Fatal("没有生成备份文件")
	}
	if _, err := os.Stat(change.BackupFile); err != nil {
		t.Errorf("备份文件不存在: %v", err)
	}

	// 再应用一次：必须幂等，PATH 不应变长
	if _, err := Apply(Plan{
		Component: "mingw",
		HomeVar:   "MINGW_TEST_HOME",
		HomeValue: `D:\envs\mingw\16.2.0`,
		Dirs:      []string{newDir},
		Position:  Prepend,
		BackupDir: backupDir,
	}); err != nil {
		t.Fatalf("第二次 Apply 失败: %v", err)
	}
	again, _, _ := readValue(t, "Path")
	if again != gotPath {
		t.Errorf("重复应用改变了 PATH：%q → %q", gotPath, again)
	}

	// 回滚：PATH 恢复原值，home 变量被删除
	if err := Revert(change); err != nil {
		t.Fatalf("Revert 失败: %v", err)
	}
	restored, _, _ := readValue(t, "Path")
	if restored != originalPath {
		t.Errorf("回滚后 PATH = %q, 期望 %q", restored, originalPath)
	}
	if _, _, stillThere := readValue(t, "MINGW_TEST_HOME"); stillThere {
		t.Error("回滚后 home 变量应被删除")
	}
}

// TestRemoveDirsFromTestKey 卸载时只移除自己的目录。
func TestRemoveDirsFromTestKey(t *testing.T) {
	setupTestKey(t)
	requireRegistry(t)

	h, err := createKey(environmentKeyPath)
	if err != nil {
		t.Fatalf("创建测试键失败: %v", err)
	}
	own := filepath.Join(t.TempDir(), "bin")
	const original = `C:\Windows`
	if err := setValue(h, "Path", own+";"+original, regExpandSZ); err != nil {
		t.Fatalf("预置 PATH 失败: %v", err)
	}
	closeKey(h)

	if err := Remove("demo", []string{own}); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	got, _, _ := readValue(t, "Path")
	if got != original {
		t.Errorf("移除后 PATH = %q, 期望 %q", got, original)
	}
}
