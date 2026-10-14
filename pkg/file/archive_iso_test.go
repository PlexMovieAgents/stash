package file

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/text/encoding/japanese"
)

type isoTestNames int

const (
	// names are stored as given in the primary volume
	isoNamesPlain isoTestNames = iota
	// names are stored in a Joliet volume, and upper-cased in the primary volume
	isoNamesJoliet
	// names are stored in Rock Ridge entries, and upper-cased in the primary volume
	isoNamesRockRidge
)

type isoTestDir struct {
	name  string
	dirs  []*isoTestDir
	files []int // indexes of files
	lba   [2]int
}

func (d *isoTestDir) subdir(name string) *isoTestDir {
	for _, c := range d.dirs {
		if c.name == name {
			return c
		}
	}
	c := &isoTestDir{name: name}
	d.dirs = append(d.dirs, c)
	return c
}

func isoTestRecord(ident []byte, lba, size int, flags byte, susp []byte) []byte {
	n := 33 + len(ident) + (len(ident)+1)%2 + len(susp)
	n += n % 2
	b := make([]byte, n)
	b[0] = byte(n)
	binary.LittleEndian.PutUint32(b[2:], uint32(lba))   //nolint:gosec
	binary.BigEndian.PutUint32(b[6:], uint32(lba))      //nolint:gosec
	binary.LittleEndian.PutUint32(b[10:], uint32(size)) //nolint:gosec
	binary.BigEndian.PutUint32(b[14:], uint32(size))    //nolint:gosec
	copy(b[18:], []byte{126, 10, 13, 12, 30, 0, 32})    // 2026-10-13 12:30 +08:00
	b[25] = flags
	b[28], b[31] = 1, 1 // volume sequence number
	b[32] = byte(len(ident))
	copy(b[33:], ident)
	copy(b[33+len(ident)+(len(ident)+1)%2:], susp)
	return b
}

func isoTestDescriptor(typ byte, rootLBA int, joliet bool) []byte {
	d := make([]byte, isoSectorSize)
	d[0] = typ
	copy(d[1:], isoMagic)
	d[6] = 1
	if typ == 255 {
		return d
	}
	if joliet {
		copy(d[88:], "%/E")
	}
	binary.LittleEndian.PutUint16(d[128:], isoSectorSize)
	binary.BigEndian.PutUint16(d[130:], isoSectorSize)
	copy(d[156:], isoTestRecord([]byte{0}, rootLBA, isoSectorSize, isoFlagDirectory, nil))
	return d
}

// writeTestISO writes a minimal ISO 9660 image of files, with the names
// stored as given by names. Each directory must fit in one sector. If
// multiExtent is true, files are recorded in extents of one sector each.
func writeTestISO(t *testing.T, imgPath string, files []testArchiveFile, names isoTestNames, multiExtent bool) {
	t.Helper()

	root := &isoTestDir{}
	for i, f := range files {
		parts := strings.Split(f.name, "/")
		d := root
		for _, p := range parts[:len(parts)-1] {
			d = d.subdir(p)
		}
		d.files = append(d.files, i)
	}

	trees := 1
	if names == isoNamesJoliet {
		trees = 2
	}

	// system area, volume descriptors and terminator
	next := 16 + trees + 1
	var assign func(d *isoTestDir, tree int)
	assign = func(d *isoTestDir, tree int) {
		d.lba[tree] = next
		next++
		for _, c := range d.dirs {
			assign(c, tree)
		}
	}
	for tree := 0; tree < trees; tree++ {
		assign(root, tree)
	}

	fileLBA := make([]int, len(files))
	for i, f := range files {
		fileLBA[i] = next
		next += max(1, (len(f.content)+isoSectorSize-1)/isoSectorSize)
	}

	img := make([]byte, next*isoSectorSize)
	sector := func(lba int) []byte { return img[lba*isoSectorSize : (lba+1)*isoSectorSize] }

	copy(sector(16), isoTestDescriptor(1, root.lba[0], false))
	if trees == 2 {
		copy(sector(17), isoTestDescriptor(2, root.lba[1], true))
	}
	copy(sector(16+trees), isoTestDescriptor(255, 0, false))

	ident := func(name string, tree int, isFile bool) []byte {
		switch {
		case tree == 1:
			if isFile {
				name += ";1"
			}
			var b []byte
			for _, u := range utf16.Encode([]rune(name)) {
				b = binary.BigEndian.AppendUint16(b, u)
			}
			return b
		case names == isoNamesPlain:
			return []byte(name)
		case isFile:
			return []byte(strings.ToUpper(name) + ";1")
		default:
			return []byte(strings.ToUpper(name))
		}
	}
	nm := func(name string, tree int) []byte {
		if names != isoNamesRockRidge || tree != 0 {
			return nil
		}
		return append([]byte{'N', 'M', byte(5 + len(name)), 1, 0}, name...)
	}

	var write func(d, parent *isoTestDir, tree int)
	write = func(d, parent *isoTestDir, tree int) {
		var self []byte
		if names == isoNamesRockRidge && d == root {
			self = []byte{'S', 'P', 7, 1, 0xbe, 0xef, 0}
		}
		recs := isoTestRecord([]byte{0}, d.lba[tree], isoSectorSize, isoFlagDirectory, self)
		recs = append(recs, isoTestRecord([]byte{1}, parent.lba[tree], isoSectorSize, isoFlagDirectory, nil)...)
		for _, c := range d.dirs {
			recs = append(recs, isoTestRecord(ident(c.name, tree, false), c.lba[tree], isoSectorSize, isoFlagDirectory, nm(c.name, tree))...)
		}
		for _, i := range d.files {
			name := path.Base(files[i].name)
			content := files[i].content
			if !multiExtent || len(content) <= isoSectorSize {
				recs = append(recs, isoTestRecord(ident(name, tree, true), fileLBA[i], len(content), 0, nm(name, tree))...)
				continue
			}
			for off := 0; off < len(content); off += isoSectorSize {
				size := min(isoSectorSize, len(content)-off)
				var flags byte
				if off+size < len(content) {
					flags = isoFlagMultiExtent
				}
				recs = append(recs, isoTestRecord(ident(name, tree, true), fileLBA[i]+off/isoSectorSize, size, flags, nm(name, tree))...)
			}
		}
		if len(recs) > isoSectorSize {
			t.Fatalf("directory %q does not fit in a sector", d.name)
		}
		copy(sector(d.lba[tree]), recs)

		for _, c := range d.dirs {
			write(c, d, tree)
		}
	}
	for tree := 0; tree < trees; tree++ {
		write(root, root, tree)
	}

	for i, f := range files {
		copy(img[fileLBA[i]*isoSectorSize:], f.content)
	}

	if err := os.WriteFile(imgPath, img, 0644); err != nil {
		t.Fatal(err)
	}
}

// readTestArchive returns the contents of every file in the archive at path.
func readTestArchive(t *testing.T, archivePath string) map[string]string {
	t.Helper()

	zfs, err := openTestArchive(t, archivePath)
	if err != nil {
		t.Fatalf("newZipFS: %v", err)
	}
	defer zfs.Close()

	ret := make(map[string]string)
	err = SymWalk(zfs, archivePath, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		r, err := zfs.Open(p)
		if err != nil {
			return err
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(archivePath, p)
		ret[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("SymWalk: %v", err)
	}
	return ret
}

func TestISO_Names(t *testing.T) {
	dir := t.TempDir()

	sjis := func(s string) string {
		b, err := japanese.ShiftJIS.NewEncoder().String(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	tests := []struct {
		name  string
		files []testArchiveFile
		names isoTestNames
		want  map[string]string
	}{
		{
			"version numbers and trailing dots are removed from plain names",
			[]testArchiveFile{{"GALLERY/IMG_001.JPG;1", "one"}, {"GALLERY/README.;1", "readme"}},
			isoNamesPlain,
			map[string]string{"GALLERY/IMG_001.JPG": "one", "GALLERY/README": "readme"},
		},
		{
			"plain names in a local encoding are decoded",
			[]testArchiveFile{
				{sjis("写真集") + "/" + sjis("グラビア画像その一.jpg"), "one"},
				{sjis("写真集") + "/" + sjis("グラビア画像その二.jpg"), "two"},
				{sjis("写真集") + "/" + sjis("あとがきのテキスト.txt"), "three"},
			},
			isoNamesPlain,
			map[string]string{
				"写真集/グラビア画像その一.jpg": "one",
				"写真集/グラビア画像その二.jpg": "two",
				"写真集/あとがきのテキスト.txt": "three",
			},
		},
		{
			"Joliet names are preferred to plain names",
			[]testArchiveFile{{"写真/グラビア 001.jpg", "one"}},
			isoNamesJoliet,
			map[string]string{"写真/グラビア 001.jpg": "one"},
		},
		{
			"Rock Ridge names are preferred to plain names",
			[]testArchiveFile{{"写真/グラビア 001.jpg", "one"}},
			isoNamesRockRidge,
			map[string]string{"写真/グラビア 001.jpg": "one"},
		},
		{
			"invalid names are skipped",
			[]testArchiveFile{{"../1.jpg", "escape"}, {"ok/2.jpg", "two"}},
			isoNamesRockRidge,
			map[string]string{"ok/2.jpg": "two"},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.Repeat("x", i+1)+".iso")
			writeTestISO(t, p, tt.files, tt.names, false)

			got := readTestArchive(t, p)
			if len(got) != len(tt.want) {
				t.Errorf("files = %q, want %q", got, tt.want)
			}
			for name, content := range tt.want {
				if got[name] != content {
					t.Errorf("content of %q = %q, want %q", name, got[name], content)
				}
			}
		})
	}
}

func TestISO_Files(t *testing.T) {
	dir := t.TempDir()

	content := strings.Repeat("0123456789", 500)
	files := []testArchiveFile{{"gallery/big.jpg", content}, {"gallery/empty.jpg", ""}}

	t.Run("files in several extents are joined", func(t *testing.T) {
		p := filepath.Join(dir, "multi.iso")
		writeTestISO(t, p, files, isoNamesRockRidge, true)
		got := readTestArchive(t, p)
		if got["gallery/big.jpg"] != content {
			t.Errorf("content of big.jpg has %d bytes, want %d", len(got["gallery/big.jpg"]), len(content))
		}
		if v, ok := got["gallery/empty.jpg"]; !ok || v != "" {
			t.Errorf("empty.jpg = %q, %v", v, ok)
		}
	})

	t.Run("files are seekable", func(t *testing.T) {
		p := filepath.Join(dir, "seek.iso")
		writeTestISO(t, p, files, isoNamesRockRidge, false)

		zfs, err := openTestArchive(t, p)
		if err != nil {
			t.Fatal(err)
		}
		defer zfs.Close()

		r, err := zfs.Open(filepath.Join(p, "gallery", "big.jpg"))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		s, ok := r.(io.ReadSeeker)
		if !ok {
			t.Fatalf("%T is not an io.ReadSeeker", r)
		}
		if _, err := s.Seek(4995, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		tail, err := io.ReadAll(s)
		if err != nil || string(tail) != "56789" {
			t.Errorf("read after seek = %q, %v", tail, err)
		}
	})

	t.Run("directory loops are skipped", func(t *testing.T) {
		p := filepath.Join(dir, "loop.iso")
		writeTestISO(t, p, []testArchiveFile{{"A/1.JPG;1", "one"}}, isoNamesPlain, false)

		// point directory A at the root directory
		img, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		const rootLBA = 18
		root := img[rootLBA*isoSectorSize : (rootLBA+1)*isoSectorSize]
		i := bytes.Index(root, []byte{1, 'A'}) - 32
		binary.LittleEndian.PutUint32(root[i+2:], rootLBA)
		if err := os.WriteFile(p, img, 0644); err != nil {
			t.Fatal(err)
		}

		if got := readTestArchive(t, p); len(got) != 0 {
			t.Errorf("files = %q, want none", got)
		}
	})
}

func TestISO_Unsupported(t *testing.T) {
	dir := t.TempDir()

	// an image with only UDF has its volume recognition sequence where ISO
	// 9660 volume descriptors would be
	udfPath := filepath.Join(dir, "udf.iso")
	img := make([]byte, 20*isoSectorSize)
	for i, id := range []string{"BEA01", "NSR02", "TEA01"} {
		d := img[(16+i)*isoSectorSize:]
		copy(d[1:], id)
		d[6] = 1
	}
	if err := os.WriteFile(udfPath, img, 0644); err != nil {
		t.Fatal(err)
	}

	zfs, err := openTestArchive(t, udfPath)
	if err == nil {
		zfs.Close()
	}
	if !errors.Is(err, ErrUnsupportedArchive) || !strings.Contains(err.Error(), "UDF") {
		t.Errorf("newZipFS error = %v, want %v about UDF", err, ErrUnsupportedArchive)
	}
}
