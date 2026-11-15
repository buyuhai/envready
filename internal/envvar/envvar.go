// 本文件负责：环境变量变更的跨平台数据结构，以及不依赖系统的纯函数。
// 边界：不直接读写注册表或 shell 配置（分别在 windows.go / unix.go）。
//
// 三条硬规则（设计文档 07）：
//  1. 只写用户级位置（Windows 的 HKCU\Environment、Unix 的用户 rc），永不提权、不写系统目录；
//  2. 写之前先备份"原始未展开值"，回滚时原样写回；
//  3. 写入后必须回读比对，读回来不一致就算失败。

package envvar

import "strings"

// Position 是新增目录放在 PATH 的哪一端。
const (
	// Prepend 放在最前面（默认）：保证用户拿到的是刚装的那份。
	Prepend = "prepend"
	// Append 放在最后面。
	Append = "append"
)

// Plan 描述一次环境变量变更的意图。
type Plan struct {
	// Component 是组件 key，用于备份文件名与 Unix 的标记块。
	Component string
	// HomeVar 是要设置的 home 变量名（如 "GOROOT"）；为空表示不设。
	HomeVar string
	// HomeValue 是 home 变量的值（通常是版本目录的绝对路径）。
	HomeValue string
	// Dirs 是要并入 PATH 的绝对目录列表。
	Dirs []string
	// Position 为 Prepend 或 Append；空值按 Prepend 处理。
	Position string
	// BackupDir 是备份文件目录。
	BackupDir string
}

// Change 是一次变更的凭据：既用于回滚，也用于写日志。
type Change struct {
	// Component 是组件 key。
	Component string
	// BackupFile 是备份文件的绝对路径（为空表示未产生备份）。
	BackupFile string
	// HomeVar 是本次涉及的 home 变量名。
	HomeVar string
	// OldHome 是 home 变量的原始未展开值。
	OldHome string
	// HadHome 表示原来是否已有该变量。
	HadHome bool
	// OldPath 是 PATH 的原始未展开值（可能是 REG_EXPAND_SZ，含 %USERPROFILE% 这类引用）。
	OldPath string
	// HadPath 表示原来是否已有 PATH。
	HadPath bool
	// NewPath 是写入后的 PATH 值，用于回读比对与日志。
	NewPath string
	// RcFile 是 Unix 上被修改的 shell 配置文件（Windows 上为空）。
	RcFile string
}

// normalizePosition 把空值与未知值统一成 Prepend。
func normalizePosition(p string) string {
	if strings.EqualFold(strings.TrimSpace(p), Append) {
		return Append
	}
	return Prepend
}

// splitPath 按分隔符拆 PATH，丢弃空项。
func splitPath(value, sep string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// samePath 比较两个路径条目是否等价。
//
// Windows 下比较忽略大小写与末尾反斜杠——否则同一个目录换个写法就会被重复加入。
func samePath(a, b string, caseInsensitive bool) bool {
	x := strings.TrimRight(strings.TrimSpace(a), `\/`)
	y := strings.TrimRight(strings.TrimSpace(b), `\/`)
	if caseInsensitive {
		return strings.EqualFold(x, y)
	}
	return x == y
}

// mergePath 把 dirs 并入 PATH 值。
//
// 幂等：已存在的条目会被移除后按目标位置重新插入，
// 因此重复安装同一个组件不会让 PATH 越滚越长（参考实现最大的烂摊子之一）。
func mergePath(current string, dirs []string, position, sep string, caseInsensitive bool) string {
	existing := splitPath(current, sep)

	// 先剔除本次要加进去的目录（不论之前在哪个位置）
	var rest []string
	for _, e := range existing {
		dup := false
		for _, d := range dirs {
			if samePath(e, d, caseInsensitive) {
				dup = true
				break
			}
		}
		if !dup {
			rest = append(rest, e)
		}
	}
	// 本次目录内部也去重
	var ours []string
	for _, d := range dirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		dup := false
		for _, o := range ours {
			if samePath(o, d, caseInsensitive) {
				dup = true
				break
			}
		}
		if !dup {
			ours = append(ours, d)
		}
	}

	var merged []string
	if normalizePosition(position) == Append {
		merged = append(merged, rest...)
		merged = append(merged, ours...)
	} else {
		merged = append(merged, ours...)
		merged = append(merged, rest...)
	}
	return strings.Join(merged, sep)
}

// removeFromPath 从 PATH 值里移除指定目录（回滚时用）。
func removeFromPath(current string, dirs []string, sep string, caseInsensitive bool) string {
	existing := splitPath(current, sep)
	kept := make([]string, 0, len(existing))
	for _, e := range existing {
		drop := false
		for _, d := range dirs {
			if samePath(e, d, caseInsensitive) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, e)
		}
	}
	return strings.Join(kept, sep)
}

// beginMarker/endMarker 生成 Unix rc 文件里的幂等块标记。
//
// 为什么用标记块而不是直接追加一行：只有能精确定位"这段是我们写的"，
// 才谈得上"换版本时整块替换"和"卸载时干净移除"。
func beginMarker(component string) string {
	return "# >>> EnvReady:" + component + " >>>"
}

func endMarker(component string) string {
	return "# <<< EnvReady:" + component + " <<<"
}

// insertBlock 把 block 写入 content，若已存在同名块则整块替换。
//
// 返回新的文件内容与是否发生了替换。
func insertBlock(content, component, block string) (string, bool) {
	begin, end := beginMarker(component), endMarker(component)
	start := strings.Index(content, begin)
	if start >= 0 {
		if stop := strings.Index(content[start:], end); stop >= 0 {
			// block 自身以换行结尾，tail 去掉紧随的一个换行，避免出现空行堆积
			tail := strings.TrimPrefix(content[start+stop+len(end):], "\n")
			head := content[:start]
			return head + block + tail, true
		}
		// 只有开始标记没有结束标记：视为损坏，整块追加而不是猜测
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	// block 已自带结尾换行，不再额外添加
	return content + block, false
}

// removeBlock 从 content 里移除指定组件的块。
func removeBlock(content, component string) string {
	begin, end := beginMarker(component), endMarker(component)
	start := strings.Index(content, begin)
	if start < 0 {
		return content
	}
	stopRel := strings.Index(content[start:], end)
	if stopRel < 0 {
		// 标记不完整：只删开始标记那一行，避免把后面的内容一起吞掉
		lineEnd := strings.IndexByte(content[start:], '\n')
		if lineEnd < 0 {
			return content[:start]
		}
		return content[:start] + content[start+lineEnd+1:]
	}
	tail := content[start+stopRel+len(end):]
	tail = strings.TrimPrefix(tail, "\n")
	return content[:start] + tail
}

// escapeShellDouble 转义双引号内部需要保护的字符（不含外层引号）。
func escapeShellDouble(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return r.Replace(s)
}

// shellQuote 给 shell 里的字符串加双引号。
func shellQuote(s string) string {
	return `"` + escapeShellDouble(s) + `"`
}

// buildShellBlock 生成 Unix rc 文件里的幂等块。
//
// fish 与 POSIX shell 的语法不同，必须分开生成——
// 把 bash 的 `export PATH=...` 写进 config.fish 会直接弄坏用户的 fish，
// 这是设计文档 07 里点名要避免的事。
func buildShellBlock(component, homeVar, homeValue string, dirs []string, position string, fish bool) string {
	var b strings.Builder
	b.WriteString(beginMarker(component) + "\n")
	b.WriteString("# 由 EnvReady 生成；升级会整块替换，卸载时请整块删除\n")

	if len(dirs) > 0 {
		if fish {
			b.WriteString("set -gx PATH ")
			if normalizePosition(position) == Append {
				b.WriteString("$PATH")
			}
			for _, d := range dirs {
				b.WriteString(" " + shellQuote(d))
			}
			if normalizePosition(position) != Append {
				b.WriteString(" $PATH")
			}
			b.WriteString("\n")
		} else {
			// POSIX：整条 PATH 用一个双引号包住，条目之间用冒号分隔；
			// 条目内只做转义、不再各自加引号（否则会得到 PATH=""a":$PATH" 这种坏语法）
			joined := strings.Join(escapeAll(dirs), ":")
			if normalizePosition(position) == Append {
				b.WriteString(`export PATH="$PATH:` + joined + "\"\n")
			} else {
				b.WriteString(`export PATH="` + joined + `:$PATH"` + "\n")
			}
		}
	}

	if strings.TrimSpace(homeVar) != "" {
		if fish {
			b.WriteString("set -gx " + homeVar + " " + shellQuote(homeValue) + "\n")
		} else {
			b.WriteString("export " + homeVar + "=" + shellQuote(homeValue) + "\n")
		}
	}

	b.WriteString(endMarker(component) + "\n")
	return b.String()
}

// escapeAll 对每个路径做双引号内转义（不加外层引号）。
func escapeAll(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = escapeShellDouble(s)
	}
	return out
}
