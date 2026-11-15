// 本文件负责：envvar 包里不依赖平台的部分（路径合并、幂等块）的单元测试。
// 平台相关的注册表测试在 envvar_windows_test.go。

package envvar

import (
	"strings"
	"testing"
)

// TestMergePathPrepend 新增目录默认放在最前面，保证用户拿到刚装的那份。
func TestMergePathPrepend(t *testing.T) {
	got := mergePath(`C:\Windows;C:\Tools`, []string{`D:\envs\mingw\bin`}, Prepend, ";", true)
	want := `D:\envs\mingw\bin;C:\Windows;C:\Tools`
	if got != want {
		t.Errorf("mergePath = %q, 期望 %q", got, want)
	}
}

// TestMergePathAppend 声明 append 时放在最后。
func TestMergePathAppend(t *testing.T) {
	got := mergePath(`C:\Windows;C:\Tools`, []string{`D:\envs\mingw\bin`}, Append, ";", true)
	want := `C:\Windows;C:\Tools;D:\envs\mingw\bin`
	if got != want {
		t.Errorf("mergePath = %q, 期望 %q", got, want)
	}
}

// TestMergePathIdempotent 重复执行不得让 PATH 变长——这是参考实现最典型的烂摊子。
func TestMergePathIdempotent(t *testing.T) {
	once := mergePath(`C:\Windows`, []string{`D:\envs\mingw\bin`}, Prepend, ";", true)
	twice := mergePath(once, []string{`D:\envs\mingw\bin`}, Prepend, ";", true)
	if once != twice {
		t.Errorf("非幂等：第一次 %q，第二次 %q", once, twice)
	}
}

// TestMergePathCaseInsensitive 同一目录换个写法不应重复加入。
func TestMergePathCaseInsensitive(t *testing.T) {
	current := `D:\ENVS\Mingw\Bin;C:\Windows`
	got := mergePath(current, []string{`d:\envs\mingw\bin\`}, Prepend, ";", true)
	if strings.Count(strings.ToLower(got), `d:\envs\mingw\bin`) != 1 {
		t.Errorf("出现重复条目: %q", got)
	}
	if !strings.HasPrefix(got, `d:\envs\mingw\bin\`) {
		t.Errorf("新条目应位于最前: %q", got)
	}
	if strings.Count(got, ";") != strings.Count(current, ";") {
		t.Errorf("条目数不应变化: %q", got)
	}
}

// TestMergePathKeepsVariables PATH 里的 %USERPROFILE% 这类引用必须原样保留。
func TestMergePathKeepsVariables(t *testing.T) {
	got := mergePath(`%USERPROFILE%\bin;C:\Windows`, []string{`D:\envs\x\bin`}, Append, ";", true)
	if !strings.Contains(got, `%USERPROFILE%\bin`) {
		t.Errorf("变量引用被改写: %q", got)
	}
}

// TestRemoveFromPath 卸载时只删自己的目录。
func TestRemoveFromPath(t *testing.T) {
	got := removeFromPath(`D:\envs\a\bin;C:\Windows;d:\ENVS\A\BIN`, []string{`D:\envs\a\bin`}, ";", true)
	if got != `C:\Windows` {
		t.Errorf("removeFromPath = %q", got)
	}
}

// TestInsertAndRemoveBlock 幂等块的写入、替换与移除。
func TestInsertAndRemoveBlock(t *testing.T) {
	original := "# 用户自己的配置\n"
	block1 := buildShellBlock("mingw", "", "", []string{"/opt/mingw/bin"}, Prepend, false)

	content, replaced := insertBlock(original, "mingw", block1)
	if replaced {
		t.Error("首次写入不应标记为替换")
	}
	if !strings.Contains(content, "/opt/mingw/bin") {
		t.Errorf("块内容缺失: %q", content)
	}
	if !strings.Contains(content, "# 用户自己的配置") {
		t.Error("用户原有内容被破坏")
	}

	// 换版本：整块替换而不是追加
	block2 := buildShellBlock("mingw", "", "", []string{"/opt/mingw-2/bin"}, Prepend, false)
	content2, replaced2 := insertBlock(content, "mingw", block2)
	if !replaced2 {
		t.Error("第二次写入应标记为替换")
	}
	if strings.Contains(content2, "/opt/mingw/bin") {
		t.Errorf("旧块未被替换: %q", content2)
	}
	if strings.Count(content2, beginMarker("mingw")) != 1 {
		t.Errorf("出现了多个块: %q", content2)
	}

	// 卸载：移除块后应回到原状
	restored := removeBlock(content2, "mingw")
	if restored != original {
		t.Errorf("移除后内容 = %q, 期望 %q", restored, original)
	}
}

// TestBuildShellBlockPOSIX POSIX shell 用 export。
func TestBuildShellBlockPOSIX(t *testing.T) {
	block := buildShellBlock("go", "GOROOT", "/opt/go/1.27", []string{"/opt/go/1.27/bin"}, Prepend, false)
	for _, want := range []string{
		beginMarker("go"),
		`export PATH="/opt/go/1.27/bin:$PATH"`,
		`export GOROOT="/opt/go/1.27"`,
		endMarker("go"),
	} {
		if !strings.Contains(block, want) {
			t.Errorf("块中缺少 %q：\n%s", want, block)
		}
	}
	if strings.Contains(block, "set -gx") {
		t.Error("POSIX 块里不应出现 fish 语法")
	}
}

// TestBuildShellBlockFish fish 必须用 set -gx，不能出现 export。
func TestBuildShellBlockFish(t *testing.T) {
	block := buildShellBlock("go", "GOROOT", "/opt/go/1.27", []string{"/opt/go/1.27/bin"}, Append, true)
	if strings.Contains(block, "export") {
		t.Errorf("fish 块里出现 POSIX 语法：\n%s", block)
	}
	for _, want := range []string{
		`set -gx PATH $PATH "/opt/go/1.27/bin"`,
		`set -gx GOROOT "/opt/go/1.27"`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("fish 块中缺少 %q：\n%s", want, block)
		}
	}
}

// TestShellQuote 路径里的空格与特殊字符要被安全引用。
func TestShellQuote(t *testing.T) {
	got := shellQuote(`/home/a b/$x`)
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("未加引号: %q", got)
	}
	if strings.Contains(got, `$x`) && !strings.Contains(got, `\$x`) {
		t.Errorf("$ 未被转义: %q", got)
	}
}
