package tarball

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type entry struct {
	name     string
	typeflag byte
	mode     int64
	content  string
	linkname string
}

func buildTarGz(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Size:     int64(len(e.content)),
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if e.typeflag == tar.TypeReg && e.content != "" {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractStripsPackagePrefix(t *testing.T) {
	data := buildTarGz(t, []entry{
		{name: "package/package.json", typeflag: tar.TypeReg, mode: 0644, content: `{"name":"foo"}`},
	})
	dest := t.TempDir()
	if err := Extract(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	path := filepath.Join(dest, "package.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != `{"name":"foo"}` {
		t.Errorf("content = %q", got)
	}

	// The "package/" prefix should not appear as a subdirectory.
	if _, err := os.Stat(filepath.Join(dest, "package")); !os.IsNotExist(err) {
		t.Errorf("expected no package/ subdirectory, stat err = %v", err)
	}
}

func TestExtractNestedDirs(t *testing.T) {
	data := buildTarGz(t, []entry{
		{name: "package/lib/", typeflag: tar.TypeDir, mode: 0755},
		{name: "package/lib/nested/deep.js", typeflag: tar.TypeReg, mode: 0644, content: "module.exports = 1;"},
	})
	dest := t.TempDir()
	if err := Extract(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	path := filepath.Join(dest, "lib", "nested", "deep.js")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read nested file: %v", err)
	}
	if string(got) != "module.exports = 1;" {
		t.Errorf("content = %q", got)
	}
}

func TestExtractPreservesExecutableBit(t *testing.T) {
	data := buildTarGz(t, []entry{
		{name: "package/bin/cli.js", typeflag: tar.TypeReg, mode: 0755, content: "#!/usr/bin/env node"},
	})
	dest := t.TempDir()
	if err := Extract(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	path := filepath.Join(dest, "bin", "cli.js")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("expected executable bit set, mode = %v", info.Mode())
	}
}

func TestExtractRejectsPathEscape(t *testing.T) {
	data := buildTarGz(t, []entry{
		{name: "package/../../evil", typeflag: tar.TypeReg, mode: 0644, content: "pwned"},
	})
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	if err := os.Mkdir(dest, 0755); err != nil {
		t.Fatal(err)
	}

	err := Extract(bytes.NewReader(data), dest)
	if err == nil {
		t.Fatal("expected error for path escape entry")
	}

	// Nothing should have been written outside dest.
	if _, statErr := os.Stat(filepath.Join(parent, "evil")); !os.IsNotExist(statErr) {
		t.Errorf("evil file was created outside dest")
	}
	entries, readErr := os.ReadDir(dest)
	if readErr != nil {
		t.Fatalf("ReadDir dest: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("expected dest to remain empty, got %v", entries)
	}
}

func TestExtractSkipsUnsupportedTypes(t *testing.T) {
	data := buildTarGz(t, []entry{
		{name: "package/link", typeflag: tar.TypeSymlink, mode: 0777, linkname: "file.txt"},
		{name: "package/file.txt", typeflag: tar.TypeReg, mode: 0644, content: "hi"},
	})
	dest := t.TempDir()
	if err := Extract(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "link")); !os.IsNotExist(err) {
		t.Errorf("expected symlink entry to be skipped")
	}
	if _, err := os.Stat(filepath.Join(dest, "file.txt")); err != nil {
		t.Errorf("expected file.txt to be extracted: %v", err)
	}
}

func TestExtractRejectsInvalidGzip(t *testing.T) {
	err := Extract(bytes.NewReader([]byte("definitely not gzip")), t.TempDir())
	if err == nil {
		t.Fatal("expected error for invalid gzip input")
	}
}

func TestExtractNormalizesDotSlashPrefix(t *testing.T) {
	data := buildTarGz(t, []entry{
		{name: "./package/package.json", typeflag: tar.TypeReg, mode: 0644, content: `{"name":"foo"}`},
		{name: "./package/lib/./a.js", typeflag: tar.TypeReg, mode: 0644, content: "a"},
	})
	dest := t.TempDir()
	if err := Extract(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "package.json"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != `{"name":"foo"}` {
		t.Errorf("content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "lib", "a.js")); err != nil {
		t.Errorf("expected lib/a.js: %v", err)
	}
}

func TestExtractRejectsIllegalNames(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{"parent", "../x"},
		{"parent only", ".."},
		{"dot", "."},
		{"absolute", "/etc/passwd"},
		{"embedded parent", "package/../../evil"},
		{"dot slash parent", "./../x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildTarGz(t, []entry{
				{name: tt.entry, typeflag: tar.TypeReg, mode: 0644, content: "pwned"},
			})
			err := Extract(bytes.NewReader(data), t.TempDir())
			if err == nil {
				t.Fatalf("Extract(%q) = nil, want error", tt.entry)
			}
			if !strings.Contains(err.Error(), "illegal entry path") {
				t.Errorf("error = %q, want illegal entry path", err)
			}
		})
	}
}

func TestExtractCreatesDest(t *testing.T) {
	data := buildTarGz(t, []entry{
		{name: "package/package.json", typeflag: tar.TypeReg, mode: 0644, content: "{}"},
	})
	dest := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := Extract(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "package.json")); err != nil {
		t.Errorf("expected package.json under a freshly created dest: %v", err)
	}
}

func TestExtractRejectsEmptyArchive(t *testing.T) {
	tests := []struct {
		name    string
		entries []entry
	}{
		{"no entries at all", nil},
		{"root directory only", []entry{{name: "package/", typeflag: tar.TypeDir, mode: 0755}}},
		{"only unsupported types", []entry{
			{name: "package/link", typeflag: tar.TypeSymlink, mode: 0777, linkname: "nowhere"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildTarGz(t, tt.entries)
			dest := t.TempDir()
			err := Extract(bytes.NewReader(data), dest)
			if err == nil {
				t.Fatal("expected error for archive with no files")
			}
			if !strings.Contains(err.Error(), "archive contains no files") {
				t.Errorf("error = %q, want mention of empty archive", err)
			}
		})
	}
}

func TestExtractRejectsOversizedArchive(t *testing.T) {
	restore := maxUnpackedBytes
	maxUnpackedBytes = 32
	t.Cleanup(func() { maxUnpackedBytes = restore })

	// A tiny gzip payload that expands well past the budget.
	data := buildTarGz(t, []entry{
		{name: "package/big.txt", typeflag: tar.TypeReg, mode: 0644, content: strings.Repeat("a", 64*1024)},
	})
	if len(data) > 4096 {
		t.Fatalf("compressed archive is %d bytes; the test wants a small one", len(data))
	}

	err := Extract(bytes.NewReader(data), t.TempDir())
	if err == nil {
		t.Fatal("expected error for oversized archive")
	}
	if !strings.Contains(err.Error(), "unpacked size exceeds 32 bytes") {
		t.Errorf("error = %q, want mention of the size cap", err)
	}
}

func TestExtractBudgetIsCumulative(t *testing.T) {
	restore := maxUnpackedBytes
	maxUnpackedBytes = 10
	t.Cleanup(func() { maxUnpackedBytes = restore })

	data := buildTarGz(t, []entry{
		{name: "package/a.txt", typeflag: tar.TypeReg, mode: 0644, content: strings.Repeat("a", 6)},
		{name: "package/b.txt", typeflag: tar.TypeReg, mode: 0644, content: strings.Repeat("b", 6)},
	})
	err := Extract(bytes.NewReader(data), t.TempDir())
	if err == nil {
		t.Fatal("expected error once the cumulative budget is spent")
	}
	if !strings.Contains(err.Error(), "unpacked size exceeds 10 bytes") {
		t.Errorf("error = %q, want mention of the size cap", err)
	}
}

func TestExtractRejectsTooManyEntries(t *testing.T) {
	restore := maxEntries
	maxEntries = 3
	t.Cleanup(func() { maxEntries = restore })

	var entries []entry
	for i := 0; i < 10; i++ {
		entries = append(entries, entry{
			name:     "package/f" + strconv.Itoa(i) + ".txt",
			typeflag: tar.TypeReg,
			mode:     0644,
			content:  "x",
		})
	}
	err := Extract(bytes.NewReader(buildTarGz(t, entries)), t.TempDir())
	if err == nil {
		t.Fatal("expected error for too many entries")
	}
	if !strings.Contains(err.Error(), "more than 3 entries") {
		t.Errorf("error = %q, want mention of the entry cap", err)
	}
}

func TestExtractDetectsCorruptGzipTrailer(t *testing.T) {
	data := buildTarGz(t, []entry{
		{name: "package/package.json", typeflag: tar.TypeReg, mode: 0644, content: `{"name":"foo"}`},
	})
	// The last 8 bytes are the CRC32 and ISIZE trailer; corrupt the CRC.
	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-8] ^= 0xff

	err := Extract(bytes.NewReader(corrupt), t.TempDir())
	if err == nil {
		t.Fatal("expected error for a corrupt gzip trailer")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error = %q, want a checksum error", err)
	}
}
