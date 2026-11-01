// 本文件负责：把"清单 → 选镜像 → 下载 → 校验 → 解压"串成一次可复用的操作。
// 边界：不含命令行解析（在 cmd 里），不含图形界面（S3），不做环境变量与安装后处理（S2）。
//
// 为什么单独一层：命令行、测试、将来的界面都要走同一条路径。
// 如果每个入口各自拼流程，很容易出现"命令行能下、界面下不了"的分叉。

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/buyuhai/envready/internal/archive"
	"github.com/buyuhai/envready/internal/component"
	"github.com/buyuhai/envready/internal/download"
	"github.com/buyuhai/envready/internal/manifest"
)

// maxManifestBytes 是清单文件的大小上限（防止被喂一个超大响应）。
const maxManifestBytes = 4 << 20 // 4 MiB

var (
	// ErrUnsupportedKind 表示该安装方式尚未实现（属于计划内，不是缺陷）。
	ErrUnsupportedKind = errors.New("该安装方式尚未实现")
	// ErrPlatformMissing 表示这个版本不支持当前平台。
	ErrPlatformMissing = errors.New("该版本不支持当前平台")
	// ErrManifestSource 表示清单来源不合法（例如 http 明文地址）。
	ErrManifestSource = errors.New("清单来源不合法")
)

// FetchRequest 描述一次取包请求。
type FetchRequest struct {
	// Key 是组件标识；用于和清单内容做一致性校验（防止拿错文件）。
	Key string
	// Version 为空时使用清单里的推荐版本。
	Version string
	// ManifestSource 是组件清单的位置：本地路径或 https:// 地址。
	ManifestSource string
	// DestDir 是安装包的存放目录。
	DestDir string
	// ExtractTo 非空时，下载校验通过后解压到该目录。
	ExtractTo string
	// Progress 是下载进度回调，可为 nil。
	Progress func(download.Progress)
	// Client 是可注入的 HTTP 客户端；nil 时用默认客户端。
	Client *http.Client
}

// FetchResult 是一次取包的完整结果。
type FetchResult struct {
	Component string
	Version   string
	File      string
	Size      int64
	SHA256    string
	Verified  bool
	Extracted string
}

// Fetch 执行一次完整取包。
//
// 失败：清单读取/解析失败、组件不支持当前平台、下载或校验失败、解压失败、上下文取消。
// 副作用：在 DestDir 写入安装包；ExtractTo 非空时在该目录产生文件。
func Fetch(ctx context.Context, req FetchRequest) (FetchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := loadManifest(ctx, req.ManifestSource, req.Client)
	if err != nil {
		return FetchResult{}, err
	}
	c, err := manifest.ParseComponent(raw)
	if err != nil {
		return FetchResult{}, err
	}
	if req.Key != "" && c.Key != req.Key {
		return FetchResult{}, fmt.Errorf("清单里是组件 %q，与请求的 %q 不一致", c.Key, req.Key)
	}

	ver, ok := pickVersion(c, req.Version)
	if !ok {
		return FetchResult{}, fmt.Errorf("组件 %s 没有可用版本", c.Key)
	}

	plat, ok := ver.PlatformFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return FetchResult{}, fmt.Errorf("%w: %s（%s）", ErrPlatformMissing,
			component.PlatformKey(runtime.GOOS, runtime.GOARCH), ver.Version)
	}

	if plat.InstallKind != component.InstallArchive {
		return FetchResult{}, fmt.Errorf("%w: install_kind=%s（首期只支持 archive）", ErrUnsupportedKind, plat.InstallKind)
	}

	res, err := download.Fetch(ctx, download.Options{
		Mirrors:  plat.Mirrors,
		File:     plat.File,
		DestDir:  req.DestDir,
		SHA256:   plat.SHA256,
		Size:     plat.Size,
		Headers:  plat.Headers,
		Client:   req.Client,
		Progress: req.Progress,
	})
	if err != nil {
		return FetchResult{}, err
	}

	out := FetchResult{
		Component: c.Key,
		Version:   ver.Version,
		File:      res.Path,
		Size:      res.Size,
		SHA256:    res.SHA256,
		Verified:  res.Verified,
	}

	if req.ExtractTo != "" {
		if err := archive.Extract(ctx, res.Path, req.ExtractTo, archive.Options{StripRoot: plat.Archive.StripRoot}); err != nil {
			return out, err
		}
		out.Extracted = req.ExtractTo
	}
	return out, nil
}

// pickVersion 按请求选版本；未指定时用推荐版本。
func pickVersion(c *component.Component, want string) (*component.Version, bool) {
	if strings.TrimSpace(want) == "" {
		return c.RecommendedVersion()
	}
	return c.Version(want)
}

// loadManifest 读取清单：支持本地路径与 https 地址。
//
// 只允许 https：清单决定"下载哪个文件、校验和是多少"，
// 明文传输等于把"装什么"的决定权交给了链路上的任何人。
func loadManifest(ctx context.Context, source string, client *http.Client) ([]byte, error) {
	src := strings.TrimSpace(source)
	if src == "" {
		return nil, fmt.Errorf("%w: 未提供清单地址", ErrManifestSource)
	}
	if strings.HasPrefix(src, "http://") {
		return nil, fmt.Errorf("%w: 不接受 http 明文地址", ErrManifestSource)
	}
	if !strings.HasPrefix(src, "https://") {
		return os.ReadFile(src)
	}

	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载清单失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载清单失败：HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
}
