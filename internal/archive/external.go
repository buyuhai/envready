// 本文件负责：标准库覆盖不到的格式（tar.xz、7z）改用外部工具解压。
// 边界：只做"找一个能用的工具并调用"，不解析压缩包内容。
//
// 三级降级（见设计文档 09）：
//  1. Windows 10+ 自带的 bsdtar（支持 zip / tar.xz / tar.gz）；
//  2. 7-Zip（含 7za / 7zr 精简版）；
//  3. 都没有则明确报错，并告诉用户缺什么，而不是含糊地"解压失败"。
//
// 安全约束：一律用参数数组调用，不拼 shell 字符串——
// 归档路径来自网络，拼字符串就等于把命令注入的口子留给自己。

package archive

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// externalCmd 是一次外部解压尝试。
type externalCmd struct {
	tool string
	args []string
}

// extractExternal 用外部工具解压，按可用性依次尝试。
//
// 失败：没有任何可用工具、所有工具都失败、上下文取消。
// 副作用：调用外部进程；成功后在 dst 下产生文件。
func extractExternal(ctx context.Context, src, dst string, format Format, opts Options) error {
	var attempts []externalCmd
	switch format {
	case FormatTarXz:
		attempts = []externalCmd{
			{"tar", []string{"-xf", src, "-C", dst}},
			{"7z", []string{"x", "-y", "-o" + dst, src}},
			{"7za", []string{"x", "-y", "-o" + dst, src}},
			{"7zr", []string{"x", "-y", "-o" + dst, src}},
		}
	case Format7z:
		attempts = []externalCmd{
			{"7z", []string{"x", "-y", "-o" + dst, src}},
			{"7za", []string{"x", "-y", "-o" + dst, src}},
			{"7zr", []string{"x", "-y", "-o" + dst, src}},
			{"tar", []string{"-xf", src, "-C", dst}},
		}
	default:
		return fmt.Errorf("%w: %s", ErrUnsupported, format)
	}

	var tried []string
	var lastErr error
	for _, a := range attempts {
		bin, err := exec.LookPath(a.tool)
		if err != nil {
			continue
		}
		tried = append(tried, a.tool)

		cmd := exec.CommandContext(ctx, bin, a.args...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			// 外部工具没有统一的"剥掉顶层目录"参数，这里统一在事后处理
			if opts.StripRoot {
				flattenSingleRoot(dst)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = fmt.Errorf("%s 解压失败: %v（输出：%s）", a.tool, err, truncate(strings.TrimSpace(string(out)), 300))
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("%w: 解压 %s 需要 tar 或 7-Zip，当前系统未找到（已尝试：%s）",
		ErrUnsupported, format, strings.Join(tried, "、"))
}

// flattenSingleRoot 在"目标目录下只有一个子目录"时，把该目录的内容上提一层。
//
// 这是外部工具解压时模拟 strip_root 的办法；不满足条件时静默不动——
// 因为此时已无法区分"清单写错了"与"包结构变了"，贸然报错会挡住正常安装。
func flattenSingleRoot(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return
	}
	root := filepath.Join(dir, entries[0].Name())
	children, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, c := range children {
		_ = os.Rename(filepath.Join(root, c.Name()), filepath.Join(dir, c.Name()))
	}
	_ = os.Remove(root)
}

// truncate 截断过长的命令输出，避免把整屏日志塞进错误信息。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
