// 本文件负责：把清单 JSON 解析成结构体，并做"格式层"校验。
// 边界：只处理"字节 → 结构 + 校验"，不负责网络拉取、缓存与降级（后续在 fetch/cache 文件里实现）。
//
// 校验分两层，职责不要混：
//   - 本文件：schema 版本、字段是否齐全、key 是否唯一、是否 HTTPS 等"格式正确性"；
//   - component 包：单个条目内部是否自洽（版本、平台、verify 等）。

package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/buyuhai/envready/internal/component"
)

// SchemaVersion 是本程序能理解的最新清单格式版本。
//
// 规则（见设计文档 04 的 4.7）：
//   - 清单 schema 高于此值：拒绝使用并回退到缓存/出厂快照，而不是尝试解析；
//   - 低于此值：按已知字段尽力解析（未知字段忽略）。
const SchemaVersion = 1

var (
	// ErrSchemaTooNew 表示清单由更新版本的程序生成，本程序无法安全理解。
	ErrSchemaTooNew = errors.New("清单格式版本高于本程序支持的版本")
	// ErrInvalidManifest 表示清单内容不合法。
	ErrInvalidManifest = errors.New("清单内容不合法")
)

// Category 是组件分类（界面上的分组）。
type Category struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Order int    `json:"order"`
	Intro string `json:"intro"`
}

// IndexEntry 是索引里的一个组件条目（足够画卡片，但不含版本细节）。
type IndexEntry struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	Aliases    []string `json:"aliases"`
	Summary    string   `json:"summary"`
	Manifest   string   `json:"manifest"`
	ManifestSH string   `json:"manifest_sha256"`
	Supported  bool     `json:"supported"`
}

// Index 是组件索引清单。
type Index struct {
	Schema     int          `json:"schema"`
	UpdatedAt  string       `json:"updated_at"`
	Categories []Category   `json:"categories"`
	Components []IndexEntry `json:"components"`
}

// indexJSON 是解析用的中间结构：supported 用指针区分"未写"与"写了 false"。
// 未写时按"支持"处理——否则清单生成脚本漏一个字段就会让组件从界面上消失。
type indexJSON struct {
	Schema     int              `json:"schema"`
	UpdatedAt  string           `json:"updated_at"`
	Categories []Category       `json:"categories"`
	Components []indexEntryJSON `json:"components"`
}

type indexEntryJSON struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	Aliases    []string `json:"aliases"`
	Summary    string   `json:"summary"`
	Manifest   string   `json:"manifest"`
	ManifestSH string   `json:"manifest_sha256"`
	Supported  *bool    `json:"supported"`
}

// ParseIndex 解析组件索引并校验。
//
// 失败：JSON 非法、schema 过新、组件 key 重复或为空、缺少 manifest 路径。
func ParseIndex(data []byte) (*Index, error) {
	var raw indexJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: 解析 JSON 失败: %v", ErrInvalidManifest, err)
	}
	if err := checkSchema(raw.Schema); err != nil {
		return nil, err
	}

	idx := &Index{
		Schema:     raw.Schema,
		UpdatedAt:  raw.UpdatedAt,
		Categories: raw.Categories,
	}
	seen := make(map[string]struct{}, len(raw.Components))
	for i, e := range raw.Components {
		if err := component.ValidKey(e.Key); err != nil {
			return nil, fmt.Errorf("%w: 第 %d 个组件: %v", ErrInvalidManifest, i+1, err)
		}
		if _, dup := seen[e.Key]; dup {
			return nil, fmt.Errorf("%w: 组件 key 重复: %q", ErrInvalidManifest, e.Key)
		}
		seen[e.Key] = struct{}{}

		if strings.TrimSpace(e.Manifest) == "" {
			return nil, fmt.Errorf("%w: 组件 %s 缺少 manifest 路径", ErrInvalidManifest, e.Key)
		}
		if strings.Contains(e.Manifest, "..") {
			return nil, fmt.Errorf("%w: 组件 %s 的 manifest 路径含 ..：%q", ErrInvalidManifest, e.Key, e.Manifest)
		}

		supported := true
		if e.Supported != nil {
			supported = *e.Supported
		}
		idx.Components = append(idx.Components, IndexEntry{
			Key:        e.Key,
			Name:       e.Name,
			Category:   e.Category,
			Aliases:    e.Aliases,
			Summary:    e.Summary,
			Manifest:   e.Manifest,
			ManifestSH: e.ManifestSH,
			Supported:  supported,
		})
	}
	return idx, nil
}

// ParseComponent 解析单个组件清单并校验。
//
// 失败：JSON 非法、schema 过新、字段不自洽（具体原因见 component.Validate）。
func ParseComponent(data []byte) (*component.Component, error) {
	var c component.Component
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%w: 解析 JSON 失败: %v", ErrInvalidManifest, err)
	}
	if err := checkSchema(c.Schema); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	return &c, nil
}

// checkSchema 校验格式版本。
func checkSchema(schema int) error {
	if schema <= 0 {
		return fmt.Errorf("%w: 缺少 schema 字段", ErrInvalidManifest)
	}
	if schema > SchemaVersion {
		return fmt.Errorf("%w: 清单 schema=%d，本程序最高支持 %d", ErrSchemaTooNew, schema, SchemaVersion)
	}
	return nil
}

// Find 按键查找组件条目。
func (i *Index) Find(key string) (IndexEntry, bool) {
	if i == nil {
		return IndexEntry{}, false
	}
	for _, e := range i.Components {
		if e.Key == key {
			return e, true
		}
	}
	return IndexEntry{}, false
}

// Search 按关键字模糊查找（大小写不敏感，匹配显示名、key 与别名）。
//
// 空关键字返回全部条目——界面首屏就是"不筛选"的状态。
// 之所以覆盖别名：零基础用户往往不知道组件的正式名称，
// 会搜"编译器""C语言"这类词（参考实现的别名表是硬编码的，我们做成数据）。
func (i *Index) Search(keyword string) []IndexEntry {
	if i == nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(keyword))
	if q == "" {
		out := make([]IndexEntry, len(i.Components))
		copy(out, i.Components)
		return out
	}

	var out []IndexEntry
	for _, e := range i.Components {
		if matches(e, q) {
			out = append(out, e)
		}
	}
	return out
}

// matches 判断条目是否命中关键字。
func matches(e IndexEntry, q string) bool {
	if strings.Contains(strings.ToLower(e.Name), q) || strings.Contains(strings.ToLower(e.Key), q) {
		return true
	}
	for _, a := range e.Aliases {
		if strings.Contains(strings.ToLower(a), q) {
			return true
		}
	}
	return false
}
