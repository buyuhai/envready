// 本文件负责：把一个已下载的归档"装"成可用目录，并在装完后立刻自检。
// 边界：不下载（download 包）、不解压细节（archive 包）、不写环境变量（envvar 包，S2 第二批）。
//
// 三条硬规则：
//  1. 先解压到同级的临时目录（.staging-*），成功后才原子改名为正式目录——
//     中途失败不会留下"看起来装好了"的半成品；
//  2. 自检不通过就回滚删除，不把不合格的目录留在 ENV_ROOT 里；
//  3. 清单声明"路径不能有空格"时，安装前就拒绝，而不是等用户编译时报错。

package install

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/buyuhai/envready/internal/archive"
	"github.com/buyuhai/envready/internal/component"
	"github.com/buyuhai/envready/internal/download"
)

// runningOnWindows 用于决定是否应用"路径不能含空格"的检查。
var runningOnWindows = runtime.GOOS == "windows"

var (
	// ErrAlreadyInstalled 表示目标版本目录已存在。
	ErrAlreadyInstalled = errors.New("该版本已安装")
	// ErrSpaceInPath 表示安装路径含空格，而该组件在此类路径下无法工作。
	ErrSpaceInPath = errors.New("安装路径包含空格，该组件无法在此路径下正常工作")
	// ErrInvalidRequest 表示安装参数不合法。
	ErrInvalidRequest = errors.New("安装参数不合法")
)

// Runner 是执行外部命令的抽象，测试可注入假实现。
// 返回值是命令的合并输出（stdout+stderr）。
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Request 描述一次安装。
type Request struct {
	// Component 是组件定义。
	Component *component.Component
	// Version 是要装的版本。
	Version *component.Version
	// Platform 是该版本在当前平台上的分发。
	Platform component.Platform
	// EnvRoot 是环境根目录（用户资产，更新不触碰）。
	EnvRoot string
	// Downloads 是安装包缓存目录。
	Downloads string
	// Client 可注入 HTTP 客户端（测试用）。
	Client *http.Client
	// Progress 是下载进度回调。
	Progress func(download.Progress)
	// Force 为 true 时允许覆盖已存在的同版本目录。
	Force bool
	// Runner 可注入命令执行器（测试用）；nil 时用真实进程。
	Runner Runner
}

// Result 是安装结果。
type Result struct {
	Key      string
	Version  string
	Dir      string
	File     string
	SHA256   string
	Verified bool
	// VerifyOutput 是自检命令的原始输出（写日志用）。
	VerifyOutput string
	// VerifyVersion 是从输出里解析出的版本号（可能为空）。
	VerifyVersion string
}

// Run 执行一次完整安装。
//
// 失败：参数非法、路径含空格、已安装、下载/解压失败、自检失败（此时已回滚）、上下文取消。
// 副作用：在 EnvRoot 下创建 <key>/<version> 目录与安装记录；失败时清理残留。
func Run(ctx context.Context, req Request) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Component == nil || req.Version == nil {
		return Result{}, fmt.Errorf("%w: 缺少组件或版本", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.EnvRoot) == "" {
		return Result{}, fmt.Errorf("%w: 缺少环境根目录", ErrInvalidRequest)
	}
	if err := component.ValidKey(req.Component.Key); err != nil {
		return Result{}, err
	}
	// 版本号也会参与路径拼接，沿用同一套"单段路径"校验
	if err := component.ValidKey(req.Version.Version); err != nil {
		return Result{}, fmt.Errorf("版本号不合法: %w", err)
	}

	root, err := filepath.Abs(req.EnvRoot)
	if err != nil {
		return Result{}, fmt.Errorf("解析环境根目录失败: %w", err)
	}
	if err := CheckPathSpace(root, req.Component.Requirements.SpaceFreePath); err != nil {
		return Result{}, err
	}

	target := TargetDir(root, req.Component.Key, req.Version.Version)
	if st, serr := os.Stat(target); serr == nil && st.IsDir() {
		if !req.Force {
			return Result{}, fmt.Errorf("%w: %s", ErrAlreadyInstalled, target)
		}
		if err := os.RemoveAll(target); err != nil {
			return Result{}, fmt.Errorf("清理旧版本失败: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return Result{}, fmt.Errorf("创建组件目录失败: %w", err)
	}

	dl, err := download.Fetch(ctx, download.Options{
		Mirrors:  req.Platform.Mirrors,
		File:     req.Platform.File,
		DestDir:  req.Downloads,
		SHA256:   req.Platform.SHA256,
		Size:     req.Platform.Size,
		Headers:  req.Platform.Headers,
		Client:   req.Client,
		Progress: req.Progress,
	})
	if err != nil {
		return Result{}, err
	}

	// 解压到同级临时目录：保证"要么是完整目录，要么什么都没有"
	staging, err := os.MkdirTemp(filepath.Dir(target), ".staging-")
	if err != nil {
		return Result{}, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := archive.Extract(ctx, dl.Path, staging, archive.Options{StripRoot: req.Platform.Archive.StripRoot}); err != nil {
		return Result{}, fmt.Errorf("解压失败: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		return Result{}, fmt.Errorf("落盘失败: %w", err)
	}

	rec := Record{
		Key:         req.Component.Key,
		Version:     req.Version.Version,
		File:        req.Platform.File,
		SHA256:      dl.SHA256,
		Verified:    dl.Verified,
		InstalledAt: time.Now().UTC(),
		Mirrors:     req.Platform.Mirrors,
	}
	if err := WriteRecord(target, rec); err != nil {
		return Result{}, err
	}

	out, verFound, err := RunVerify(ctx, target, req.Component.Env.Path.Dirs, req.Version.Verify, req.Runner)
	if err != nil {
		// 自检不过 = 这次安装失败：删掉目录，避免用户以为"装好了"
		_ = os.RemoveAll(target)
		return Result{}, fmt.Errorf("安装后自检失败（已回滚）：%w", err)
	}

	return Result{
		Key:           req.Component.Key,
		Version:       req.Version.Version,
		Dir:           target,
		File:          dl.Path,
		SHA256:        dl.SHA256,
		Verified:      dl.Verified,
		VerifyOutput:  out,
		VerifyVersion: verFound,
	}, nil
}

// TargetDir 返回某组件某版本的安装目录。
func TargetDir(envRoot, key, version string) string {
	return filepath.Join(envRoot, key, version)
}

// CheckPathSpace 检查安装路径是否满足"不能有空格"的要求。
//
// 为什么只在 Windows 上判断：这条限制来自 winlibs 的 binutils 在 Windows 上的
// 行为（ld.exe 不给内部路径加引号），Unix 下路径含空格是正常的。
// required 由清单的 requirements.space_free_path 给出，不写死在代码里。
func CheckPathSpace(path string, required bool) error {
	if !required || !runningOnWindows {
		return nil
	}
	if strings.ContainsAny(path, " \t") {
		return fmt.Errorf("%w：%s", ErrSpaceInPath, path)
	}
	return nil
}
