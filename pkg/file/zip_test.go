package file

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type testArchiveFile struct {
	name    string
	content string
}

var testArchiveFiles = []testArchiveFile{
	{"gallery/1.jpg", "image one"},
	{"gallery/sub/2.jpg", "image two!"},
}

func writeTestZip(t *testing.T, path string) {
	t.Helper()

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, f := range testArchiveFiles {
		fw, err := w.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, f.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

func appendVint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// rar5Block encodes a RAR 5.0 block header: CRC32, header size, then the header body.
func rar5Block(body []byte) []byte {
	sized := appendVint(nil, uint64(len(body)))
	sized = append(sized, body...)

	ret := binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(sized))
	return append(ret, sized...)
}

// writeTestRar5 writes a minimal RAR 5.0 archive of testArchiveFiles using the
// "store" method (no compression), so that no RAR compressor is required.
// If solid is true, files after the first are flagged as solid.
// See https://www.rarlab.com/technote.htm for the format.
func writeTestRar5(t *testing.T, path string, solid bool) {
	t.Helper()
	writeTestRar5Files(t, path, testArchiveFiles, solid)
}

func writeTestRar5Files(t *testing.T, path string, files []testArchiveFile, solid bool) {
	t.Helper()

	const (
		blockArc    = 1
		blockFile   = 2
		blockEnd    = 5
		flagHasData = 0x2
		arcSolid    = 0x4
		fileHasCRC  = 0x4
		compSolid   = 0x40
		hostOSUnix  = 1
	)

	out := []byte("Rar!\x1a\x07\x01\x00")

	var arcFlags uint64
	if solid {
		arcFlags = arcSolid
	}
	main := appendVint(nil, blockArc)
	main = appendVint(main, 0) // block flags
	main = appendVint(main, arcFlags)
	out = append(out, rar5Block(main)...)

	for i, f := range files {
		var compFlags uint64 // method 0: store
		if solid && i > 0 {
			compFlags = compSolid
		}

		h := appendVint(nil, blockFile)
		h = appendVint(h, flagHasData)
		h = appendVint(h, uint64(len(f.content))) // data size
		h = appendVint(h, fileHasCRC)
		h = appendVint(h, uint64(len(f.content))) // unpacked size
		h = appendVint(h, 0o644)                  // attributes
		h = binary.LittleEndian.AppendUint32(h, crc32.ChecksumIEEE([]byte(f.content)))
		h = appendVint(h, compFlags)
		h = appendVint(h, hostOSUnix)
		h = appendVint(h, uint64(len(f.name)))
		h = append(h, f.name...)

		out = append(out, rar5Block(h)...)
		out = append(out, f.content...)
	}

	end := appendVint(nil, blockEnd)
	end = appendVint(end, 0) // block flags
	end = appendVint(end, 0) // end of archive flags
	out = append(out, rar5Block(end)...)

	if err := os.WriteFile(path, out, 0644); err != nil {
		t.Fatal(err)
	}
}

func openTestArchive(t *testing.T, path string) (*zipFS, error) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return newZipFS(&OsFS{}, path, info.Size())
}

func TestZipFS_Formats(t *testing.T) {
	dir := t.TempDir()

	zipPath := filepath.Join(dir, "gallery.zip")
	writeTestZip(t, zipPath)

	rarPath := filepath.Join(dir, "gallery.rar")
	writeTestRar5(t, rarPath, false)

	jolietPath := filepath.Join(dir, "joliet.iso")
	writeTestISO(t, jolietPath, testArchiveFiles, isoNamesJoliet, false)

	rockRidgePath := filepath.Join(dir, "rockridge.iso")
	writeTestISO(t, rockRidgePath, testArchiveFiles, isoNamesRockRidge, false)

	// detection is by content, not extension
	disguisedPath := filepath.Join(dir, "gallery.cbr")
	writeTestZip(t, disguisedPath)

	// zip files may have data prepended, such as a self-extractor stub
	prefixedPath := filepath.Join(dir, "prefixed.zip")
	writeTestZip(t, prefixedPath)
	zipData, err := os.ReadFile(prefixedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefixedPath, append([]byte("MZ stub data"), zipData...), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{"zip", zipPath},
		{"7z", filepath.Join("testdata", "gallery.7z")},
		{"rar", rarPath},
		{"iso with Joliet names", jolietPath},
		{"iso with Rock Ridge names", rockRidgePath},
		{"zip with cbr extension", disguisedPath},
		{"zip with prepended data", prefixedPath},
	}

	wantPaths := []string{".", "gallery", "gallery/1.jpg", "gallery/sub", "gallery/sub/2.jpg"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zfs, err := openTestArchive(t, tt.path)
			if err != nil {
				t.Fatalf("newZipFS: %v", err)
			}
			defer zfs.Close()

			var got []string
			err = SymWalk(zfs, tt.path, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(tt.path, path)
				if err != nil {
					return err
				}
				got = append(got, filepath.ToSlash(rel))
				return nil
			})
			if err != nil {
				t.Fatalf("SymWalk: %v", err)
			}
			slices.Sort(got)
			if !slices.Equal(got, wantPaths) {
				t.Errorf("walked paths = %v, want %v", got, wantPaths)
			}

			for _, f := range testArchiveFiles {
				name := filepath.Join(tt.path, filepath.FromSlash(f.name))

				info, err := zfs.Stat(name)
				if err != nil {
					t.Fatalf("Stat(%q): %v", f.name, err)
				}
				if info.Size() != int64(len(f.content)) {
					t.Errorf("Stat(%q).Size() = %d, want %d", f.name, info.Size(), len(f.content))
				}

				r, err := zfs.Open(name)
				if err != nil {
					t.Fatalf("Open(%q): %v", f.name, err)
				}
				data, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatalf("reading %q: %v", f.name, err)
				}
				if string(data) != f.content {
					t.Errorf("content of %q = %q, want %q", f.name, data, f.content)
				}
			}

			if _, err := zfs.OpenZip(filepath.Join(tt.path, "nested.zip"), 0); !errors.Is(err, errZipFSOpenZip) {
				t.Errorf("OpenZip error = %v, want %v", err, errZipFSOpenZip)
			}
		})
	}
}

func TestZipFS_Unsupported(t *testing.T) {
	dir := t.TempDir()

	garbagePath := filepath.Join(dir, "garbage.7z")
	if err := os.WriteFile(garbagePath, []byte("not an archive"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want error
	}{
		{"unknown format", garbagePath, ErrUnsupportedArchive},
		{"encrypted 7z headers", filepath.Join("testdata", "encrypted.7z"), ErrEncryptedArchive},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zfs, err := openTestArchive(t, tt.path)
			if err == nil {
				zfs.Close()
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("newZipFS error = %v, want %v", err, tt.want)
			}
			if !errors.Is(err, ErrUnsupportedArchive) {
				t.Errorf("newZipFS error = %v, should wrap %v", err, ErrUnsupportedArchive)
			}
		})
	}
}
