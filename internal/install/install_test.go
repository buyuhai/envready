// 本文件负责：install 包的单元测试。
// 覆盖：目标目录推导、空格路径策略、命令定位、自检解析、完整安装与自检失败回滚。

package install

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/buyuhai/envready/internal/component"
)

// TestTargetDir 安装目录必须是 <envRoot>/<key>/<version>。
func TestTargetDir(t *testing.T) {
	got := TargetDir(filepath.FromSlash("/data/envs"), "mingw", "16.2.0")
	want := filepath.Join(filepath.FromSlash("/data/envs"), "mingw", "16.2.0")
	if got != want {
		t.Errorf("TargetDir = %q, 期望 %q", got, want)
	}
}

// TestCheckPathSpace 空格策略由清单声明，且只在 Windows 上生效。
func TestCheckPathSpace(t *testing.T) {
	orig := runningOnWindows
	defer func() { runningOnWindows = orig }()

	// 未声明要求：任何路径都放过
	if err := CheckPathSpace(`D:\work place\envs`, false); err != nil {
		t.Errorf("未要求无空格时不应报错: %v", err)
	}

	// Windows + 声明要求 + 含空格：必须拒绝
	runningOnWindows = true
	if err := CheckPathSpace(`D:\work place\envs`, true); !errors.Is(err, ErrSpaceInPath) {
		t.Errorf("期望 ErrSpaceInPath，得到 %v", err)
	}
	// Windows + 声明要求 + 无空格：放过
	if err := CheckPathSpace(`D:\envs`, true); err != nil {
		t.Errorf("无空格路径不应报错: %v", err)
	}

	// 非 Windows：即使声明要求也放过（该限制来自 winlibs 的 binutils）
	runningOnWindows = false
	if err := CheckPathSpace(`/home/a b/envs`, true); err != nil {
		t.Errorf("非 Windows 不应拦截: %v", err)
	}
}

// TestFindCommand 在候选子目录里按平台命名规则找可执行文件。
func TestFindCommand(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := executableNames("tool")[0]
	toolPath := filepath.Join(binDir, name)
	if err := os.WriteFile(toolPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := findCommand(dir, []string{"bin"}, "tool")
	if err != nil {
		t.Fatalf("应找到命令: %v", err)
	}
	if got != toolPath {
		t.Errorf("找到的路径 = %q, 期望 %q", got, toolPath)
	}

	if _, err := findCommand(dir, []string{"bin"}, "missing"); err == nil {
		t.Error("不存在的命令应报错")
	}
}

// TestRunVerifyParsesVersion 自检输出按正则提取版本号。
func TestRunVerifyParsesVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", executableNames("tool")[0]), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	v := &component.Verify{Command: "tool", Args: []string{"--version"}, Parse: `tool (\d+\.\d+\.\d+)`}
	fake := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("tool 1.2.3\nbuilt somewhere"), nil
	}

	_, ver, err := RunVerify(context.Background(), dir, []string{"bin"}, v, fake)
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if ver != "1.2.3" {
		t.Errorf("解析出版本 = %q", ver)
	}

	// 输出里没有版本号时必须报错，不能当作成功
	bad := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("nothing useful"), nil
	}
	if _, _, err := RunVerify(context.Background(), dir, []string{"bin"}, v, bad); err == nil {
		t.Error("找不到版本号时应报错")
	}
}

// demoComponent 构造一个可安装的组件定义（archive 类型）。
func demoComponent(mirror, file, sum string, size int64) (*component.Component, *component.Version, component.Platform) {
	plat := component.Platform{
		InstallKind: component.InstallArchive,
		File:        file,
		Size:        size,
		SHA256:      sum,
		Mirrors:     []string{mirror},
		Archive:     component.ArchiveSpec{Format: component.FormatZip, StripRoot: true},
	}
	ver := &component.Version{
		Version: "1.0.0",
		Verify:  &component.Verify{Command: "tool", Args: []string{"--version"}, Parse: `tool (\d+\.\d+\.\d+)`},
	}
	c := &component.Component{
		Schema:   1,
		Key:      "demo",
		Name:     "Demo",
		Env:      component.EnvSpec{Path: component.PathSpec{Dirs: []string{"bin"}}},
		Versions: []component.Version{*ver},
	}
	return c, ver, plat
}

// buildZip 生成含 demo-1.0/bin/tool 的 zip。
func buildZip(t *testing.T) []byte {
	t.Helper()
	var buf zipBuffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create("demo-1.0/"); err != nil {
		t.Fatal(err)
	}
	w, err := zw.Create("demo-1.0/bin/" + executableNames("tool")[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipBuffer 是 bytes.Buffer 的最小替身，避免测试文件里再引一个包。
type zipBuffer struct {
	b []byte
}

func (z *zipBuffer) Write(p []byte) (int, error) {
	z.b = append(z.b, p...)
	return len(p), nil
}

func (z *zipBuffer) Bytes() []byte { return z.b }

// TestRunEndToEnd 完整安装并校验落盘结果与安装记录。
func TestRunEndToEnd(t *testing.T) {
	zipBytes := buildZip(t)
	sum := sha256.Sum256(zipBytes)
	hexSum := hex.EncodeToString(sum[:])

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	c, ver, plat := demoComponent(srv.URL, "demo.zip", hexSum, int64(len(zipBytes)))
	envRoot := filepath.Join(t.TempDir(), "envs")
	downloads := filepath.Join(t.TempDir(), "dl")

	res, err := Run(context.Background(), Request{
		Component: c,
		Version:   ver,
		Platform:  plat,
		EnvRoot:   envRoot,
		Downloads: downloads,
		Client:    srv.Client(),
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("tool 1.0.0"), nil
		},
	})
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if res.Dir != TargetDir(envRoot, "demo", "1.0.0") {
		t.Errorf("安装目录 = %q", res.Dir)
	}
	if !res.Verified {
		t.Error("应完成校验")
	}
	if res.VerifyVersion != "1.0.0" {
		t.Errorf("自检版本 = %q", res.VerifyVersion)
	}

	// 安装记录必须可读回
	rec, err := ReadRecord(res.Dir)
	if err != nil {
		t.Fatalf("读取安装记录失败: %v", err)
	}
	if rec.SHA256 != hexSum || !rec.Verified {
		t.Errorf("记录内容不符: %+v", rec)
	}

	// 重复安装应被拒绝
	if _, err := Run(context.Background(), Request{
		Component: c, Version: ver, Platform: plat,
		EnvRoot: envRoot, Downloads: downloads, Client: srv.Client(),
		Runner: func(context.Context, string, ...string) ([]byte, error) { return []byte("tool 1.0.0"), nil },
	}); !errors.Is(err, ErrAlreadyInstalled) {
		t.Errorf("期望 ErrAlreadyInstalled，得到 %v", err)
	}
}

// TestRunRollsBackOnVerifyFailure 自检失败必须回滚，不能留下"看起来装好了"的目录。
func TestRunRollsBackOnVerifyFailure(t *testing.T) {
	zipBytes := buildZip(t)
	sum := sha256.Sum256(zipBytes)
	hexSum := hex.EncodeToString(sum[:])

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	c, ver, plat := demoComponent(srv.URL, "demo.zip", hexSum, int64(len(zipBytes)))
	envRoot := filepath.Join(t.TempDir(), "envs")

	_, err := Run(context.Background(), Request{
		Component: c, Version: ver, Platform: plat,
		EnvRoot:   envRoot,
		Downloads: filepath.Join(t.TempDir(), "dl"),
		Client:    srv.Client(),
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("boom"), errors.New("自检失败")
		},
	})
	if err == nil {
		t.Fatal("自检失败时安装应报错")
	}
	if _, serr := os.Stat(TargetDir(envRoot, "demo", "1.0.0")); !os.IsNotExist(serr) {
		t.Error("自检失败后安装目录应被回滚删除")
	}
}

// TestRunRejectsSpacePath 声明了 space_free_path 的组件在含空格路径下应安装前就拒绝。
func TestRunRejectsSpacePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("该限制只针对 Windows 上的 winlibs binutils")
	}
	c, ver, plat := demoComponent("https://example.com", "demo.zip", "", 0)
	c.Requirements.SpaceFreePath = true

	_, err := Run(context.Background(), Request{
		Component: c, Version: ver, Platform: plat,
		EnvRoot:   filepath.Join(t.TempDir(), "with space", "envs"),
		Downloads: t.TempDir(),
	})
	if !errors.Is(err, ErrSpaceInPath) {
		t.Fatalf("期望 ErrSpaceInPath，得到 %v", err)
	}
}
