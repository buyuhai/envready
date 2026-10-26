// 本文件负责：按平台推导程序根（APP_ROOT）与环境根（ENV_ROOT），以及它们下面的固定子目录。
// 边界：只计算路径，不创建目录、不检查可写性、不读写任何文件。
// 平台：Windows 用 %LOCALAPPDATA%，macOS 用 ~/Library/Application Support，
// Linux 用 $XDG_DATA_HOME（缺省 ~/.local/share）。
//
// 为什么集中在一个包里：真实用户的主目录可能含空格或中文，路径一旦散落各处，
// 很容易某处漏掉 filepath.Join 而拼出错路径——这类 bug 只会在特定用户名下出现。

package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// appDirName 是 Windows/macOS 下的目录名（区分大小写地展示给用户看）。
	appDirName = "EnvReady"
	// appDirNameUnix 是 Linux 下的目录名（遵循 XDG 惯例用小写）。
	appDirNameUnix = "envready"

	// envAppRoot 是覆盖程序根的调试/便携开关；测试与"绿色版"都靠它。
	envAppRoot = "ENVREADY_APP_ROOT"
	// envEnvRoot 是覆盖环境根的开关。
	envEnvRoot = "ENVREADY_ENV_ROOT"
)

// Layout 是"东西放在哪"的完整答案。
//
// 两个字段的区别是硬性的：
//   - AppRoot 是程序自己的地盘，更新时可以整体切换、可以清理旧版本；
//   - EnvRoot 是用户资产（装好的编译器、Python 等），任何时候都不清理、不迁移。
type Layout struct {
	AppRoot string
	EnvRoot string
}

// New 按当前平台的实际情况推导布局。
//
// 失败：无法确定用户主目录时返回错误（此时调用方应提示用户手动指定目录）。
// 副作用：只读环境变量，不创建任何目录。
func New() (Layout, error) {
	return resolve(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// resolve 是纯函数：平台名、环境变量读取、主目录获取都作为参数传入，
// 因此三个平台的分支都能在任意一台机器上被单元测试覆盖。
func resolve(goos string, getenv func(string) string, homeDir func() (string, error)) (Layout, error) {
	if override := strings.TrimSpace(getenv(envAppRoot)); override != "" {
		appRoot := filepath.Clean(override)
		envRoot := strings.TrimSpace(getenv(envEnvRoot))
		if envRoot == "" {
			envRoot = filepath.Join(appRoot, "envs")
		} else {
			envRoot = filepath.Clean(envRoot)
		}
		return Layout{AppRoot: appRoot, EnvRoot: envRoot}, nil
	}

	appRoot, err := defaultAppRoot(goos, getenv, homeDir)
	if err != nil {
		return Layout{}, err
	}

	envRoot := strings.TrimSpace(getenv(envEnvRoot))
	if envRoot == "" {
		envRoot = filepath.Join(appRoot, "envs")
	} else {
		envRoot = filepath.Clean(envRoot)
	}
	return Layout{AppRoot: appRoot, EnvRoot: envRoot}, nil
}

// defaultAppRoot 给出各平台的默认程序根。
func defaultAppRoot(goos string, getenv func(string) string, homeDir func() (string, error)) (string, error) {
	switch goos {
	case "windows":
		if local := strings.TrimSpace(getenv("LOCALAPPDATA")); local != "" {
			return filepath.Join(local, appDirName), nil
		}
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "AppData", "Local", appDirName), nil

	case "darwin":
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", appDirName), nil

	default:
		// Linux 与其他 Unix：优先 XDG_DATA_HOME
		if xdg := strings.TrimSpace(getenv("XDG_DATA_HOME")); xdg != "" {
			return filepath.Join(xdg, appDirNameUnix), nil
		}
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", appDirNameUnix), nil
	}
}

// PointerFile 返回指针文件路径（记录当前该启动哪个版本）。
func (l Layout) PointerFile() string {
	return filepath.Join(l.AppRoot, "current.json")
}

// AppDir 返回存放各版本主程序的目录。
func (l Layout) AppDir() string {
	return filepath.Join(l.AppRoot, "app")
}

// DownloadsDir 返回下载缓存目录（放 *.part 与已校验的安装包）。
func (l Layout) DownloadsDir() string {
	return filepath.Join(l.AppRoot, "downloads")
}

// LogsDir 返回日志目录。
func (l Layout) LogsDir() string {
	return filepath.Join(l.AppRoot, "logs")
}

// CacheDir 返回清单缓存目录。
func (l Layout) CacheDir() string {
	return filepath.Join(l.AppRoot, "cache")
}

// BackupDir 返回环境变量备份目录。
func (l Layout) BackupDir() string {
	return filepath.Join(l.AppRoot, "env-backup")
}

// AppVersionDir 返回某个版本主程序所在的目录。
//
// 失败：version 含路径分隔符、".." 或为空时返回错误。
// 这层校验是必要的——版本号来自云端清单，属于半可信输入，
// 不加检查就能把文件写到 APP_ROOT 之外。
func (l Layout) AppVersionDir(version string) (string, error) {
	seg, err := safeSegment("版本号", version)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.AppDir(), seg), nil
}

// ComponentDir 返回某个组件在环境根下的目录（其下再按 <key>-<version> 分子目录）。
//
// 失败：key 含路径分隔符、".." 或为空时返回错误。
func (l Layout) ComponentDir(key string) (string, error) {
	seg, err := safeSegment("组件 key", key)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.EnvRoot, seg), nil
}

// safeSegment 校验"将要作为单个路径片段使用"的字符串。
//
// 拒绝：空串、"."、".."、含 '/' 或 '\\'、含 NUL、形如 Windows 盘的绝对路径。
func safeSegment(what, s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	switch {
	case trimmed == "":
		return "", fmt.Errorf("%s 不能为空", what)
	case trimmed == "." || trimmed == "..":
		return "", fmt.Errorf("%s 不能是 %q", what, trimmed)
	case strings.ContainsAny(trimmed, `/\`):
		return "", fmt.Errorf("%s 不能包含路径分隔符：%q", what, s)
	case strings.ContainsRune(trimmed, 0):
		return "", fmt.Errorf("%s 不能包含 NUL 字符", what)
	case filepath.IsAbs(trimmed) || filepath.VolumeName(trimmed) != "":
		return "", fmt.Errorf("%s 不能是绝对路径：%q", what, s)
	}
	return trimmed, nil
}

// ErrNoHomeDir 表示既没有可用的平台目录变量，也拿不到用户主目录。
var ErrNoHomeDir = errors.New("无法确定用户主目录")
