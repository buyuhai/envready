// 本文件负责：安装后的自检——找到命令、真正执行、解析版本号。
// 边界：只负责"跑命令并判断结果"，不负责回滚（由 install.Run 决定）。
//
// 为什么必须真的跑：S1 验收时发现，MinGW 在含空格路径下 gcc --version 完全正常，
// 但链接会失败。可见"能打印版本"只是最低门槛，至少证明二进制能起来。

package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/buyuhai/envready/internal/component"
)

// RunVerify 执行清单里的自检命令。
//
// 返回：命令输出、解析出的版本号（清单未给正则时为空）、错误。
// 失败：找不到命令、退出码与期望不符、输出里没有版本号、上下文取消。
func RunVerify(ctx context.Context, dir string, dirs []string, v *component.Verify, runner Runner) (string, string, error) {
	if v == nil || strings.TrimSpace(v.Command) == "" {
		return "", "", errors.New("清单缺少自检命令（verify.command）")
	}
	bin, err := findCommand(dir, dirs, v.Command)
	if err != nil {
		return "", "", err
	}
	if runner == nil {
		runner = execRunner
	}

	out, err := runner(ctx, bin, v.Args...)
	text := string(out)
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != v.ExpectExit {
			return text, "", fmt.Errorf("自检命令执行失败: %w（输出：%s）", err, truncate(text, 300))
		}
	}

	if v.Parse == "" {
		return text, "", nil
	}
	re, cerr := regexp.Compile(v.Parse)
	if cerr != nil {
		return text, "", fmt.Errorf("清单里的版本正则非法: %w", cerr)
	}
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return text, "", fmt.Errorf("自检输出里没有找到版本号（正则 %q）", v.Parse)
	}
	return text, m[1], nil
}

// findCommand 在安装目录的候选子目录里找可执行文件。
//
// 为什么不用 PATH：安装阶段还没有写环境变量，靠 PATH 找不到；
// 而且按路径直接定位也能保证"跑的就是刚装的那份"，不会误跑到系统里的旧版本。
func findCommand(dir string, dirs []string, command string) (string, error) {
	cands := dirs
	if len(cands) == 0 {
		cands = []string{"."}
	}
	names := executableNames(command)
	for _, d := range cands {
		for _, name := range names {
			p := filepath.Join(dir, d, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("安装目录里找不到自检命令 %q（查找位置：%v）", command, dirs)
}

// executableNames 返回命令在不同平台上的候选文件名。
func executableNames(command string) []string {
	if runtime.GOOS == "windows" {
		return []string{command + ".exe", command + ".cmd", command + ".bat", command}
	}
	return []string{command}
}

// execRunner 是默认的命令执行器：参数数组调用，不经过 shell。
func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// 在命令自身目录下运行，避免依赖调用方的当前目录
	cmd.Dir = filepath.Dir(name)
	return cmd.CombinedOutput()
}

// truncate 截断过长输出，避免把整屏内容塞进错误信息。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
