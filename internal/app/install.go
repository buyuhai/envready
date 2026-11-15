// 本文件负责：把"清单 → 安装 → 环境变量"串成一次安装操作。
// 边界：不含命令行解析；图形界面在 S3 复用同一入口。

package app

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/buyuhai/envready/internal/component"
	"github.com/buyuhai/envready/internal/download"
	"github.com/buyuhai/envready/internal/envvar"
	"github.com/buyuhai/envready/internal/install"
	"github.com/buyuhai/envready/internal/manifest"
)

// InstallRequest 描述一次安装请求。
type InstallRequest struct {
	// Key 是组件标识，用于与清单内容核对。
	Key string
	// Version 为空时使用推荐版本。
	Version string
	// ManifestSource 是组件清单位置：本地路径或 https 地址。
	ManifestSource string
	// EnvRoot 是环境根目录。
	EnvRoot string
	// Downloads 是安装包缓存目录。
	Downloads string
	// BackupDir 是环境变量备份目录。
	BackupDir string
	// SkipEnv 为 true 时只安装、不写环境变量。
	SkipEnv bool
	// Force 允许覆盖已存在的同版本。
	Force bool
	// Client 可注入 HTTP 客户端。
	Client *http.Client
	// Progress 是下载进度回调。
	Progress func(download.Progress)
	// Runner 可注入命令执行器（测试用）。
	Runner install.Runner
}

// InstallResult 是一次安装的结果。
type InstallResult struct {
	Component     string
	Version       string
	Dir           string
	Verified      bool
	VerifyVersion string
	EnvChanged    bool
	BackupFile    string
	// PostNotes 是装完之后必须展示给用户的人工操作提示。
	PostNotes []string
}

// Install 执行一次完整安装：下载 → 校验 → 解压 → 自检 → 写环境变量。
//
// 失败：清单读取/解析失败、平台不支持、安装失败、自检失败（已回滚）、环境变量写入失败。
// 副作用：在 EnvRoot 下创建版本目录；必要时修改用户环境变量并写备份。
func Install(ctx context.Context, req InstallRequest) (InstallResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := loadManifest(ctx, req.ManifestSource, req.Client)
	if err != nil {
		return InstallResult{}, err
	}
	c, err := manifest.ParseComponent(raw)
	if err != nil {
		return InstallResult{}, err
	}
	if req.Key != "" && c.Key != req.Key {
		return InstallResult{}, fmt.Errorf("清单里是组件 %q，与请求的 %q 不一致", c.Key, req.Key)
	}

	ver, ok := pickVersion(c, req.Version)
	if !ok {
		return InstallResult{}, fmt.Errorf("组件 %s 没有可用版本", c.Key)
	}
	plat, ok := ver.PlatformFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return InstallResult{}, fmt.Errorf("%w: %s（%s）", ErrPlatformMissing,
			component.PlatformKey(runtime.GOOS, runtime.GOARCH), ver.Version)
	}
	if plat.InstallKind != component.InstallArchive {
		return InstallResult{}, fmt.Errorf("%w: install_kind=%s（首期只支持 archive）", ErrUnsupportedKind, plat.InstallKind)
	}

	res, err := install.Run(ctx, install.Request{
		Component: c,
		Version:   ver,
		Platform:  plat,
		EnvRoot:   req.EnvRoot,
		Downloads: req.Downloads,
		Client:    req.Client,
		Progress:  req.Progress,
		Force:     req.Force,
		Runner:    req.Runner,
	})
	if err != nil {
		return InstallResult{}, err
	}

	out := InstallResult{
		Component:     c.Key,
		Version:       ver.Version,
		Dir:           res.Dir,
		Verified:      res.Verified,
		VerifyVersion: res.VerifyVersion,
		PostNotes:     ver.PostNotes,
	}

	// 环境变量：目录取"版本目录 + 清单里的子目录"，绝不写相对路径
	dirs := make([]string, 0, len(c.Env.Path.Dirs))
	for _, d := range c.Env.Path.Dirs {
		dirs = append(dirs, filepath.Join(res.Dir, d))
	}
	if !req.SkipEnv && (len(dirs) > 0 || strings.TrimSpace(c.Env.HomeVar) != "") {
		change, aerr := envvar.Apply(envvar.Plan{
			Component: c.Key,
			HomeVar:   c.Env.HomeVar,
			HomeValue: res.Dir,
			Dirs:      dirs,
			Position:  c.Env.Path.Position,
			BackupDir: req.BackupDir,
		})
		if aerr != nil {
			// 程序已经装好，只是环境没生效：保留目录，明确告知而不是假装成功
			return out, fmt.Errorf("程序已安装到 %s，但写入环境变量失败：%w", res.Dir, aerr)
		}
		out.EnvChanged = true
		out.BackupFile = change.BackupFile
	}
	return out, nil
}
