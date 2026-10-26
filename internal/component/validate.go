// 本文件负责：组件标识（key）的合法性与清单条目的字段自检。
// 边界：只做"数据是否自洽"的判断，不涉及格式版本（那属于 manifest 包）。
//
// 这些规则是清单的"最后一道闸"：上游生成脚本出错、或清单被人工改动，
// 都应该在这里被拦住，而不是等到安装到一半才失败。

package component

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// sha256Pattern 匹配 64 位小写十六进制。
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidKey 校验组件标识能否安全地用作目录名。
//
// 失败：key 为空、为 "." 或 ".."、含路径分隔符或 NUL、或是绝对路径。
// 之所以严格：key 会直接参与 <ENV_ROOT>/<key> 的拼接，
// 而它来自云端清单，属于半可信输入。
func ValidKey(key string) error {
	k := strings.TrimSpace(key)
	switch {
	case k == "":
		return fmt.Errorf("组件 key 不能为空")
	case k == "." || k == "..":
		return fmt.Errorf("组件 key 不能是 %q", k)
	case strings.ContainsAny(k, `/\`):
		return fmt.Errorf("组件 key 不能包含路径分隔符：%q", key)
	case strings.ContainsRune(k, 0):
		return fmt.Errorf("组件 key 不能包含 NUL 字符")
	case filepath.IsAbs(k) || filepath.VolumeName(k) != "":
		return fmt.Errorf("组件 key 不能是绝对路径：%q", key)
	}
	return nil
}

// Validate 检查组件定义是否自洽。
//
// 返回的错误都带上下文（哪个版本、哪个平台），便于定位清单生成脚本的问题。
// 不检查格式版本与"文件是否存在"：前者属于 manifest，后者属于运行时。
func (c *Component) Validate() error {
	if c == nil {
		return fmt.Errorf("组件为 nil")
	}
	if err := ValidKey(c.Key); err != nil {
		return err
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("组件 %s：缺少 name", c.Key)
	}
	if len(c.Versions) == 0 {
		return fmt.Errorf("组件 %s：没有任何版本", c.Key)
	}
	if c.Recommended != "" {
		if _, ok := c.Version(c.Recommended); !ok {
			return fmt.Errorf("组件 %s：recommended=%q 不在版本列表中", c.Key, c.Recommended)
		}
	}

	for i := range c.Versions {
		if err := c.Versions[i].validate(c.Key); err != nil {
			return err
		}
	}
	return nil
}

// validate 检查单个版本。
func (v *Version) validate(componentKey string) error {
	if strings.TrimSpace(v.Version) == "" {
		return fmt.Errorf("组件 %s：存在版本号为空的条目", componentKey)
	}
	where := fmt.Sprintf("组件 %s 版本 %s", componentKey, v.Version)

	if len(v.Platforms) == 0 {
		return fmt.Errorf("%s：没有任何平台分发", where)
	}
	if v.Verify == nil || strings.TrimSpace(v.Verify.Command) == "" {
		return fmt.Errorf("%s：缺少 verify（每个版本都必须能自检）", where)
	}

	for key, p := range v.Platforms {
		if err := p.validate(); err != nil {
			return fmt.Errorf("%s 平台 %s：%w", where, key, err)
		}
	}
	return nil
}

// validate 检查单个平台分发。
func (p Platform) validate() error {
	if !p.InstallKind.Valid() {
		return fmt.Errorf("install_kind=%q 不是已知取值", p.InstallKind)
	}
	if strings.TrimSpace(p.File) == "" {
		return fmt.Errorf("缺少 file")
	}
	if len(p.Mirrors) == 0 {
		return fmt.Errorf("缺少 mirrors（至少要有一个下载前缀）")
	}
	for _, m := range p.Mirrors {
		if !strings.HasPrefix(m, "https://") {
			// 只允许 HTTPS：明文下载会让"完整性校验"变成摆设
			return fmt.Errorf("mirror 必须是 https 开头：%q", m)
		}
	}
	if p.Size < 0 {
		return fmt.Errorf("size 不能为负：%d", p.Size)
	}
	// sha256 允许缺省（上游未提供），但一旦提供就必须是合法摘要
	if p.SHA256 != "" && !sha256Pattern.MatchString(p.SHA256) {
		return fmt.Errorf("sha256 格式非法（应为 64 位小写十六进制）")
	}
	if p.InstallKind == InstallArchive {
		if !p.Archive.Format.Valid() {
			return fmt.Errorf("archive.format=%q 不是已知取值", p.Archive.Format)
		}
	}
	return nil
}
