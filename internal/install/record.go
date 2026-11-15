// 本文件负责：安装记录的读写（记录"这个目录是谁、什么时候、从哪、校验过没有"）。
// 边界：只读写单个目录下的一个 JSON 文件，不维护全局索引。
//
// 为什么要记录：以后要回答"这个组件我装过没有、装的是哪个包、校验通过没"，
// 只能靠这份随目录走的记录；单靠目录名无法得知来源与校验状态。

package install

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// recordName 是安装记录的文件名（以点开头，避免和安装内容混淆）。
const recordName = ".envready-install.json"

// Record 是一次安装的凭据。
type Record struct {
	Key         string    `json:"key"`
	Version     string    `json:"version"`
	File        string    `json:"file"`
	SHA256      string    `json:"sha256"`
	Verified    bool      `json:"verified"`
	InstalledAt time.Time `json:"installed_at"`
	Mirrors     []string  `json:"mirrors"`
}

// WriteRecord 把安装记录写入目录。
//
// 失败：序列化或写盘失败（此时目录已就位，调用方应视为安装失败并回滚）。
func WriteRecord(dir string, r Record) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化安装记录失败: %w", err)
	}
	p := filepath.Join(dir, recordName)
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return fmt.Errorf("写入安装记录失败: %w", err)
	}
	return nil
}

// ReadRecord 读取目录下的安装记录。
//
// 失败：文件不存在或内容损坏。
func ReadRecord(dir string) (Record, error) {
	var r Record
	raw, err := os.ReadFile(filepath.Join(dir, recordName))
	if err != nil {
		return Record{}, fmt.Errorf("读取安装记录失败: %w", err)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("解析安装记录失败: %w", err)
	}
	return r, nil
}
