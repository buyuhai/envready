// 本文件负责：archive 包的单元测试。
// 重点覆盖三件事：格式识别不看扩展名、strip_root 的两种结果、以及路径穿越必须被拒绝。

package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// makeZip 构造一个 zip 夹具。
func makeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 zip 失败: %v", err)
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("写入 zip 条目失败: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("写入 zip 内容失败: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
}

// makeTar 构造一个 tar 或 tar.gz 夹具。
func makeTar(t *testing.T, path string, files map[string]string, gzipped bool) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 tar 失败: %v", err)
	}
	defer func() { _ = f.Close() }()

	var w = &nopWriteCloser{f}
	if gzipped {
		gz := gzip.NewWriter(f)
		defer func() { _ = gz.Close() }()
		w = &nopWriteCloser{gz}
	}
	tw := tar.NewWriter(w)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("写入 tar 头失败: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("写入 tar 内容失败: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关闭 tar 失败: %v", err)
	}
}

// nopWriteCloser 把 io.Writer 适配成 io.WriteCloser（tar/gzip 需要）。
type nopWriteCloser struct {
	w interface{ Write([]byte) (int, error) }
}

func (n *nopWriteCloser) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n *nopWriteCloser) Close() error                { return nil }

// TestDetect 格式识别必须看文件头，而不是扩展名。
func TestDetect(t *testing.T) {
	dir := t.TempDir()

	zipPath := filepath.Join(dir, "a.bin") // 故意用错误扩展名
	makeZip(t, zipPath, map[string]string{"x.txt": "hi"})

	tarGzPath := filepath.Join(dir, "b.zip")
	makeTar(t, tarGzPath, map[string]string{"x.txt": "hi"}, true)

	tarPath := filepath.Join(dir, "c.dat")
	makeTar(t, tarPath, map[string]string{"x.txt": "hi"}, false)

	plain := filepath.Join(dir, "d.txt")
	if err := os.WriteFile(plain, []byte("just text, not an archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want Format
	}{
		{zipPath, FormatZip},
		{tarGzPath, FormatTarGz},
		{tarPath, FormatTar},
		{plain, FormatUnknown},
	}
	for _, tc := range cases {
		got, err := Detect(tc.path)
		if err != nil {
			t.Fatalf("Detect(%s) 失败: %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("Detect(%s) = %s, 期望 %s", filepath.Base(tc.path), got, tc.want)
		}
	}
}

// TestExtractZipWithStripRoot 覆盖剥与不剥两种情况。
func TestExtractZipWithStripRoot(t *testing.T) {
	files := map[string]string{
		"pkg-1.0/bin/tool.exe": "binary",
		"pkg-1.0/README.txt":   "readme",
	}

	cases := []struct {
		name   string
		strip  bool
		expect string
	}{
		{"不剥顶层目录", false, "pkg-1.0/bin/tool.exe"},
		{"剥掉顶层目录", true, "bin/tool.exe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "pkg.zip")
			makeZip(t, src, files)
			dst := filepath.Join(dir, "out")

			if err := Extract(context.Background(), src, dst, Options{StripRoot: tc.strip}); err != nil {
				t.Fatalf("解压失败: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(tc.expect)))
			if err != nil {
				t.Fatalf("期望文件不存在: %v", err)
			}
			if string(got) != "binary" {
				t.Errorf("内容 = %q", string(got))
			}
		})
	}
}

// TestExtractTarGz 覆盖 tar.gz 的解压与剥目录。
func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg.tar.gz")
	makeTar(t, src, map[string]string{
		"python-3.12/bin/python": "binary",
		"python-3.12/LICENSE":    "license",
	}, true)

	dst := filepath.Join(dir, "out")
	if err := Extract(context.Background(), src, dst, Options{StripRoot: true}); err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "bin", "python")); err != nil {
		t.Errorf("剥目录后 bin/python 应存在: %v", err)
	}
}

// TestExtractRejectsTraversal 越界条目必须拒绝，且不能在目标目录外留下文件。
func TestExtractRejectsTraversal(t *testing.T) {
	cases := []struct {
		name    string
		entry   string
		wantErr error
	}{
		{"相对越界", "../evil.txt", ErrUnsafeEntry},
		{"多级越界", "../../evil.txt", ErrUnsafeEntry},
		{"绝对路径", "/tmp/evil.txt", ErrUnsafeEntry},
		{"盘符", `C:/Windows/evil.txt`, ErrUnsafeEntry},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "pkg.zip")
			makeZip(t, src, map[string]string{tc.entry: "pwned"})

			dst := filepath.Join(dir, "out")
			err := Extract(context.Background(), src, dst, Options{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v，得到 %v", tc.wantErr, err)
			}
			// 目标目录的上级不应出现 evil.txt（特别注意 ../ 的情况）
			if _, serr := os.Stat(filepath.Join(dir, "evil.txt")); !os.IsNotExist(serr) {
				t.Error("越界文件被写到了目标目录之外")
			}
		})
	}
}

// TestExtractRejectsSymlink tar 里的符号链接一律拒绝。
func TestExtractRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg.tar")

	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	hdr := &tar.Header{Name: "link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = Extract(context.Background(), src, filepath.Join(dir, "out"), Options{})
	if !errors.Is(err, ErrLinkEntry) {
		t.Fatalf("期望 ErrLinkEntry，得到 %v", err)
	}
}

// TestStripRootWithDirEntry 真实包（如 winlibs）第一条是目录条目 "mingw64/"，
// 这种"顶层目录条目 + 其下文件"的组合必须能正确剥掉顶层目录。
func TestStripRootWithDirEntry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg.zip")

	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 先写一条目录条目，再写其中的文件——与真实 zip 的结构一致
	if _, err := zw.Create("mingw64/"); err != nil {
		t.Fatal(err)
	}
	w, err := zw.Create("mingw64/bin/gcc.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("gcc")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "out")
	if err := Extract(context.Background(), src, dst, Options{StripRoot: true}); err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "bin", "gcc.exe")); err != nil {
		t.Errorf("剥掉顶层目录后应为 bin/gcc.exe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "mingw64")); !os.IsNotExist(err) {
		t.Error("顶层目录 mingw64 不应保留")
	}
}

// TestCommonRoot 只有"所有条目共享同一顶层目录"时才返回该目录。
func TestCommonRoot(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  string
	}{
		{"共享根", []string{"a/bin/x", "a/README"}, "a"},
		{"共享根（含顶层目录条目）", []string{"mingw64/", "mingw64/bin/gcc.exe"}, "mingw64"},
		{"顶层有文件", []string{"a/bin/x", "README"}, ""},
		{"两个根", []string{"a/x", "b/y"}, ""},
		{"关闭时恒为空", []string{"a/bin/x", "a/README"}, ""},
	}

	for _, tc := range cases {
		enabled := tc.name != "关闭时恒为空"
		if got := commonRoot(tc.names, enabled); got != tc.want {
			t.Errorf("%s: commonRoot = %q, 期望 %q", tc.name, got, tc.want)
		}
	}
}

// TestEntryPath 条目名换算与拒绝规则。
func TestEntryPath(t *testing.T) {
	if rel, skip, err := entryPath("a/b.txt", ""); err != nil || skip || rel != "a/b.txt" {
		t.Errorf("普通条目: rel=%q skip=%v err=%v", rel, skip, err)
	}
	if rel, skip, err := entryPath(`a\b.txt`, ""); err != nil || skip || rel != "a/b.txt" {
		t.Errorf("反斜杠条目: rel=%q skip=%v err=%v", rel, skip, err)
	}
	if _, _, err := entryPath("", ""); err == nil {
		// 空名在实现里被当作"跳过"，这里只要求不 panic
		_ = err
	}
	if _, _, err := entryPath("a\x00b", ""); !errors.Is(err, ErrUnsafeEntry) {
		t.Errorf("含 NUL 应被拒绝，得到 %v", err)
	}
	if _, _, err := entryPath("..", ""); !errors.Is(err, ErrUnsafeEntry) {
		t.Errorf(".. 应被拒绝，得到 %v", err)
	}
}
