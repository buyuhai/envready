// 本文件负责：paths 包的单元测试。
// 重点覆盖三平台默认目录的推导，以及"版本号/key 来自云端时"的路径穿越防护。

package paths

import (
	"path/filepath"
	"testing"
)

// fakeEnv 构造一个只认识给定键值的 getenv。
func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// fakeHome 构造一个固定主目录的 homeDir。
func fakeHome(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// TestResolveDefaults 覆盖三平台默认目录推导。
func TestResolveDefaults(t *testing.T) {
	home := filepath.FromSlash("/home/alice")

	cases := []struct {
		name    string
		goos    string
		env     map[string]string
		wantApp string
	}{
		{
			name:    "windows 使用 LOCALAPPDATA",
			goos:    "windows",
			env:     map[string]string{"LOCALAPPDATA": filepath.FromSlash("C:/Users/alice/AppData/Local")},
			wantApp: filepath.Join(filepath.FromSlash("C:/Users/alice/AppData/Local"), "EnvReady"),
		},
		{
			name:    "windows 缺少 LOCALAPPDATA 时回退到主目录",
			goos:    "windows",
			env:     map[string]string{},
			wantApp: filepath.Join(home, "AppData", "Local", "EnvReady"),
		},
		{
			name:    "darwin 使用 Application Support",
			goos:    "darwin",
			env:     map[string]string{},
			wantApp: filepath.Join(home, "Library", "Application Support", "EnvReady"),
		},
		{
			name:    "linux 使用 XDG_DATA_HOME",
			goos:    "linux",
			env:     map[string]string{"XDG_DATA_HOME": filepath.FromSlash("/data")},
			wantApp: filepath.Join(filepath.FromSlash("/data"), "envready"),
		},
		{
			name:    "linux 缺少 XDG_DATA_HOME 时回退到 ~/.local/share",
			goos:    "linux",
			env:     map[string]string{},
			wantApp: filepath.Join(home, ".local", "share", "envready"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolve(tc.goos, fakeEnv(tc.env), fakeHome(home))
			if err != nil {
				t.Fatalf("resolve 失败: %v", err)
			}
			if got.AppRoot != tc.wantApp {
				t.Errorf("AppRoot = %q, 期望 %q", got.AppRoot, tc.wantApp)
			}
			wantEnv := filepath.Join(tc.wantApp, "envs")
			if got.EnvRoot != wantEnv {
				t.Errorf("EnvRoot = %q, 期望 %q", got.EnvRoot, wantEnv)
			}
		})
	}
}

// TestResolveOverrides 验证调试/便携开关。
func TestResolveOverrides(t *testing.T) {
	home := filepath.FromSlash("/home/alice")

	// 只覆盖程序根：环境根自动落在其下 envs
	got, err := resolve("linux", fakeEnv(map[string]string{
		envAppRoot: filepath.FromSlash("/tmp/app"),
	}), fakeHome(home))
	if err != nil {
		t.Fatalf("resolve 失败: %v", err)
	}
	if got.AppRoot != filepath.FromSlash("/tmp/app") {
		t.Errorf("AppRoot = %q", got.AppRoot)
	}
	if got.EnvRoot != filepath.Join(filepath.FromSlash("/tmp/app"), "envs") {
		t.Errorf("EnvRoot = %q", got.EnvRoot)
	}

	// 两个都覆盖
	got, err = resolve("linux", fakeEnv(map[string]string{
		envAppRoot: filepath.FromSlash("/tmp/app"),
		envEnvRoot: filepath.FromSlash("/data/envs"),
	}), fakeHome(home))
	if err != nil {
		t.Fatalf("resolve 失败: %v", err)
	}
	if got.EnvRoot != filepath.FromSlash("/data/envs") {
		t.Errorf("EnvRoot = %q, 期望 /data/envs", got.EnvRoot)
	}
}

// TestResolveHomeError 主目录不可用且无平台变量时应报错，而不是拼出相对路径。
func TestResolveHomeError(t *testing.T) {
	_, err := resolve("linux", fakeEnv(map[string]string{}), func() (string, error) {
		return "", ErrNoHomeDir
	})
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
}

// TestLayoutSubdirs 校验固定子目录拼接。
func TestLayoutSubdirs(t *testing.T) {
	app := filepath.FromSlash("/opt/envready")
	envRoot := filepath.FromSlash("/data/envs")
	l := Layout{AppRoot: app, EnvRoot: envRoot}

	checks := map[string]string{
		l.PointerFile():  filepath.Join(app, "current.json"),
		l.AppDir():       filepath.Join(app, "app"),
		l.DownloadsDir(): filepath.Join(app, "downloads"),
		l.LogsDir():      filepath.Join(app, "logs"),
		l.CacheDir():     filepath.Join(app, "cache"),
		l.BackupDir():    filepath.Join(app, "env-backup"),
	}
	for got, want := range checks {
		if got != want {
			t.Errorf("路径 = %q, 期望 %q", got, want)
		}
	}
}

// TestVersionDirRejectsTraversal 版本号来自云端清单，必须拒绝路径穿越。
func TestVersionDirRejectsTraversal(t *testing.T) {
	l := Layout{AppRoot: filepath.FromSlash("/opt/envready"), EnvRoot: filepath.FromSlash("/data/envs")}

	bad := []string{"", "  ", ".", "..", "../etc", `a\b`, "a/b", "C:/windows"}
	for _, v := range bad {
		if _, err := l.AppVersionDir(v); err == nil {
			t.Errorf("AppVersionDir(%q) 期望报错，实际通过", v)
		}
		if _, err := l.ComponentDir(v); err == nil {
			t.Errorf("ComponentDir(%q) 期望报错，实际通过", v)
		}
	}

	// 正常值应通过，且落在预期目录下
	got, err := l.AppVersionDir("1.2.3")
	if err != nil {
		t.Fatalf("合法版本号被拒: %v", err)
	}
	if got != filepath.Join(l.AppDir(), "1.2.3") {
		t.Errorf("AppVersionDir = %q", got)
	}

	got, err = l.ComponentDir("mingw")
	if err != nil {
		t.Fatalf("合法 key 被拒: %v", err)
	}
	if got != filepath.Join(l.EnvRoot, "mingw") {
		t.Errorf("ComponentDir = %q", got)
	}
}
