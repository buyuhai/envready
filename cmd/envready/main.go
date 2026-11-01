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
			"（当前阶段：S1，图形界面在 S3 接入）。")
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
	default:
		fmt.Fprintf(os.Stderr, "未识别的命令：%s\n\n", args[0])
		printUsage(os.Stderr)
		return 2
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
	_, _ = fmt.Fprintf(w, `%s 启动器（当前阶段：S1）

用法：
  envready <命令> [参数]

命令：
  fetch <组件>    下载并校验组件安装包（S1）
  --version       显示版本信息
  --help          显示本说明

fetch 的参数：
  --manifest <路径|https地址>   组件清单位置（必填）
  --version <版本号>            指定版本，缺省用清单推荐版本
  --dest <目录>                 安装包存放目录
  --extract-to <目录>           下载校验通过后解压到该目录
  --quiet                       不显示下载进度

示例：
  envready fetch mingw --manifest ./manifests/mingw.json --extract-to ./envs/mingw

正常使用时不需要直接运行启动器，双击程序图标即可。
`, appName)
}
