// 本文件负责：启动器（launcher）入口。
//
// 启动器只做三件事：
//  1. 读取指针文件，决定该启动哪个版本的主程序；
//  2. 拉起主程序，并在"刚更新过"时驻留观察启动是否健康；
//  3. 找不到可用版本时进入修复模式。
//
// 边界：本文件只做参数解析与装配，不含业务逻辑；
// 指针读取、健康检查、回退与修复模式的实现都在 internal/update。
//
// 为什么启动器要独立存在：正在运行的程序无法原子替换自身，
// 所以"替换"必须交给一个不在被替换范围内的角色（详见设计文档 03）。
//
// 当前阶段：S0 工程骨架，只实现 --version。
package main

import (
	"fmt"
	"os"

	"github.com/buyuhai/envready/internal/buildinfo"
)

// appName 是展示给用户的应用名。可执行文件名统一小写（envready），
// 但界面上显示的名字保持这个写法。
const appName = "EnvReady"

func main() {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--version", "-v":
			fmt.Println(buildinfo.String(appName + " 启动器"))
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

	// S4 之前启动器还没有"读指针、拉起主程序"的能力。
	// 这里明确告知当前阶段，而不是静默退出——对应"没有静默失败"原则。
	fmt.Fprintln(os.Stderr, "EnvReady 启动器尚未实现（当前阶段：S0 工程骨架）。")
	os.Exit(1)
}

// printUsage 输出启动器的用法说明。
func printUsage() {
	fmt.Printf(`%s 启动器

用法：
  envready --version    显示版本信息
  envready --help       显示本说明

正常使用时不需要直接运行启动器，双击程序图标即可。
`, appName)
}
