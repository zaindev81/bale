// Package tarball extracts npm .tgz package archives.
package tarball

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits on what a single archive may expand to, so that a hostile "zip
// bomb" tarball cannot exhaust disk or inodes. They are variables rather
// than constants only so that tests can shrink them.
var (
	maxUnpackedBytes int64 = 1 << 30 // 1 GiB
	maxEntries             = 100_000
)

// maxTrailingBytes caps how much data may follow the tar end-of-archive
// marker inside the gzip stream. Only block padding belongs there, so the
// bound is generous but finite.
const maxTrailingBytes int64 = 1 << 20 // 1 MiB

// errNoFiles reports an archive that yielded nothing worth installing.
var errNoFiles = errors.New("extract tarball: archive contains no files")

// Extract reads a gzipped tar from r and writes it under dest, stripping the
// first path component of every entry (npm tarballs are rooted at
// "package/"). dest is created if it does not exist, so a successful
// Extract always leaves dest in place.
//
// Only regular files and directories are extracted; other entry types
// (symlinks, devices, etc.) are skipped silently. Entry names are cleaned
// first: a name that is absolute, is "." or "..", or contains a ".."
// segment is an error rather than a skipped entry, as is one that would
// escape dest after joining.
//
// Extraction is bounded by maxUnpackedBytes and maxEntries, and an archive
// that contains no regular files at all is rejected.
func Extract(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}
	defer func() { _ = gz.Close() }()

	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}

	files, err := extractAll(gz, dest)
	if err != nil {
		return err
	}

	// Drain whatever the tar reader left behind so that the gzip reader
	// reaches and verifies the CRC32/length trailer, then report a corrupt
	// stream instead of silently accepting truncated data. Only the tar
	// end-of-archive padding is expected here, so the drain is bounded:
	// a hostile archive must not be able to make us inflate forever.
	n, err := io.Copy(io.Discard, io.LimitReader(gz, maxTrailingBytes+1))
	if err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}
	if n > maxTrailingBytes {
		return fmt.Errorf("extract tarball: more than %d bytes of trailing data after the archive", maxTrailingBytes)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("extract tarball: %w", err)
	}

	if files == 0 {
		return errNoFiles
	}
	return nil
}

// extractAll walks the tar stream in r, writing entries under dest. It
// returns the number of regular files written.
func extractAll(r io.Reader, dest string) (int, error) {
	tr := tar.NewReader(r)
	var files, entries int
	var total int64

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("extract tarball: %w", err)
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		default:
			continue
		}

		entries++
		if entries > maxEntries {
			return files, fmt.Errorf("extract tarball: archive contains more than %d entries", maxEntries)
		}

		rel, err := entryPath(hdr.Name)
		if err != nil {
			return files, err
		}
		if rel == "" {
			continue // the archive root itself, e.g. "package/"
		}

		target, err := safeJoin(dest, rel)
		if err != nil {
			return files, err
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0755); err != nil {
				return files, fmt.Errorf("extract tarball: %w", err)
			}
			continue
		}

		n, err := extractFile(tr, hdr, target, maxUnpackedBytes-total)
		if err != nil {
			return files, err
		}
		total += n
		files++
	}
}

// extractFile writes a single regular-file entry to target, copying at most
// budget bytes. It returns the number of bytes written.
func extractFile(r io.Reader, hdr *tar.Header, target string, budget int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return 0, fmt.Errorf("extract tarball: %w", err)
	}

	mode := os.FileMode(hdr.Mode) & 0o777
	if mode == 0 {
		mode = 0644
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, fmt.Errorf("extract tarball: %w", err)
	}
	// Read one byte past the remaining budget so that going over the limit
	// is detectable rather than silently truncating the file.
	n, err := io.Copy(f, io.LimitReader(r, budget+1))
	if err != nil {
		_ = f.Close() // the copy error is the one worth reporting
		return n, fmt.Errorf("extract tarball: %w", err)
	}
	if err := f.Close(); err != nil {
		return n, fmt.Errorf("extract tarball: %w", err)
	}
	if n > budget {
		return n, fmt.Errorf("extract tarball: unpacked size exceeds %d bytes", maxUnpackedBytes)
	}
	return n, nil
}

// entryPath normalizes a tar entry name and strips its first path component
// (npm tarballs are rooted at "package/"). It returns an empty path, and no
// error, for an entry that is nothing but that root component. Names that
// are absolute or walk outside the archive are errors.
func entryPath(name string) (string, error) {
	illegal := func() (string, error) {
		return "", fmt.Errorf("extract tarball: illegal entry path %q", name)
	}

	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if path.IsAbs(clean) || clean == "." || clean == ".." {
		return illegal()
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return illegal()
		}
	}

	_, rest, ok := strings.Cut(clean, "/")
	if !ok || rest == "" {
		return "", nil
	}
	return rest, nil
}

// safeJoin joins rel onto dest, rejecting entries that try to escape dest.
func safeJoin(dest, rel string) (string, error) {
	relClean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(relClean) || relClean == ".." || strings.HasPrefix(relClean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("extract tarball: illegal entry path %q", rel)
	}

	target := filepath.Join(dest, relClean)
	if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
		return "", fmt.Errorf("extract tarball: entry %q escapes destination", rel)
	}
	return target, nil
}
