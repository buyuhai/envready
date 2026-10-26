// 本文件负责：manifest 包的单元测试。
// 覆盖：真实夹具解析、格式层校验（schema / 重复 key / 路径穿越）、以及搜索行为。

package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// readFixture 读取 testdata 下的夹具文件。
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取夹具 %s 失败: %v", name, err)
	}
	return data
}

// TestParseIndexFixture 用真实结构的索引夹具验证解析结果。
func TestParseIndexFixture(t *testing.T) {
	idx, err := ParseIndex(readFixture(t, "index.json"))
	if err != nil {
		t.Fatalf("解析索引失败: %v", err)
	}
	if idx.Schema != 1 {
		t.Errorf("Schema = %d", idx.Schema)
	}
	if len(idx.Components) != 2 {
		t.Fatalf("组件数 = %d, 期望 2", len(idx.Components))
	}

	mingw, ok := idx.Find("mingw")
	if !ok {
		t.Fatal("找不到 mingw 条目")
	}
	if !mingw.Supported {
		t.Error("mingw 显式写了 supported:true，应为 true")
	}
	if len(mingw.Aliases) == 0 {
		t.Error("mingw 应有别名")
	}

	// 第二个条目没写 supported，语义必须是"支持"，否则漏字段会让组件从界面消失
	py, ok := idx.Find("python")
	if !ok {
		t.Fatal("找不到 python 条目")
	}
	if !py.Supported {
		t.Error("未写 supported 时应默认为 true")
	}
}

// TestParseIndexRejects 覆盖索引的各种非法输入。
func TestParseIndexRejects(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  error
	}{
		{
			name:  "JSON 非法",
			input: `{`,
			want:  ErrInvalidManifest,
		},
		{
			name:  "schema 过新",
			input: `{"schema":2,"components":[]}`,
			want:  ErrSchemaTooNew,
		},
		{
			name:  "缺少 schema",
			input: `{"components":[]}`,
			want:  ErrInvalidManifest,
		},
		{
			name:  "key 重复",
			input: `{"schema":1,"components":[{"key":"a","manifest":"a.json"},{"key":"a","manifest":"b.json"}]}`,
			want:  ErrInvalidManifest,
		},
		{
			name:  "key 含路径分隔符",
			input: `{"schema":1,"components":[{"key":"a/b","manifest":"a.json"}]}`,
			want:  ErrInvalidManifest,
		},
		{
			name:  "缺少 manifest 路径",
			input: `{"schema":1,"components":[{"key":"a"}]}`,
			want:  ErrInvalidManifest,
		},
		{
			name:  "manifest 路径含 ..",
			input: `{"schema":1,"components":[{"key":"a","manifest":"../evil.json"}]}`,
			want:  ErrInvalidManifest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseIndex([]byte(tc.input))
			if err == nil {
				t.Fatalf("%s：期望报错，实际通过", tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("%s：错误类型不符，得到 %v，期望包含 %v", tc.name, err, tc.want)
			}
		})
	}
}

// TestParseComponentFixture 用真实结构的组件夹具验证解析与平台匹配。
func TestParseComponentFixture(t *testing.T) {
	c, err := ParseComponent(readFixture(t, "mingw.json"))
	if err != nil {
		t.Fatalf("解析组件失败: %v", err)
	}
	if c.Key != "mingw" {
		t.Errorf("Key = %q", c.Key)
	}
	if c.Env.HomeVar != "" {
		t.Error("MinGW 不应设置自造的 home 变量")
	}
	if len(c.Env.Path.Dirs) == 0 || c.Env.Path.Dirs[0] != "bin" {
		t.Errorf("PATH 子目录 = %v, 期望 [bin]", c.Env.Path.Dirs)
	}

	v, ok := c.RecommendedVersion()
	if !ok {
		t.Fatal("应能取到推荐版本")
	}
	if v.Version != "14.2.0" {
		t.Errorf("推荐版本 = %q", v.Version)
	}
	if v.DisplayLabel() != "14.2.0（推荐）" {
		t.Errorf("显示文案 = %q", v.DisplayLabel())
	}

	// 清单里显式给了 windows-arm64，应能命中
	if _, ok := v.PlatformFor("windows", "arm64"); !ok {
		t.Error("windows-arm64 应命中")
	}
	// 没有 darwin，应明确返回 false
	if _, ok := v.PlatformFor("darwin", "arm64"); ok {
		t.Error("darwin-arm64 不应命中")
	}
	if v.Verify == nil || v.Verify.Command != "gcc" {
		t.Errorf("verify 解析异常: %+v", v.Verify)
	}
}

// TestParseComponentRejects 覆盖组件清单的非法输入。
func TestParseComponentRejects(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  error
	}{
		{
			name:  "JSON 非法",
			input: `[`,
			want:  ErrInvalidManifest,
		},
		{
			name:  "schema 过新",
			input: `{"schema":2,"key":"a","name":"A","versions":[]}`,
			want:  ErrSchemaTooNew,
		},
		{
			name:  "没有版本",
			input: `{"schema":1,"key":"a","name":"A","versions":[]}`,
			want:  ErrInvalidManifest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseComponent([]byte(tc.input))
			if err == nil {
				t.Fatalf("%s：期望报错，实际通过", tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("%s：错误类型不符，得到 %v", tc.name, err)
			}
		})
	}
}

// TestSearch 覆盖别名与中文搜索——零基础用户往往不知道正式名称。
func TestSearch(t *testing.T) {
	idx, err := ParseIndex(readFixture(t, "index.json"))
	if err != nil {
		t.Fatalf("解析索引失败: %v", err)
	}

	cases := []struct {
		keyword string
		wantKey string
	}{
		{"gcc", "mingw"},
		{"GCC", "mingw"},
		{"编译器", "mingw"},
		{"C语言", "mingw"},
		{"py", "python"},
		{"mingw", "mingw"},
	}

	for _, tc := range cases {
		got := idx.Search(tc.keyword)
		if len(got) != 1 || got[0].Key != tc.wantKey {
			t.Errorf("Search(%q) = %+v, 期望命中 %s", tc.keyword, got, tc.wantKey)
		}
	}

	if got := idx.Search("不存在的关键词"); len(got) != 0 {
		t.Errorf("无关关键词应无结果，得到 %d 条", len(got))
	}
	if got := idx.Search("   "); len(got) != len(idx.Components) {
		t.Errorf("空关键词应返回全部，得到 %d 条", len(got))
	}
}
