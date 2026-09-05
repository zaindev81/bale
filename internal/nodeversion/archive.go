package nodeversion

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Node distributions include npm/npx symlinks. Create links only after all
// regular files, confine them to the archive root, then resolve every link.
func extract(ctx context.Context, reader io.Reader, dest, archiveRoot string) error {
	// macOS temporary paths may themselves contain /var -> /private/var.
	var err error
	dest, err = filepath.EvalSymlinks(dest)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	type link struct{ name, target string }
	var links []link
	var total int64
	seen := map[string]bool{}
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if count >= 100_000 {
			return errors.New("archive has too many entries")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == archiveRoot && header.Typeflag == tar.TypeDir {
			continue
		}
		if !strings.HasPrefix(name, archiveRoot+"/") {
			return fmt.Errorf("invalid archive path %q", header.Name)
		}
		rel := strings.TrimPrefix(name, archiveRoot+"/")
		if !safeRelative(rel) || path.Clean(rel) != rel {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		if seen[rel] {
			return fmt.Errorf("duplicate archive path %q", rel)
		}
		seen[rel] = true
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > (1<<30)-total {
				return errors.New("archive exceeds unpacked size limit")
			}
			total += header.Size
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644|os.FileMode(header.Mode)&0111)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tr, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if header.Linkname == "" || path.IsAbs(header.Linkname) || strings.ContainsAny(header.Linkname, "\\\x00") || !safeRelative(path.Join(path.Dir(rel), header.Linkname)) {
				return fmt.Errorf("unsafe archive symlink %q -> %q", rel, header.Linkname)
			}
			links = append(links, link{target, header.Linkname})
		default:
			return fmt.Errorf("unsupported archive entry type for %q", rel)
		}
	}
	// Read the trailer so gzip CRC errors aren't hidden by tar's end marker.
	n, err := io.Copy(io.Discard, io.LimitReader(gz, (1<<20)+1))
	if err != nil {
		return err
	}
	if n > 1<<20 {
		return errors.New("excess trailing archive data")
	}
	for _, l := range links {
		if err := os.Symlink(l.target, l.name); err != nil {
			return err
		}
	}
	for _, l := range links {
		resolved, err := filepath.EvalSymlinks(l.name)
		if err != nil {
			return fmt.Errorf("invalid archive symlink: %w", err)
		}
		rel, err := filepath.Rel(dest, resolved)
		if err != nil || !safeRelative(filepath.ToSlash(rel)) {
			return errors.New("archive symlink escapes installation")
		}
	}
	return nil
}

func safeRelative(name string) bool {
	return name != "" && name != "." && name != ".." && !path.IsAbs(name) && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\\x00")
}
