// 本文件负责：定义"组件"与"版本"的数据模型，以及按平台/架构挑选条目。
// 边界：纯数据结构与纯函数。不读文件、不联网、不决定校验策略（那属于 manifest 包）。
//
// 这些字段与设计文档 04（资源清单规范）一一对应，改动字段名等于改协议，
// 必须同步改文档与 tools/ 下的生成脚本。

package component

import (
	"strings"
)

// InstallKind 是"这个包怎么装"的枚举。
// 用类型化常量而不是散落的字符串，避免拼错（拼错的字符串会让分支永远走不到）。
type InstallKind string

const (
	// InstallArchive 表示解压即用（最常见）。
	InstallArchive InstallKind = "archive"
	// InstallInstaller 表示需要运行官方安装器（带静默参数）。
	InstallInstaller InstallKind = "installer"
	// InstallPortable 表示单个可执行文件，放下即可用。
	InstallPortable InstallKind = "portable"
	// InstallNpm 表示通过 npm 安装（需要 Node 作为依赖）。
	InstallNpm InstallKind = "npm"
	// InstallNoop 表示无需安装，只做检测（例如系统自带的工具）。
	InstallNoop InstallKind = "noop"
)

// Valid 判断安装方式是否是已知取值。
func (k InstallKind) Valid() bool {
	switch k {
	case InstallArchive, InstallInstaller, InstallPortable, InstallNpm, InstallNoop:
		return true
	default:
		return false
	}
}

// ArchiveFormat 是归档格式的枚举。
//
// 注意：实际识别一律以文件头魔术字节为准（见 archive 包），
// 这里的字段只用于"预期是什么格式"的交叉检查与界面提示。
type ArchiveFormat string

const (
	FormatZip   ArchiveFormat = "zip"
	FormatTarGz ArchiveFormat = "tar.gz"
	FormatTarXz ArchiveFormat = "tar.xz"
	FormatTar   ArchiveFormat = "tar"
	Format7z    ArchiveFormat = "7z"
	FormatRPM   ArchiveFormat = "rpm"
	FormatBin   ArchiveFormat = "bin"
)

// Valid 判断归档格式是否是已知取值。
func (f ArchiveFormat) Valid() bool {
	switch f {
	case FormatZip, FormatTarGz, FormatTarXz, FormatTar, Format7z, FormatRPM, FormatBin:
		return true
	default:
		return false
	}
}

// Official 记录"这个东西的官方来源"，用于界面展示来源与合规追溯。
// 本项目只做"替用户去官方取"，因此来源必须可追溯。
type Official struct {
	// Page 是官方主页或下载页。
	Page string `json:"page"`
	// Distribution 说明实际使用的是官方推荐的哪个分发（例如 MinGW 用 winlibs）。
	Distribution string `json:"distribution"`
	// MirrorPolicy 说明镜像策略（例如"直取官方与官方镜像，不由本项目分发"）。
	MirrorPolicy string `json:"mirror_policy"`
}

// PathSpec 描述"要加进 PATH 的目录"以及放在前面还是后面。
type PathSpec struct {
	// Dirs 是相对组件安装目录的子目录（通常为 ["bin"]）。
	Dirs []string `json:"dirs"`
	// Position 是 "prepend" 或 "append"；空值按 prepend 处理（见设计文档 07 的 7.5）。
	Position string `json:"position"`
}

// EnvSpec 描述安装完成后要写哪些环境变量。
//
// HomeVar 为空表示"只加 PATH，不设 XXX_HOME"——例如 MinGW 就没有官方要求的 home 变量，
// 我们绝不自己发明一个（参考实现往注册表里塞了大量自造变量且永不清理）。
type EnvSpec struct {
	HomeVar string   `json:"home_var"`
	Path    PathSpec `json:"path"`
}

// ArchiveSpec 描述归档包的处理方式。
type ArchiveSpec struct {
	Format ArchiveFormat `json:"format"`
	// StripRoot 为 true 表示解压后去掉顶层目录（有些包外层套了一层 <name>-<version>/）。
	StripRoot bool `json:"strip_root"`
}

// Verify 描述"装完怎么验证它真的能用"。
//
// 这是本项目相对参考实现的核心差异：进度条走完不等于装好，
// 必须真的跑一次命令并核对版本号（见设计文档 05 的 5.6）。
type Verify struct {
	// Command 是可执行文件名（不含扩展名），例如 "gcc"。
	Command string `json:"command"`
	// Args 是传给它的参数，例如 ["--version"]。
	Args []string `json:"args"`
	// ExpectExit 是期望的退出码；未填时按 0 处理。
	ExpectExit int `json:"expect_exit"`
	// Parse 是从输出里提取版本号的正则；为空表示只验退出码。
	Parse string `json:"parse"`
}

// Platform 是"某个版本在某个平台/架构上的具体分发"。
type Platform struct {
	// InstallKind 决定走哪条安装分支。
	InstallKind InstallKind `json:"install_kind"`
	// File 是文件名（与 Mirrors 里的前缀拼接成完整地址）。
	File string `json:"file"`
	// Size 是字节数，用于下载前空间预检与界面提示。
	Size int64 `json:"size"`
	// SHA256 是完整性校验值；为空表示上游未提供（界面必须显示"未校验"）。
	SHA256 string `json:"sha256"`
	// Headers 是下载时需要附加的请求头（例如防盗链要的 Referer）。
	Headers map[string]string `json:"headers"`
	// Mirrors 是 URL 前缀列表，客户端测速择优。
	Mirrors []string `json:"mirrors"`
	// Archive 是归档处理参数（install_kind 为 archive 时必填）。
	Archive ArchiveSpec `json:"archive"`
}

// Version 是一个可安装的版本。
type Version struct {
	// Version 是逻辑版本标识（如 "14.2.0"）。注意它与实际文件名不一定相同。
	Version string `json:"version"`
	// Label 是界面显示文案（如 "14.2.0（推荐）"）；为空时显示 Version。
	Label string `json:"label"`
	// ReleasedAt 是发布日期的字符串（如 "2026-08-01"），用于界面提示"这是什么时候的版本"。
	ReleasedAt string `json:"released_at"`
	// Platforms 以 "<goos>-<goarch>" 为键，例如 "windows-amd64"、"darwin-arm64"。
	Platforms map[string]Platform `json:"platforms"`
	// PostNotes 是装完之后必须告诉用户的人工操作提示。
	// 必须渲染到界面结果页，不能只写日志（参考实现只打在日志窗里，等于没说）。
	PostNotes []string `json:"post_notes"`
	// Verify 是自检定义；为空表示该版本缺少自检能力（清单校验会拒绝这种条目）。
	Verify *Verify `json:"verify"`
}

// Component 是一个组件的完整定义。
type Component struct {
	// Schema 是清单格式版本。
	Schema int `json:"schema"`
	// Key 是稳定标识（如 "mingw"），用于目录名与环境变量，必须能作为单个路径片段。
	Key string `json:"key"`
	// Name 是界面显示名。
	Name string `json:"name"`
	// Official 是来源信息。
	Official Official `json:"official"`
	// License 是许可证说明（仅作指引，不构成法律意见）。
	License string `json:"license"`
	// Env 是安装后要写的环境变量。
	Env EnvSpec `json:"env"`
	// Requires 是硬依赖的组件 key（安装前会检查）。
	Requires []string `json:"requires"`
	// ConflictsWith 是互斥的组件 key。
	ConflictsWith []string `json:"conflicts_with"`
	// Recommended 是默认选中的版本。
	Recommended string `json:"recommended"`
	// Versions 是可安装版本列表。
	Versions []Version `json:"versions"`
	// VersionsAllURL 指向全量版本列表（可选，避免主清单过大）。
	VersionsAllURL string `json:"versions_all_url"`
	// NotesForUser 是卡片上的一句话说明（用人话讲"这是什么、为什么要装"）。
	NotesForUser string `json:"notes_for_user"`
}

// PlatformKey 把 goos/goarch 归一化成清单里的键，例如 "windows-amd64"。
//
// 归一化是必要的：Go 的 GOARCH 是 amd64/arm64，而清单里可能沿用人习惯的
// x86_64/aarch64；不统一就会出现"明明有包却匹配不到"。
func PlatformKey(goos, goarch string) string {
	return normalizeOS(goos) + "-" + normalizeArch(goarch)
}

func normalizeOS(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "windows":
		return "windows"
	case "darwin", "macos", "mac":
		return "darwin"
	case "linux":
		return "linux"
	default:
		return strings.ToLower(strings.TrimSpace(goos))
	}
}

func normalizeArch(goarch string) string {
	switch strings.ToLower(strings.TrimSpace(goarch)) {
	case "amd64", "x86_64", "x64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	case "386", "i386", "x86":
		return "386"
	default:
		return strings.ToLower(strings.TrimSpace(goarch))
	}
}

// PlatformFor 取出该版本在当前平台上的分发。
//
// 返回 false 表示"这个版本不支持当前平台"——调用方必须把它当成正常情况处理
// （界面上说明"该组件不支持你的系统"），而不是当成错误。
func (v *Version) PlatformFor(goos, goarch string) (Platform, bool) {
	if v == nil || v.Platforms == nil {
		return Platform{}, false
	}
	p, ok := v.Platforms[PlatformKey(goos, goarch)]
	return p, ok
}

// DisplayLabel 返回界面上显示的版本文案。
func (v *Version) DisplayLabel() string {
	if v == nil {
		return ""
	}
	if strings.TrimSpace(v.Label) != "" {
		return v.Label
	}
	return v.Version
}

// Version 按版本标识查找版本。
func (c *Component) Version(version string) (*Version, bool) {
	if c == nil {
		return nil, false
	}
	for i := range c.Versions {
		if c.Versions[i].Version == version {
			return &c.Versions[i], true
		}
	}
	return nil, false
}

// RecommendedVersion 返回推荐版本；清单未指定或找不到时退回第一个版本。
//
// 失败：组件没有任何版本时返回 false。
func (c *Component) RecommendedVersion() (*Version, bool) {
	if c == nil || len(c.Versions) == 0 {
		return nil, false
	}
	if v, ok := c.Version(c.Recommended); ok {
		return v, true
	}
	return &c.Versions[0], true
}
