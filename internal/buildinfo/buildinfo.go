// Package buildinfo 提供构建期注入的版本信息，供 --version 输出与日志使用。
//
// 这三个变量由链接器在构建时通过 -X 覆盖，因此 `go run` 与 `go test` 在未注入时
// 也能正常工作（取默认值 dev / none / unknown）。
package buildinfo

import (
	"fmt"
	"runtime"
)

// Version 是语义化版本号（如 v0.1.0）；未注入时为 dev。
var Version = "dev"

// Commit 是构建所用的提交号；未注入时为 none。
var Commit = "none"

// BuildDate 是构建时间（UTC，RFC3339）；未注入时为 unknown。
var BuildDate = "unknown"

// String 返回一行可直接展示的版本信息，例如：
//
//	EnvReady v0.1.0 (windows/amd64, commit 1a2b3c4, built 2026-10-18T12:00:00Z)
//
// 参数 name 是展示用的应用名（启动器与主程序传入不同的名字以便区分）。
func String(name string) string {
	return fmt.Sprintf("%s %s (%s/%s, commit %s, built %s)",
		name, Version, runtime.GOOS, runtime.GOARCH, Commit, BuildDate)
}
