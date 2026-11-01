// 本文件负责：把安装包从镜像下载到本地，支持断点续传、边下边校验、进度回调与取消。
// 边界：不解析清单、不解压、不安装。只保证"拿到的文件字节正确、大小正确"。
//
// 关键设计（详见设计文档 06）：
//   - 只用 GET + Range，不用 HEAD（很多镜像对 HEAD 的处理与 GET 不一致）；
//   - 分层超时：连接 10s / 首字节 30s / 空闲 60s，且不设整体超时（否则大文件会被误杀）；
//   - 先写 .part + .part.meta，校验通过后才原子改名，绝不把半截文件当成品；
//   - 边下边算 SHA-256，避免"下完了才发现不对"。

package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// connectTimeout 是建立 TCP 连接的超时。
	connectTimeout = 10 * time.Second
	// firstByteTimeout 是"发出请求到收到响应头"的超时。
	firstByteTimeout = 30 * time.Second
	// idleTimeout 是两次收到数据之间的最大间隔；超过即认为连接已死。
	idleTimeout = 60 * time.Second
	// progressInterval 控制进度回调的最小间隔，避免回调过密。
	progressInterval = 200 * time.Millisecond
	// defaultRetries 是每个镜像的默认重试次数。
	defaultRetries = 2
	// readBufferSize 是单次读取块大小。
	readBufferSize = 256 * 1024
)

var (
	// ErrNoMirrors 表示调用方没有提供任何镜像地址。
	ErrNoMirrors = errors.New("没有提供下载镜像")
	// ErrInvalidOptions 表示参数不合法（缺少文件名或目录等）。
	ErrInvalidOptions = errors.New("下载参数不合法")
	// ErrChecksum 表示下载完成后校验和不匹配。
	ErrChecksum = errors.New("文件校验和不匹配")
	// ErrSize 表示下载得到的大小与清单声明的不一致。
	ErrSize = errors.New("文件大小与清单不符")
)

// Options 描述一次下载任务。
type Options struct {
	// Mirrors 是镜像地址前缀列表；客户端先测速再择优。
	Mirrors []string
	// File 是文件名（与镜像前缀拼成完整地址，也用作本地文件名）。
	File string
	// DestDir 是最终文件与 .part 所在的目录。
	DestDir string
	// SHA256 是期望的校验和；为空表示上游未提供，此时不做比对（界面必须提示"未校验"）。
	SHA256 string
	// Size 是期望字节数；0 表示未知。
	Size int64
	// Headers 是附加请求头（如防盗链需要的 Referer）。
	Headers map[string]string
	// Client 可注入自定义 HTTP 客户端（测试用）；nil 时用带分层超时的默认客户端。
	Client *http.Client
	// Progress 是进度回调，可为 nil。
	Progress func(Progress)
	// Retries 是每个镜像的重试次数；<=0 时取默认值 2。
	Retries int
}

// Progress 是一次进度汇报。
type Progress struct {
	// Downloaded 是已下载字节数（含续传前已有的部分）。
	Downloaded int64
	// Total 是预计总字节数；UnknownTotal 为 true 时该值不可信。
	Total int64
	// UnknownTotal 表示服务端没给 Content-Length，因此界面上不显示百分比。
	UnknownTotal bool
	// Mirror 是当前使用的镜像地址。
	Mirror string
}

// Result 是下载成功后的结果。
type Result struct {
	// Path 是最终文件路径。
	Path string
	// Size 是实际字节数。
	Size int64
	// SHA256 是实际校验和（即使未校验也给出，便于写入日志）。
	SHA256 string
	// Verified 表示是否与清单提供的校验和比对通过。
	Verified bool
	// Mirror 是最终成功下载所用的镜像。
	Mirror string
}

// DefaultClient 返回带分层超时的 HTTP 客户端。
//
// 刻意不设置 http.Client.Timeout：它限制的是"整个请求"，
// 而大文件下载本就该持续很久；真正需要限制的是连接、首字节与空闲间隔。
func DefaultClient() *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   connectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: firstByteTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{Transport: tr}
}

// meta 是断点续传的元数据，与 .part 文件成对存在。
//
// 为什么要它：只看 .part 的大小无法判断"这半截文件是不是同一个版本的"。
// 上游换包之后接着旧 .part 写，会得到一个大小对、内容错的文件。
type meta struct {
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Fetch 执行一次完整下载。
//
// 失败：参数非法、所有镜像失败、校验和不符、磁盘错误、上下文被取消。
// 副作用：在 DestDir 下创建 .part/.part.meta，成功时改名为最终文件并删除元数据。
func Fetch(ctx context.Context, opts Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateOptions(opts); err != nil {
		return Result{}, err
	}
	client := opts.Client
	if client == nil {
		client = DefaultClient()
	}
	if err := os.MkdirAll(opts.DestDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("创建下载目录失败: %w", err)
	}

	dest := filepath.Join(opts.DestDir, opts.File)
	partPath := dest + ".part"
	metaPath := partPath + ".meta"

	urls := make([]string, 0, len(opts.Mirrors))
	for _, m := range opts.Mirrors {
		urls = append(urls, joinURL(m, opts.File))
	}

	candidates := probeMirrors(ctx, client, urls, opts.Headers)
	if len(candidates) == 0 {
		return Result{}, ErrAllMirrors
	}

	retries := opts.Retries
	if retries <= 0 {
		retries = defaultRetries
	}

	offset := loadResumeOffset(partPath, metaPath, opts)

	var lastErr error
	for _, c := range candidates {
		for attempt := 0; attempt <= retries; attempt++ {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}

			size, sum, err := fetchTo(ctx, partPath, metaPath, c.url, offset, opts, client)
			if err == nil {
				res, ferr := finish(dest, partPath, metaPath, c.url, size, sum, opts)
				if ferr == nil {
					return res, nil
				}
				err = ferr
			}
			lastErr = err

			// 校验失败说明这个镜像的包不对（或 .part 被污染）：
			// 丢掉半截文件，换下一个镜像从头下，而不是在坏数据上继续追加
			if errors.Is(err, ErrChecksum) || errors.Is(err, ErrSize) {
				resetPart(partPath, metaPath)
				offset = 0
				break
			}

			// 网络中断：保留已写入的部分，下次从新长度继续
			if n, ok := currentSize(partPath); ok {
				offset = n
			}
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			sleepBackoff(ctx, attempt)
		}
	}

	if lastErr == nil {
		lastErr = ErrAllMirrors
	}
	return Result{}, fmt.Errorf("%w: %v", ErrAllMirrors, lastErr)
}

// validateOptions 检查必需参数。
func validateOptions(opts Options) error {
	switch {
	case len(opts.Mirrors) == 0:
		return ErrNoMirrors
	case strings.TrimSpace(opts.File) == "":
		return fmt.Errorf("%w: 缺少文件名", ErrInvalidOptions)
	case strings.ContainsAny(opts.File, `/\`):
		return fmt.Errorf("%w: 文件名不能包含路径分隔符", ErrInvalidOptions)
	case strings.TrimSpace(opts.DestDir) == "":
		return fmt.Errorf("%w: 缺少目标目录", ErrInvalidOptions)
	}
	return nil
}

// fetchTo 从单个地址下载到 .part，返回最终文件大小与 SHA-256。
//
// 失败：网络错误、HTTP 状态异常、写盘失败、上下文取消。
func fetchTo(ctx context.Context, partPath, metaPath, url string, offset int64, opts Options, client *http.Client) (int64, string, error) {
	if err := writeMeta(metaPath, opts); err != nil {
		return 0, "", fmt.Errorf("写入续传元数据失败: %w", err)
	}

	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, "", fmt.Errorf("打开 .part 失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, "", fmt.Errorf("定位到续传位置失败: %w", err)
		}
	} else {
		if err := f.Truncate(0); err != nil {
			return 0, "", fmt.Errorf("清空 .part 失败: %w", err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, "", fmt.Errorf("定位到文件头失败: %w", err)
		}
	}

	// 续传时先把已存在的字节计入哈希，否则最终摘要会缺前半段
	h := sha256.New()
	if offset > 0 {
		if err := hashPrefix(partPath, h); err != nil {
			return 0, "", err
		}
	}

	// 空闲看门狗：连续 idleTimeout 没有数据就取消本次请求
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(idleTimeout, cancel)
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return offset, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// 续传正常：校验服务端给的起点是否与我们请求的一致
		start, err := contentRangeStart(resp.Header.Get("Content-Range"))
		if err != nil || start != offset {
			return offset, "", fmt.Errorf("服务端返回的续传区间不匹配：%q", resp.Header.Get("Content-Range"))
		}
	case http.StatusOK:
		// 服务端忽略了 Range（或不支持续传）：只能从头写
		if offset > 0 {
			if err := f.Truncate(0); err != nil {
				return 0, "", fmt.Errorf("清空 .part 失败: %w", err)
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return 0, "", fmt.Errorf("从头定位失败: %w", err)
			}
			offset = 0
			h = sha256.New()
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// 已下完（或超过总长）：交给 finish 去校验
		size, _ := currentSize(partPath)
		sum, herr := hashFile(partPath)
		return size, sum, herr
	default:
		return offset, "", fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}

	total := int64(-1)
	if resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}
	reportProgress(opts.Progress, Progress{Downloaded: offset, Total: total, UnknownTotal: total < 0, Mirror: url})

	buf := make([]byte, readBufferSize)
	written := offset
	var lastReport time.Time
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(idleTimeout)
			if _, werr := f.Write(buf[:n]); werr != nil {
				return written, "", fmt.Errorf("写入磁盘失败: %w", werr)
			}
			_, _ = h.Write(buf[:n])
			written += int64(n)

			if opts.Progress != nil && time.Since(lastReport) >= progressInterval {
				lastReport = time.Now()
				reportProgress(opts.Progress, Progress{
					Downloaded:   written,
					Total:        total,
					UnknownTotal: total < 0,
					Mirror:       url,
				})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return written, "", rerr
		}
	}

	if err := f.Sync(); err != nil {
		return written, "", fmt.Errorf("落盘失败: %w", err)
	}
	reportProgress(opts.Progress, Progress{Downloaded: written, Total: written, Mirror: url})
	return written, hex.EncodeToString(h.Sum(nil)), nil
}

// finish 校验并把 .part 改名为最终文件。
func finish(dest, partPath, metaPath, mirror string, size int64, sum string, opts Options) (Result, error) {
	if opts.SHA256 != "" && !strings.EqualFold(sum, opts.SHA256) {
		resetPart(partPath, metaPath)
		return Result{}, fmt.Errorf("%w: 期望 %s，实际 %s", ErrChecksum, opts.SHA256, sum)
	}
	if opts.Size > 0 && size != opts.Size {
		resetPart(partPath, metaPath)
		return Result{}, fmt.Errorf("%w: 期望 %d，实际 %d", ErrSize, opts.Size, size)
	}
	if err := os.Rename(partPath, dest); err != nil {
		return Result{}, fmt.Errorf("重命名到最终文件失败: %w", err)
	}
	_ = os.Remove(metaPath)
	return Result{Path: dest, Size: size, SHA256: sum, Verified: opts.SHA256 != "", Mirror: mirror}, nil
}

// reportProgress 安全地调用进度回调。
func reportProgress(fn func(Progress), p Progress) {
	if fn == nil {
		return
	}
	fn(p)
}

// hashFile 计算整个文件的 SHA-256。
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashPrefix 把文件已有内容计入哈希（续传用）。
func hashPrefix(path string, h hash.Hash) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开续传文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("读取续传文件失败: %w", err)
	}
	return nil
}

// contentRangeStart 从 "bytes 100-199/2000" 里取出起点 100。
func contentRangeStart(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, fmt.Errorf("无法解析 Content-Range: %q", v)
	}
	rest := strings.TrimPrefix(v, "bytes ")
	dash := strings.IndexByte(rest, '-')
	if dash < 0 {
		return 0, fmt.Errorf("无法解析 Content-Range: %q", v)
	}
	return strconv.ParseInt(rest[:dash], 10, 64)
}

// loadResumeOffset 判断能否续传，返回可续传的字节数。
//
// 只有 .part 与 .meta 都在、且 meta 描述的就是本次要下的东西时才算数；
// 否则一律从 0 开始，避免"续"出一个错误的文件。
func loadResumeOffset(partPath, metaPath string, opts Options) int64 {
	size, ok := currentSize(partPath)
	if !ok || size == 0 {
		return 0
	}
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		resetPart(partPath, metaPath)
		return 0
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		resetPart(partPath, metaPath)
		return 0
	}
	if m.File != opts.File || m.Size != opts.Size || !strings.EqualFold(m.SHA256, opts.SHA256) {
		resetPart(partPath, metaPath)
		return 0
	}
	return size
}

// writeMeta 记录本次下载任务的身份，供下次续传时比对。
func writeMeta(metaPath string, opts Options) error {
	raw, err := json.Marshal(meta{File: opts.File, Size: opts.Size, SHA256: opts.SHA256})
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, raw, 0o644)
}

// resetPart 删除半截文件与元数据。
func resetPart(partPath, metaPath string) {
	_ = os.Remove(partPath)
	_ = os.Remove(metaPath)
}

// currentSize 返回文件大小；文件不存在时返回 false。
func currentSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return 0, false
	}
	return st.Size(), true
}

// sleepBackoff 在重试前等待，且可被上下文取消打断。
func sleepBackoff(ctx context.Context, attempt int) {
	d := time.Duration(attempt+1) * 2 * time.Second
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
