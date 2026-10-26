// 本文件负责：component 包的单元测试。
// 重点覆盖三件事：平台键归一化、推荐版本的兜底逻辑、以及清单数据的自洽校验。

package component

import (
	"strings"
	"testing"
)

// TestPlatformKey 覆盖平台键的归一化，避免"明明有包却匹配不到"。
func TestPlatformKey(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"windows", "amd64", "windows-amd64"},
		{"windows", "x86_64", "windows-amd64"},
		{"windows", "arm64", "windows-arm64"},
		{"darwin", "arm64", "darwin-arm64"},
		{"darwin", "aarch64", "darwin-arm64"},
		{"darwin", "x86_64", "darwin-amd64"},
		{"linux", "amd64", "linux-amd64"},
		{"Linux", "AMD64", "linux-amd64"},
	}
	for _, tc := range cases {
		if got := PlatformKey(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("PlatformKey(%q,%q) = %q, 期望 %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// TestPlatformFor 缺少当前平台时应返回 false，而不是一个空结构被人误用。
func TestPlatformFor(t *testing.T) {
	v := &Version{
		Version: "1.0.0",
		Platforms: map[string]Platform{
			"windows-amd64": {File: "a.zip"},
		},
	}

	if p, ok := v.PlatformFor("windows", "amd64"); !ok || p.File != "a.zip" {
		t.Fatalf("windows/amd64 应命中，得到 %+v ok=%v", p, ok)
	}
	if _, ok := v.PlatformFor("darwin", "arm64"); ok {
		t.Error("darwin/arm64 不应命中")
	}
	if _, ok := (*Version)(nil).PlatformFor("windows", "amd64"); ok {
		t.Error("nil 版本不应命中")
	}
}

// TestDisplayLabel 没有 label 时退回 version，避免界面出现空白。
func TestDisplayLabel(t *testing.T) {
	if got := (&Version{Version: "1.2.3"}).DisplayLabel(); got != "1.2.3" {
		t.Errorf("DisplayLabel = %q", got)
	}
	if got := (&Version{Version: "1.2.3", Label: "1.2.3（推荐）"}).DisplayLabel(); got != "1.2.3（推荐）" {
		t.Errorf("DisplayLabel = %q", got)
	}
	if got := (*Version)(nil).DisplayLabel(); got != "" {
		t.Errorf("nil DisplayLabel = %q", got)
	}
}

// TestRecommendedVersionFallback 未指定 recommended 时退回第一个版本。
func TestRecommendedVersionFallback(t *testing.T) {
	c := &Component{
		Key:      "demo",
		Name:     "Demo",
		Versions: []Version{{Version: "2.0.0"}, {Version: "1.0.0"}},
	}
	v, ok := c.RecommendedVersion()
	if !ok || v.Version != "2.0.0" {
		t.Fatalf("应退回第一个版本，得到 %+v ok=%v", v, ok)
	}

	c.Recommended = "1.0.0"
	v, ok = c.RecommendedVersion()
	if !ok || v.Version != "1.0.0" {
		t.Fatalf("应使用 recommended，得到 %+v ok=%v", v, ok)
	}

	empty := &Component{Key: "x"}
	if _, ok := empty.RecommendedVersion(); ok {
		t.Error("没有版本时应返回 false")
	}
}

// validComponent 返回一个通过校验的组件，供各用例做"改一处就失败"的基线。
func validComponent() *Component {
	return &Component{
		Schema: 1,
		Key:    "mingw",
		Name:   "MinGW",
		Env: EnvSpec{
			Path: PathSpec{Dirs: []string{"bin"}, Position: "prepend"},
		},
		Recommended: "14.2.0",
		Versions: []Version{
			{
				Version: "14.2.0",
				Platforms: map[string]Platform{
					"windows-amd64": {
						InstallKind: InstallArchive,
						File:        "a.zip",
						Size:        10,
						SHA256:      strings.Repeat("a", 64),
						Mirrors:     []string{"https://example.com/dl/"},
						Archive:     ArchiveSpec{Format: FormatZip},
					},
				},
				Verify: &Verify{Command: "gcc", Args: []string{"--version"}},
			},
		},
	}
}

// TestValidateOK 基线应通过。
func TestValidateOK(t *testing.T) {
	if err := validComponent().Validate(); err != nil {
		t.Fatalf("基线组件应通过校验，得到: %v", err)
	}
}

// TestValidateFailures 逐项破坏字段，确认都能被拦住。
func TestValidateFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Component)
	}{
		{"key 为空", func(c *Component) { c.Key = "" }},
		{"key 含分隔符", func(c *Component) { c.Key = "a/b" }},
		{"key 为 ..", func(c *Component) { c.Key = ".." }},
		{"缺少 name", func(c *Component) { c.Name = "" }},
		{"没有版本", func(c *Component) { c.Versions = nil }},
		{"recommended 不在列表", func(c *Component) { c.Recommended = "9.9.9" }},
		{"版本号为空", func(c *Component) { c.Versions[0].Version = "" }},
		{"没有平台", func(c *Component) { c.Versions[0].Platforms = nil }},
		{"缺少 verify", func(c *Component) { c.Versions[0].Verify = nil }},
		{"verify 无 command", func(c *Component) { c.Versions[0].Verify = &Verify{} }},
		{"install_kind 非法", func(c *Component) {
			p := c.Versions[0].Platforms["windows-amd64"]
			p.InstallKind = "magic"
			c.Versions[0].Platforms["windows-amd64"] = p
		}},
		{"archive 格式非法", func(c *Component) {
			p := c.Versions[0].Platforms["windows-amd64"]
			p.Archive.Format = "rar"
			c.Versions[0].Platforms["windows-amd64"] = p
		}},
		{"缺少 file", func(c *Component) {
			p := c.Versions[0].Platforms["windows-amd64"]
			p.File = ""
			c.Versions[0].Platforms["windows-amd64"] = p
		}},
		{"没有 mirrors", func(c *Component) {
			p := c.Versions[0].Platforms["windows-amd64"]
			p.Mirrors = nil
			c.Versions[0].Platforms["windows-amd64"] = p
		}},
		{"mirror 非 https", func(c *Component) {
			p := c.Versions[0].Platforms["windows-amd64"]
			p.Mirrors = []string{"http://example.com/dl/"}
			c.Versions[0].Platforms["windows-amd64"] = p
		}},
		{"sha256 格式错误", func(c *Component) {
			p := c.Versions[0].Platforms["windows-amd64"]
			p.SHA256 = "not-a-hash"
			c.Versions[0].Platforms["windows-amd64"] = p
		}},
		{"size 为负", func(c *Component) {
			p := c.Versions[0].Platforms["windows-amd64"]
			p.Size = -1
			c.Versions[0].Platforms["windows-amd64"] = p
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validComponent()
			tc.mutate(c)
			if err := c.Validate(); err == nil {
				t.Errorf("%s：期望校验失败，实际通过", tc.name)
			}
		})
	}
}

// TestValidateAllowsEmptySHA256 上游未提供摘要时应允许（由界面显示"未校验"）。
func TestValidateAllowsEmptySHA256(t *testing.T) {
	c := validComponent()
	p := c.Versions[0].Platforms["windows-amd64"]
	p.SHA256 = ""
	c.Versions[0].Platforms["windows-amd64"] = p

	if err := c.Validate(); err != nil {
		t.Fatalf("缺少 sha256 应被允许，得到: %v", err)
	}
}
