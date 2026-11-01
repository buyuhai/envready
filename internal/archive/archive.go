// 本文件负责：识别归档格式、分发到具体解压实现、以及"解压到哪里"的安全边界。
// 边界：不负责下载（download 包）、不负责安装后处理（install 包）。
//
// 两条硬规则：
//  1. 格式一律按文件头魔术字节判断，不信扩展名——上游把 .zip 写成 .tar.gz 的情况真实存在；
//  2. 任何会落到目标目录之外的条目一律拒绝（zip slip），而不是"尽力修正"。

package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Format 是由文件头识别出的归档格式。
type Format string

const (
	// FormatZip 是 zip。
	FormatZip Format = "zip"
	// FormatTarGz 是 gzip 压缩的 tar。
	FormatTarGz Format = "tar.gz"
	// FormatTarXz 是 xz 压缩的 tar。
	FormatTarXz Format = "tar.xz"
	// FormatTar 是未压缩的 tar。
	FormatTar Format = "tar"
	// Format7z 是 7z。
	Format7z Format = "7z"
	// FormatUnknown 表示无法识别。
	FormatUnknown Format = "unknown"
)

var (
	// ErrUnsupported 表示当前环境没有可用的解压手段。
	ErrUnsupported = errors.New("不支持或无法识别的归档格式")
	// ErrUnsafeEntry 表示归档中存在会逃出目标目录的条目。
	ErrUnsafeEntry = errors.New("归档中包含越界条目，已拒绝解压")
	// ErrLinkEntry 表示归档中包含符号链接或硬链接，出于安全考虑直接拒绝。
	ErrLinkEntry = errors.New("归档中包含链接条目，出于安全考虑拒绝解压")
)

// Options 控制解压行为。
type Options struct {
	// StripRoot 为 true 时，若所有条目共享同一个顶层目录，则去掉这层目录。
	StripRoot bool
}

// Detect 按文件头识别归档格式。
//
// 失败：文件无法打开或读取。
// 返回 FormatUnknown 且 err 为 nil 表示"读到了，但不是已知格式"——
// 这属于正常情况，调用方应给出"文件可能已损坏或格式不支持"的提示。
func Detect(p string) (Format, error) {
	f, err := os.Open(p)
	if err != nil {
		return FormatUnknown, err
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return FormatUnknown, err
	}
	head = head[:n]
	return detectBytes(head), nil
}

// detectBytes 从文件头判断格式。
func detectBytes(head []byte) Format {
	switch {
	case len(head) >= 4 && bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return FormatZip
	case len(head) >= 4 && bytes.HasPrefix(head, []byte("PK\x05\x06")):
		// 空 zip
		return FormatZip
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return FormatTarGz
	case len(head) >= 6 && bytes.HasPrefix(head, []byte("7z\xbc\xaf\x27\x1c")):
		return Format7z
	case len(head) >= 6 && bytes.HasPrefix(head, []byte("\xfd7zXZ\x00")):
		return FormatTarXz
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return FormatTar
	default:
		return FormatUnknown
	}
}

// Extract 把 src 解压到 dst。
//
// 失败：格式不支持、条目越界、包含链接、磁盘错误、上下文取消。
// 副作用：在 dst 下创建文件与目录；中途失败会留下部分文件（由调用方决定是否清理）。
func Extract(ctx context.Context, src, dst string, opts Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	format, err := Detect(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("创建解压目录失败: %w", err)
	}

	switch format {
	case FormatZip:
		return extractZip(ctx, src, dst, opts)
	case FormatTar:
		return extractTarFile(ctx, src, dst, opts, false)
	case FormatTarGz:
		return extractTarFile(ctx, src, dst, opts, true)
	case FormatTarXz, Format7z:
		// 标准库没有 xz 与 7z；走外部工具（tar / 7z / 7zr）
		return extractExternal(ctx, src, dst, format, opts)
	default:
		return fmt.Errorf("%w: 文件头无法识别", ErrUnsupported)
	}
}

// entryPath 把归档内的条目名换算成"相对目标目录"的路径。
//
// 返回值 skip 表示该条目因 strip_root 被跳过；
// 失败：条目名为空、含 NUL、或是绝对路径/含 ..（这类一律拒绝而不是修正）。
func entryPath(name, stripRoot string) (rel string, skip bool, err error) {
	if strings.ContainsRune(name, 0) {
		return "", false, fmt.Errorf("%w: 条目名含 NUL", ErrUnsafeEntry)
	}
	// 统一用 / 处理，兼容 Windows 打包工具写出的反斜杠
	unified := strings.ReplaceAll(name, `\`, "/")
	unified = strings.TrimSuffix(unified, "/")
	if unified == "" {
		return "", true, nil
	}
	if path.IsAbs(unified) {
		return "", false, fmt.Errorf("%w: 绝对路径 %q", ErrUnsafeEntry, name)
	}
	// 冒号可能是 Windows 盘符（C:/x）或 NTFS 备用数据流（a.txt:evil），一律拒绝
	if strings.ContainsRune(unified, ':') {
		return "", false, fmt.Errorf("%w: 条目名含冒号 %q", ErrUnsafeEntry, name)
	}
	cleaned := path.Clean(unified)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false, fmt.Errorf("%w: 越界路径 %q", ErrUnsafeEntry, name)
	}

	if stripRoot != "" {
		if cleaned == stripRoot {
			return "", true, nil
		}
		prefix := stripRoot + "/"
		if !strings.HasPrefix(cleaned, prefix) {
			// 存在不在该顶层目录下的条目：说明"共享根"判断错误，不剥
			return cleaned, false, nil
		}
		cleaned = strings.TrimPrefix(cleaned, prefix)
		if cleaned == "" {
			return "", true, nil
		}
	}
	return cleaned, false, nil
}

// safeJoin 把相对路径拼到目标目录下，并再次确认结果仍在目录内。
func safeJoin(dst, rel string) (string, error) {
	target := filepath.Join(dst, filepath.FromSlash(rel))
	baseAbs, err := filepath.Abs(dst)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrUnsafeEntry, rel)
	}
	return target, nil
}

// commonRoot 判断所有条目是否共享同一个顶层目录，是则返回该目录名。
//
// 只在 opts.StripRoot 为真时才需要；否则直接返回空串（不剥）。
func commonRoot(names []string, enabled bool) string {
	if !enabled {
		return ""
	}
	root := ""
	for _, name := range names {
		// 目录条目（如 "mingw64/"）与普通文件要区别对待：
		// 真实的 MinGW 包里第一条就是 "mingw64/"，若把它当成"顶层文件"，
		// 就会误判为"没有唯一顶层目录"，从而永远不剥——这个 bug 只有真包才暴露。
		isDirEntry := strings.HasSuffix(name, "/") || strings.HasSuffix(name, `\`)

		unified := strings.TrimSuffix(strings.ReplaceAll(name, `\`, "/"), "/")
		if unified == "" {
			continue
		}
		cleaned := path.Clean(unified)

		var first string
		switch i := strings.IndexByte(cleaned, '/'); {
		case i >= 0:
			first = cleaned[:i]
		case isDirEntry:
			// 顶层目录条目就是候选根本身
			first = cleaned
		default:
			// 顶层就是一个文件：不存在"共享的单一目录"，不能剥
			return ""
		}
		if root == "" {
			root = first
			continue
		}
		if root != first {
			return ""
		}
	}
	return root
}

// listZipNames 返回 zip 内所有条目名（只读目录，不解压）。
func listZipNames(src string) ([]string, error) {
	r, err := zip.OpenReader(src)
	if err != nil {
		return nil, fmt.Errorf("打开 zip 失败: %w", err)
	}
	defer func() { _ = r.Close() }()

	names := make([]string, 0, len(r.File))
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names, nil
}
