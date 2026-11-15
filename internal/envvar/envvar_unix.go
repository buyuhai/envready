//go:build !windows

// 本文件负责：Unix 上的环境变量写入——在用户的 shell 配置里维护一段幂等块。
// 边界：只改用户自己的 rc 文件；不碰 /etc、不 sudo。
//
// 为什么用"标记块"而不是追加一行 export：
//   - 换版本时要整块替换，追加会越堆越多；
//   - 卸载时要能精确移除，否则用户的 rc 文件里永远留着死路径。
//
// fish 必须单独处理：它的语法是 `set -gx`，写 bash 语法进去会直接弄坏用户的 shell。

package envvar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// rcTarget 决定改哪个 shell 配置文件，以及是否是 fish。
//
// macOS 上不能只信 $SHELL（从 Finder 启动时可能为空），因此没有可用信息时退到 ~/.profile。
func rcTarget() (path string, fish bool, err error) {
	home, herr := os.UserHomeDir()
	if herr != nil {
		return "", false, fmt.Errorf("无法确定用户主目录: %w", herr)
	}
	shell := strings.ToLower(filepath.Base(strings.TrimSpace(os.Getenv("SHELL"))))
	switch {
	case strings.Contains(shell, "fish"):
		return filepath.Join(home, ".config", "fish", "config.fish"), true, nil
	case strings.Contains(shell, "zsh"):
		return filepath.Join(home, ".zshrc"), false, nil
	case strings.Contains(shell, "bash"):
		return filepath.Join(home, ".bashrc"), false, nil
	default:
		return filepath.Join(home, ".profile"), false, nil
	}
}

// unixBackup 是 Unix 侧的备份内容：整份旧文件。
type unixBackup struct {
	Component  string    `json:"component"`
	RcFile     string    `json:"rc_file"`
	OldContent string    `json:"old_content"`
	CreatedAt  time.Time `json:"created_at"`
}

// Apply 在 shell 配置里写入/更新组件的幂等块。
//
// 失败：主目录不可用、写备份失败、写配置失败、回读不一致。
// 副作用：修改用户 rc 文件；在 BackupDir 写一份原文备份。
func Apply(p Plan) (Change, error) {
	if strings.TrimSpace(p.Component) == "" {
		return Change{}, fmt.Errorf("环境变量变更缺少组件标识")
	}
	rc, fish, err := rcTarget()
	if err != nil {
		return Change{}, err
	}

	old, readErr := os.ReadFile(rc)
	if readErr != nil && !os.IsNotExist(readErr) {
		return Change{}, fmt.Errorf("读取 shell 配置失败: %w", readErr)
	}
	oldContent := string(old)

	change := Change{Component: p.Component, HomeVar: p.HomeVar, OldPath: oldContent, RcFile: rc}

	if err := os.MkdirAll(p.BackupDir, 0o755); err != nil {
		return Change{}, fmt.Errorf("创建备份目录失败: %w", err)
	}
	backupPath := filepath.Join(p.BackupDir,
		fmt.Sprintf("env-%s-%s.json", p.Component, time.Now().UTC().Format("20060102-150405")))
	raw, merr := json.MarshalIndent(unixBackup{
		Component:  p.Component,
		RcFile:     rc,
		OldContent: oldContent,
		CreatedAt:  time.Now().UTC(),
	}, "", "  ")
	if merr != nil {
		return Change{}, fmt.Errorf("序列化备份失败: %w", merr)
	}
	if werr := os.WriteFile(backupPath, raw, 0o644); werr != nil {
		return Change{}, fmt.Errorf("写备份失败（未做任何修改）: %w", werr)
	}
	change.BackupFile = backupPath

	block := buildShellBlock(p.Component, p.HomeVar, p.HomeValue, p.Dirs, p.Position, fish)
	newContent, replaced := insertBlock(oldContent, p.Component, block)
	change.HadHome = replaced
	change.NewPath = block

	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
		return change, fmt.Errorf("创建配置目录失败: %w", err)
	}
	if err := os.WriteFile(rc, []byte(newContent), 0o644); err != nil {
		return change, fmt.Errorf("写入 shell 配置失败: %w", err)
	}

	// 回读比对
	got, gerr := os.ReadFile(rc)
	if gerr != nil {
		return change, fmt.Errorf("回读 shell 配置失败: %w", gerr)
	}
	if string(got) != newContent {
		return change, fmt.Errorf("shell 配置回读不一致（%s）", rc)
	}
	return change, nil
}

// Revert 移除组件的块，恢复配置原状。
//
// 失败：读取或写回配置失败。
// 副作用：修改用户 rc 文件。
func Revert(c Change) error {
	rc := c.RcFile
	if rc == "" {
		var err error
		rc, _, err = rcTarget()
		if err != nil {
			return err
		}
	}
	cur, err := os.ReadFile(rc)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 shell 配置失败: %w", err)
	}
	newContent := removeBlock(string(cur), c.Component)
	if newContent == string(cur) {
		return nil
	}
	if err := os.WriteFile(rc, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("写回 shell 配置失败: %w", err)
	}
	return nil
}

// Remove 从配置里移除组件块（卸载用）。
func Remove(component string, _ []string) error {
	return Revert(Change{Component: component})
}

// ReadPath 返回当前进程看到的 PATH（诊断用）。
func ReadPath() (string, error) {
	return os.Getenv("PATH"), nil
}
