// 本文件负责：download 包的单元测试。
// 用 httptest 构造各种"真实世界里会遇到"的镜像行为：正常、校验和错、忽略 Range、
// 不支持 Range、无 Content-Length、首个镜像挂掉、中途取消。

package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testContent 是被下载的假文件内容。
var testContent = []byte(strings.Repeat("EnvReady-test-payload-", 500))

// testSHA 返回 testContent 的 sha256。
func testSHA() string {
	sum := sha256.Sum256(testContent)
	return hex.EncodeToString(sum[:])
}

// mirrorServer 是一个可配置的假镜像。
type mirrorServer struct {
	content      []byte
	supportRange bool // 是否响应 206
	ignoreRange  bool // 是否无视 Range 直接 200
	status       int  // 非 0 时固定返回该状态码
	chunked      bool // 是否不给 Content-Length
	delay        time.Duration

	mu       sync.Mutex
	ranges   []string
	bodySize int64
}

func (m *mirrorServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-r.Context().Done():
			return
		}
	}
	if m.status != 0 {
		w.WriteHeader(m.status)
		return
	}

	rng := r.Header.Get("Range")
	m.mu.Lock()
	if rng != "" {
		m.ranges = append(m.ranges, rng)
	}
	m.mu.Unlock()

	if m.ignoreRange {
		m.writeAll(w, 200)
		return
	}

	start := int64(0)
	if m.supportRange && strings.HasPrefix(rng, "bytes=") {
		spec := strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-")
		if i, err := strconv.ParseInt(spec, 10, 64); err == nil {
			start = i
		}
		if start >= int64(len(m.content)) {
			w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(len(m.content)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		body := m.content[start:]
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.Itoa(len(m.content)-1)+"/"+strconv.Itoa(len(m.content)))
		m.writeAll(w, http.StatusPartialContent, body...)
		return
	}
	m.writeAll(w, 200)
}

// writeAll 写响应体并记录字节数。
func (m *mirrorServer) writeAll(w http.ResponseWriter, status int, body ...byte) {
	if len(body) == 0 {
		body = m.content
	}
	if !m.chunked {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	n, _ := w.Write(body)
	m.mu.Lock()
	m.bodySize += int64(n)
	m.mu.Unlock()
}

// sawRange 返回是否收到过指定的 Range 请求头。
func (m *mirrorServer) sawRange(want string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.ranges {
		if r == want {
			return true
		}
	}
	return false
}

// newServer 启动假镜像并返回地址。
func newServer(t *testing.T, m *mirrorServer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return srv
}

// baseOptions 构造一份指向给定镜像的基础参数。
func baseOptions(dir string, mirrors ...string) Options {
	return Options{
		Mirrors: mirrors,
		File:    "pkg.zip",
		DestDir: dir,
		SHA256:  testSHA(),
		Size:    int64(len(testContent)),
	}
}

// TestFetchSuccess 正常下载：校验通过、没有残留 .part。
func TestFetchSuccess(t *testing.T) {
	m := &mirrorServer{content: testContent, supportRange: true}
	srv := newServer(t, m)

	dir := t.TempDir()
	res, err := Fetch(context.Background(), baseOptions(dir, srv.URL))
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if !res.Verified {
		t.Error("Verified 应为 true")
	}
	if res.Size != int64(len(testContent)) {
		t.Errorf("Size = %d", res.Size)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("读取结果文件失败: %v", err)
	}
	if string(got) != string(testContent) {
		t.Error("文件内容不一致")
	}
	if _, err := os.Stat(res.Path + ".part"); !os.IsNotExist(err) {
		t.Error(".part 应已被改名，不应残留")
	}
	if _, err := os.Stat(res.Path + ".part.meta"); !os.IsNotExist(err) {
		t.Error(".part.meta 应已删除")
	}
}

// TestFetchChecksumMismatch 校验和不符时必须失败，且不留下半成品。
func TestFetchChecksumMismatch(t *testing.T) {
	m := &mirrorServer{content: testContent, supportRange: true}
	srv := newServer(t, m)

	dir := t.TempDir()
	opts := baseOptions(dir, srv.URL)
	opts.SHA256 = strings.Repeat("0", 64)

	_, err := Fetch(context.Background(), opts)
	if !errors.Is(err, ErrAllMirrors) {
		t.Fatalf("期望 ErrAllMirrors，得到 %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "pkg.zip")); !os.IsNotExist(serr) {
		t.Error("校验失败时不应生成最终文件")
	}
	if _, serr := os.Stat(filepath.Join(dir, "pkg.zip.part")); !os.IsNotExist(serr) {
		t.Error("校验失败时应清掉 .part")
	}
}

// TestFetchMirrorFailover 第一个镜像 500，应自动换第二个。
func TestFetchMirrorFailover(t *testing.T) {
	bad := &mirrorServer{status: http.StatusInternalServerError}
	good := &mirrorServer{content: testContent, supportRange: true}
	badSrv := newServer(t, bad)
	goodSrv := newServer(t, good)

	dir := t.TempDir()
	res, err := Fetch(context.Background(), baseOptions(dir, badSrv.URL, goodSrv.URL))
	if err != nil {
		t.Fatalf("应回退到可用镜像，实际失败: %v", err)
	}
	if res.Mirror != goodSrv.URL+"/pkg.zip" {
		t.Errorf("使用的镜像 = %q", res.Mirror)
	}
}

// TestFetchResume 已有半截 .part 时应只请求剩余部分。
func TestFetchResume(t *testing.T) {
	const have = 3000
	m := &mirrorServer{content: testContent, supportRange: true}
	srv := newServer(t, m)

	dir := t.TempDir()
	opts := baseOptions(dir, srv.URL)

	partPath := filepath.Join(dir, "pkg.zip.part")
	if err := os.WriteFile(partPath, testContent[:have], 0o644); err != nil {
		t.Fatalf("预置 .part 失败: %v", err)
	}
	if err := writeMeta(partPath+".meta", opts); err != nil {
		t.Fatalf("预置 meta 失败: %v", err)
	}

	res, err := Fetch(context.Background(), opts)
	if err != nil {
		t.Fatalf("续传失败: %v", err)
	}
	if !m.sawRange("bytes=" + strconv.Itoa(have) + "-") {
		t.Error("没有发出从断点开始的 Range 请求")
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("读取结果失败: %v", err)
	}
	if string(got) != string(testContent) {
		t.Error("续传后内容不一致（前半段可能没被计入或重复写入）")
	}
	if !res.Verified {
		t.Error("续传后仍应校验通过")
	}
}

// TestFetchIgnoresRange 服务端无视 Range 时必须从头重下，不能拼接出错。
func TestFetchIgnoresRange(t *testing.T) {
	m := &mirrorServer{content: testContent, ignoreRange: true}
	srv := newServer(t, m)

	dir := t.TempDir()
	opts := baseOptions(dir, srv.URL)

	partPath := filepath.Join(dir, "pkg.zip.part")
	if err := os.WriteFile(partPath, testContent[:100], 0o644); err != nil {
		t.Fatalf("预置 .part 失败: %v", err)
	}
	if err := writeMeta(partPath+".meta", opts); err != nil {
		t.Fatalf("预置 meta 失败: %v", err)
	}

	res, err := Fetch(context.Background(), opts)
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if string(got) != string(testContent) {
		t.Error("服务端忽略 Range 时应从头重下，内容必须一致")
	}
}

// TestFetchUnknownSize 无 Content-Length 时应标记 UnknownTotal，且内容正确。
func TestFetchUnknownSize(t *testing.T) {
	m := &mirrorServer{content: testContent, supportRange: true, chunked: true}
	srv := newServer(t, m)

	dir := t.TempDir()
	var sawUnknown bool
	opts := baseOptions(dir, srv.URL)
	opts.Progress = func(p Progress) {
		if p.UnknownTotal {
			sawUnknown = true
		}
	}

	if _, err := Fetch(context.Background(), opts); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if !sawUnknown {
		t.Error("没有 Content-Length 时应报告 UnknownTotal")
	}
}

// TestFetchCancel 上下文取消后应尽快返回，且不产生最终文件。
func TestFetchCancel(t *testing.T) {
	m := &mirrorServer{content: testContent, supportRange: true, delay: 2 * time.Second}
	srv := newServer(t, m)

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := Fetch(ctx, baseOptions(dir, srv.URL))
	if err == nil {
		t.Fatal("取消后应返回错误")
	}
	if _, serr := os.Stat(filepath.Join(dir, "pkg.zip")); !os.IsNotExist(serr) {
		t.Error("取消时不应生成最终文件")
	}
}

// TestValidateOptions 参数校验。
func TestValidateOptions(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want error
	}{
		{"没有镜像", Options{File: "a", DestDir: "d"}, ErrNoMirrors},
		{"没有文件名", Options{Mirrors: []string{"https://x"}, DestDir: "d"}, ErrInvalidOptions},
		{"文件名含分隔符", Options{Mirrors: []string{"https://x"}, File: "a/b", DestDir: "d"}, ErrInvalidOptions},
		{"没有目标目录", Options{Mirrors: []string{"https://x"}, File: "a"}, ErrInvalidOptions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Fetch(context.Background(), tc.opts)
			if !errors.Is(err, tc.want) {
				t.Errorf("期望 %v，得到 %v", tc.want, err)
			}
		})
	}
}

// TestJoinURL 覆盖前缀与文件名的斜杠组合。
func TestJoinURL(t *testing.T) {
	cases := []struct{ prefix, file, want string }{
		{"https://a/b", "f.zip", "https://a/b/f.zip"},
		{"https://a/b/", "f.zip", "https://a/b/f.zip"},
		{"https://a/b", "/f.zip", "https://a/b/f.zip"},
		{" https://a/b/ ", " f.zip ", "https://a/b/f.zip"},
	}
	for _, tc := range cases {
		if got := joinURL(tc.prefix, tc.file); got != tc.want {
			t.Errorf("joinURL(%q,%q) = %q, 期望 %q", tc.prefix, tc.file, got, tc.want)
		}
	}
}

// TestContentRangeStart 解析 Content-Range 起点。
func TestContentRangeStart(t *testing.T) {
	if v, err := contentRangeStart("bytes 3000-1499/1500"); err != nil || v != 3000 {
		t.Errorf("解析结果 = %d, err=%v", v, err)
	}
	for _, bad := range []string{"", "items 0-1/2", "bytes abc-1/2"} {
		if _, err := contentRangeStart(bad); err == nil {
			t.Errorf("%q 应解析失败", bad)
		}
	}
}
