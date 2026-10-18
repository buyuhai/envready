// 本文件负责：主程序（GUI 应用）入口。
//
// 边界：这里只做进程级装配——解析参数、打印版本、把控制权交给 internal/app。
// 不写任何业务逻辑，也不直接操作界面控件。
//
// 为什么当前阶段不 import Wails：Wails 会引入 cgo 与平台 GUI 依赖
// （Linux 需要 gtk/webkit 系统库），如果在 S0 就接进来，CI 第一天就得安装一堆系统依赖，
// 而 S1/S2 要验证的内核（下载、解压、环境变量）根本用不到界面。
// 界面在 S3 接入，届时内核已经可以用命令行独立验证。
//
// 当前阶段：S0 工程骨架，只实现 --version。
package main

import (
	"fmt"
	"os"

	"github.com/buyuhai/envready/internal/buildinfo"
)

// appName 是展示给用户的应用名。
const appName = "EnvReady"

func main() {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--version", "-v":
			fmt.Println(buildinfo.String(appName))
			return
		case "--help", "-h":
			printUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "未识别的参数：%s\n\n", arg)
			printUsage()
			os.Exit(2)
		}
	}

	// S3 之前还没有界面。
	fmt.Fprintln(os.Stderr, "EnvReady 主程序尚未实现（当前阶段：S0 工程骨架）。")
	os.Exit(1)
}

// printUsage 输出主程序的用法说明。
func printUsage() {
	fmt.Printf(`%s

用法：
  envready-gui --version    显示版本信息
  envready-gui --help       显示本说明

正常使用时不需要命令行参数，双击程序图标即可。
`, appName)
}
