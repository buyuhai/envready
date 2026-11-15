// 本文件负责：命令行入口。既包含启动器（launcher）的参数解析，也包含 S1 期间的取包命令。
//
// 启动器最终只做三件事：
//  1. 读取指针文件，决定该启动哪个版本的主程序；
//  2. 拉起主程序，并在"刚更新过"时驻留观察启动是否健康；
//  3. 找不到可用版本时进入修复模式。
//
// 边界：本文件只做参数解析、进度显示与结果汇报，业务逻辑都在 internal/app。
//
// 为什么启动器要独立存在：正在运行的程序无法原子替换自身，
// 所以"替换"必须交给一个不在被替换范围内的角色（详见设计文档 03）。
//
// 当前阶段：S1。`fetch` 已可用，`--version/--help` 保留；读指针与拉起主程序在 S4 实现。

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/buyuhai/envready/internal/app"
	"github.com/buyuhai/envready/internal/buildinfo"
	"github.com/buyuhai/envready/internal/download"
	"github.com/buyuhai/envready/internal/install"
	"github.com/buyuhai/envready/internal/paths"
)

// appName 是展示给用户的应用名。可执行文件名统一小写（envready），
// 但界面上显示的名字保持这个写法。
const appName = "EnvReady"

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 分发命令并返回进程退出码。
//
// 退出码约定：0 成功；1 运行期错误；2 用法错误。
// 区分 1 与 2 是为了让脚本能判断"是我用错了"还是"程序失败了"。
func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "EnvReady 启动器尚未实现"+
			"（当前阶段：S3，图形界面尚未接入）。")
		fmt.Fprintln(os.Stderr, "")
		printUsage(os.Stderr)
		return 1
	}

	switch args[0] {
	case "--version", "-v", "version":
		fmt.Println(buildinfo.String(appName + " 启动器"))
		return 0
	case "--help", "-h", "help":
		printUsage(os.Stdout)
		return 0
	case "fetch":
		return runFetch(args[1:])
	case "install":
		return runInstall(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未识别的命令：%s\n\n", args[0])
		printUsage(os.Stderr)
		return 2
	}
}

// runInstall 执行 `envready install <组件> [参数]`。
func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	manifestSrc := fs.String("manifest", "", "组件清单：本地路径或 https 地址（必填）")
	version := fs.String("version", "", "指定版本号；缺省用清单中的推荐版本")
	envRoot := fs.String("env-root", "", "环境根目录；缺省用程序数据目录下的 envs")
	destDir := fs.String("dest", "", "安装包存放目录；缺省用程序数据目录下的 downloads")
	backupDir := fs.String("backup", "", "环境变量备份目录；缺省用程序数据目录下的 env-backup")
	noEnv := fs.Bool("no-env", false, "只安装，不写环境变量")
	force := fs.Bool("force", false, "同版本已存在时覆盖重装")
	quiet := fs.Bool("quiet", false, "不显示下载进度")

	if err := fs.Parse(reorderArgs(args)); err != nil {
		// -h / --help 是正常请求，不是用法错误：应该退出 0 而不是 2
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "缺少组件名，例如：envready install mingw --manifest ./mingw.json")
		return 2
	}
	if *manifestSrc == "" {
		fmt.Fprintln(os.Stderr, "缺少 --manifest（组件清单的路径或 https 地址）")
		return 2
	}
	key := fs.Arg(0)

	layout, err := paths.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法确定数据目录："+err.Error())
		return 1
	}
	root := *envRoot
	if root == "" {
		root = layout.EnvRoot
	}
	dl := *destDir
	if dl == "" {
		dl = layout.DownloadsDir()
	}
	bk := *backupDir
	if bk == "" {
		bk = layout.BackupDir()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	printer := newProgressPrinter(!*quiet)
	res, err := app.Install(ctx, app.InstallRequest{
		Key:            key,
		Version:        *version,
		ManifestSource: *manifestSrc,
		EnvRoot:        root,
		Downloads:      dl,
		BackupDir:      bk,
		SkipEnv:        *noEnv,
		Force:          *force,
		Progress:       printer,
	})
	if printer != nil {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if err != nil {
		// 空格路径：不只说"不行"，直接给出可执行的下一步
		var spe *install.SpacePathError
		if errors.As(err, &spe) {
			fmt.Fprintln(os.Stderr, "这个组件不能在含空格的路径下工作（它的链接器不会给路径加引号）。")
			fmt.Fprintf(os.Stderr, "  当前路径：%s\n", spe.Path)
			fmt.Fprintf(os.Stderr, "  建议改用：%s\n", spe.Suggested)
			fmt.Fprintf(os.Stderr, "  重试：envready install %s --manifest %q --env-root %q\n", key, *manifestSrc, spe.Suggested)
			return 1
		}
		// 安装成功但环境变量没写成时，res.Dir 有值，要如实告诉用户"已经装好了"
		if res.Dir != "" {
			fmt.Printf("程序已安装到：%s\n", res.Dir)
			fmt.Fprintln(os.Stderr, "但环境变量没有写成功："+err.Error())
			return 1
		}
		fmt.Fprintln(os.Stderr, "安装失败："+err.Error())
		return 1
	}

	printInstallResult(res)
	return 0
}

// printInstallResult 用白话汇报安装结果与下一步。
func printInstallResult(res app.InstallResult) {
	fmt.Printf("已安装：%s %s\n", res.Component, res.Version)
	fmt.Printf("位置：%s\n", res.Dir)
	if res.Verified {
		if res.VerifyVersion != "" {
			fmt.Printf("自检：通过（版本 %s）\n", res.VerifyVersion)
		} else {
			fmt.Println("自检：通过")
		}
	} else {
		fmt.Println("自检：上游未提供校验和，安装包未经完整性校验")
	}
	if res.EnvChanged {
		fmt.Printf("环境变量：已更新（备份 %s）\n", res.BackupFile)
	} else {
		fmt.Println("环境变量：未修改")
	}
	if len(res.PostNotes) > 0 {
		fmt.Println("接下来：")
		for _, n := range res.PostNotes {
			fmt.Println("  - " + n)
		}
	}
}

// runFetch 执行 `envready fetch <组件> [参数]`。
func runFetch(args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	manifestSrc := fs.String("manifest", "", "组件清单：本地路径或 https 地址（必填）")
	version := fs.String("version", "", "指定版本号；缺省用清单中的推荐版本")
	destDir := fs.String("dest", "", "安装包存放目录；缺省用程序数据目录下的 downloads")
	extractTo := fs.String("extract-to", "", "下载并校验通过后解压到该目录")
	quiet := fs.Bool("quiet", false, "不显示下载进度")

	if err := fs.Parse(reorderArgs(args)); err != nil {
		// -h / --help 是正常请求，不是用法错误：应该退出 0 而不是 2
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "缺少组件名，例如：envready fetch mingw --manifest ./mingw.json")
		return 2
	}
	if *manifestSrc == "" {
		fmt.Fprintln(os.Stderr, "缺少 --manifest（组件清单的路径或 https 地址）")
		return 2
	}
	key := fs.Arg(0)

	dir := *destDir
	if dir == "" {
		layout, err := paths.New()
		if err != nil {
			fmt.Fprintln(os.Stderr, "无法确定数据目录："+err.Error())
			return 1
		}
		dir = layout.DownloadsDir()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	printer := newProgressPrinter(!*quiet)
	res, err := app.Fetch(ctx, app.FetchRequest{
		Key:            key,
		Version:        *version,
		ManifestSource: *manifestSrc,
		DestDir:        dir,
		ExtractTo:      *extractTo,
		Progress:       printer,
	})
	if printer != nil {
		fmt.Fprint(os.Stderr, "\r\033[K") // 清掉进度行
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "取包失败："+err.Error())
		return 1
	}

	printFetchResult(res)
	return 0
}

// reorderArgs 把"位置参数"挪到 flag 之后，使 `fetch python --manifest x`
// 与 `fetch --manifest x python` 两种写法都能用。
//
// 为什么需要：Go 的 flag 包在遇到第一个非 flag 参数时停止解析，
// 而用户（包括我）习惯把组件名写在前面。这里做一个无损重排，
// 避免把"解析规则"当成"用户必须记住的规矩"。
func reorderArgs(args []string) []string {
	// 需要消费下一个参数的 flag（布尔型不在此列）
	valueFlags := map[string]bool{
		"manifest":   true,
		"version":    true,
		"dest":       true,
		"extract-to": true,
		"env-root":   true,
		"backup":     true,
	}

	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue // --key=value 形式自带值
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// printFetchResult 用普通人能看懂的话汇报结果。
//
// 这一层的文案归属设计文档 08：不堆术语，但必须说清"装的是什么、放在哪、校验了没有"。
func printFetchResult(res app.FetchResult) {
	fmt.Printf("组件：%s\n", res.Component)
	fmt.Printf("版本：%s\n", res.Version)
	fmt.Printf("文件：%s（%s）\n", res.File, humanBytes(res.Size))
	if res.Verified {
		fmt.Printf("校验：已通过（SHA-256 %s…）\n", shortHash(res.SHA256))
	} else {
		fmt.Println("校验：上游未提供校验和，本次未做完整性校验")
	}
	if res.Extracted != "" {
		fmt.Printf("解压：%s\n", res.Extracted)
	}
}

// newProgressPrinter 返回一个限流的进度打印函数；enabled 为 false 时返回 nil。
func newProgressPrinter(enabled bool) func(download.Progress) {
	if !enabled {
		return nil
	}
	var last time.Time
	return func(p download.Progress) {
		if time.Since(last) < 200*time.Millisecond {
			return
		}
		last = time.Now()
		fmt.Fprintf(os.Stderr, "\r下载中：%s", humanProgress(p))
	}
}

// humanProgress 把进度格式化成一行。
func humanProgress(p download.Progress) string {
	if p.UnknownTotal || p.Total <= 0 {
		return humanBytes(p.Downloaded) + "（服务端未提供总大小）"
	}
	pct := float64(p.Downloaded) / float64(p.Total) * 100
	return fmt.Sprintf("%.1f%%（%s / %s）", pct, humanBytes(p.Downloaded), humanBytes(p.Total))
}

// humanBytes 把字节数变成人话单位。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// shortHash 截取校验和前 12 位用于展示。
func shortHash(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}

// printUsage 输出用法说明。
func printUsage(w *os.File) {
	// 写 usage 失败无处可报（这里已经在错误处理路径上），显式忽略
	_, _ = fmt.Fprintf(w, `%s 命令行（S2 已完成；S3 图形界面开发中）

用法：
  envready <命令> [参数]

命令：
  fetch <组件>    只下载并校验安装包（S1）
  install <组件>  下载 → 校验 → 解压 → 自检 → 写环境变量（S2）
  --version       显示版本信息
  --help          显示本说明

公共参数：
  --manifest <路径|https地址>   组件清单位置（必填）
  --version <版本号>            指定版本，缺省用清单推荐版本
  --dest <目录>                 安装包存放目录
  --quiet                       不显示下载进度

fetch 专有：
  --extract-to <目录>           下载校验通过后解压到该目录

install 专有：
  --env-root <目录>             环境根目录（默认在程序数据目录下）
  --backup <目录>               环境变量备份目录
  --no-env                      只安装，不写环境变量
  --force                       同版本已存在时覆盖重装

示例：
  envready fetch mingw --manifest ./manifests/mingw.json --extract-to ./envs/mingw
  envready install mingw --manifest ./manifests/mingw.json

图形界面尚未接入（S3），当前请通过命令行使用。
`, appName)
}
