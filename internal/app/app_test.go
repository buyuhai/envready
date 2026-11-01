// 本文件负责：app 包的编排测试。
// 用本地 HTTP 服务器 + 临时清单，验证"清单 → 下载 → 校验 → 解压"整条链路。

package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/buyuhai/envready/internal/component"
)

// buildZip 生成一个内存里的 zip，内含 bin/demo.txt。
func buildZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("demo-1.0/bin/demo.txt")
	if err != nil {
		t.Fatalf("创建 zip 条目失败: %v", err)
	}
	if _, err := w.Write([]byte("hello envready")); err != nil {
		t.Fatalf("写 zip 失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	return buf.Bytes()
}

// writeManifest 把组件定义写成临时清单文件，返回路径。
func writeManifest(t *testing.T, dir string, c component.Component) string {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("序列化清单失败: %v", err)
	}
	p := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatalf("写入清单失败: %v", err)
	}
	return p
}

// demoComponent 构造一个指向给定地址的组件定义。
func demoComponent(mirror, file string, size int64, sum string) component.Component {
	return component.Component{
		Schema:      1,
		Key:         "demo",
		Name:        "Demo",
		Recommended: "1.0.0",
		Env:         component.EnvSpec{Path: component.PathSpec{Dirs: []string{"bin"}}},
		Versions: []component.Version{
			{
				Version: "1.0.0",
				Platforms: map[string]component.Platform{
					component.PlatformKey(runtime.GOOS, runtime.GOARCH): {
						InstallKind: component.InstallArchive,
						File:        file,
						Size:        size,
						SHA256:      sum,
						Mirrors:     []string{mirror},
						Archive:     component.ArchiveSpec{Format: component.FormatZip, StripRoot: true},
					},
				},
				Verify: &component.Verify{Command: "demo"},
			},
		},
	}
}

// TestFetchEndToEnd 跑通整条链路并检查解压结果。
func TestFetchEndToEnd(t *testing.T) {
	zipBytes := buildZip(t)
	sum := sha256.Sum256(zipBytes)
	hexSum := hex.EncodeToString(sum[:])

	// 用 TLS 服务器：清单校验要求镜像必须是 https，正好把这条规则也覆盖到
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", itoa(len(zipBytes)))
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	manifestPath := writeManifest(t, dir, demoComponent(srv.URL, "demo.zip", int64(len(zipBytes)), hexSum))
	destDir := filepath.Join(dir, "dl")
	extractTo := filepath.Join(dir, "out")

	res, err := Fetch(context.Background(), FetchRequest{
		Key:            "demo",
		ManifestSource: manifestPath,
		DestDir:        destDir,
		ExtractTo:      extractTo,
		Client:         srv.Client(),
	})
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if !res.Verified {
		t.Error("应完成校验")
	}
	if res.Version != "1.0.0" {
		t.Errorf("版本 = %q", res.Version)
	}
	if _, err := os.Stat(res.File); err != nil {
		t.Errorf("安装包不存在: %v", err)
	}
	// StripRoot 为 true：bin/demo.txt 应直接落在解压目录下
	got, err := os.ReadFile(filepath.Join(extractTo, "bin", "demo.txt"))
	if err != nil {
		t.Fatalf("解压结果不存在: %v", err)
	}
	if string(got) != "hello envready" {
		t.Errorf("解压内容 = %q", string(got))
	}
}

// TestFetchKeyMismatch 请求的组件名与清单不一致时必须拒绝。
func TestFetchKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeManifest(t, dir, demoComponent("https://example.com", "x.zip", 1, ""))

	_, err := Fetch(context.Background(), FetchRequest{
		Key:            "other",
		ManifestSource: manifestPath,
		DestDir:        filepath.Join(dir, "dl"),
	})
	if err == nil {
		t.Fatal("组件名不一致时应报错")
	}
}

// TestFetchRejectsHTTPManifest 只接受 https 或本地路径。
func TestFetchRejectsHTTPManifest(t *testing.T) {
	_, err := Fetch(context.Background(), FetchRequest{
		Key:            "demo",
		ManifestSource: "http://example.com/manifest.json",
		DestDir:        t.TempDir(),
	})
	if !errors.Is(err, ErrManifestSource) {
		t.Fatalf("期望 ErrManifestSource，得到 %v", err)
	}
}

// TestFetchPlatformMissing 清单里没有当前平台时应给出明确错误。
func TestFetchPlatformMissing(t *testing.T) {
	dir := t.TempDir()
	c := component.Component{
		Schema: 1,
		Key:    "demo",
		Name:   "Demo",
		Versions: []component.Version{
			{
				Version: "1.0.0",
				Platforms: map[string]component.Platform{
					"linux-s390x": {
						InstallKind: component.InstallArchive,
						File:        "x.zip",
						Mirrors:     []string{"https://example.com"},
						Archive:     component.ArchiveSpec{Format: component.FormatZip},
					},
				},
				Verify: &component.Verify{Command: "demo"},
			},
		},
	}
	manifestPath := writeManifest(t, dir, c)

	_, err := Fetch(context.Background(), FetchRequest{
		Key:            "demo",
		ManifestSource: manifestPath,
		DestDir:        filepath.Join(dir, "dl"),
	})
	if !errors.Is(err, ErrPlatformMissing) {
		t.Fatalf("期望 ErrPlatformMissing，得到 %v", err)
	}
}

// itoa 是 strconv.Itoa 的简写替身，避免为一行引入额外导入。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
