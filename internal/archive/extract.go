// 本文件负责：zip 与 tar（含 gzip）的实际解压。
// 边界：不做格式识别（调用方已识别），不做外部工具降级（见 external.go）。

package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// extractZip 解压 zip 到 dst。
//
// 失败：条目越界、含符号链接、写盘失败、上下文取消。
func extractZip(ctx context.Context, src, dst string, opts Options) error {
	names, err := listZipNames(src)
	if err != nil {
		return err
	}
	strip := commonRoot(names, opts.StripRoot)

	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("打开 zip 失败: %w", err)
	}
	defer func() { _ = r.Close() }()

	for _, f := range r.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, skip, err := entryPath(f.Name, strip)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		target, err := safeJoin(dst, rel)
		if err != nil {
			return err
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("创建目录 %s 失败: %w", rel, err)
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q", ErrLinkEntry, f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("创建父目录失败: %w", err)
		}
		if err := writeZipEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

// writeZipEntry 把一个 zip 条目写到磁盘。
func writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("读取条目 %s 失败: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	perm := f.Mode().Perm()
	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("创建文件 %s 失败: %w", target, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, rc); err != nil {
		return fmt.Errorf("写入文件 %s 失败: %w", target, err)
	}
	return nil
}

// extractTarFile 解压 tar（可选 gzip）到 dst。
//
// 先扫一遍条目名以判断是否需要剥掉顶层目录，再真正解压——
// tar 是流式的，不预扫就无法在"遇到第一个条目时"决定是否剥。
func extractTarFile(ctx context.Context, src, dst string, opts Options, gzipped bool) error {
	names, err := scanTarNames(src, gzipped)
	if err != nil {
		return err
	}
	strip := commonRoot(names, opts.StripRoot)

	closer, tr, err := openTar(src, gzipped)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取 tar 失败: %w", err)
		}

		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("%w: %q", ErrLinkEntry, hdr.Name)
		}

		rel, skip, err := entryPath(hdr.Name, strip)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		target, err := safeJoin(dst, rel)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("创建目录 %s 失败: %w", rel, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("创建父目录失败: %w", err)
			}
			if err := writeFileFrom(target, tr, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		default:
			// 设备文件、FIFO 等一律不处理：正常安装包里不该有
			return fmt.Errorf("%w: 非常规条目 %q (type %d)", ErrUnsupported, hdr.Name, hdr.Typeflag)
		}
	}
}

// writeFileFrom 把当前 tar 条目的内容写到磁盘。
func writeFileFrom(target string, r io.Reader, perm os.FileMode) error {
	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("创建文件 %s 失败: %w", target, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, r); err != nil {
		return fmt.Errorf("写入文件 %s 失败: %w", target, err)
	}
	return nil
}

// scanTarNames 预扫 tar 里的条目名。
func scanTarNames(src string, gzipped bool) ([]string, error) {
	closer, tr, err := openTar(src, gzipped)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closer.Close() }()

	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return nil, fmt.Errorf("读取 tar 失败: %w", err)
		}
		names = append(names, hdr.Name)
	}
}

// multiCloser 按逆序关闭多个资源（gzip 需要先于文件关闭）。
type multiCloser struct {
	closers []io.Closer
}

// Close 依次关闭所有资源，返回第一个错误。
func (m *multiCloser) Close() error {
	var first error
	for i := len(m.closers) - 1; i >= 0; i-- {
		if err := m.closers[i].Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// openTar 打开 tar（可选 gzip），返回需要关闭的资源与 tar 读取器。
func openTar(src string, gzipped bool) (io.Closer, *tar.Reader, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, nil, fmt.Errorf("打开归档失败: %w", err)
	}
	if !gzipped {
		return f, tar.NewReader(f), nil
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("解压 gzip 头失败: %w", err)
	}
	return &multiCloser{closers: []io.Closer{gz, f}}, tar.NewReader(gz), nil
}
