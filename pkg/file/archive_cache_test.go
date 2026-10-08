package file

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// manyTestFiles matches the contents of testdata/solid_many.7z and
// testdata/solid_many.rar, which are a single solid block each.
func manyTestFiles() []testArchiveFile {
	var ret []testArchiveFile
	for i := range 40 {
		ret = append(ret, testArchiveFile{
			name:    fmt.Sprintf("gallery/%02d.jpg", i),
			content: strings.Repeat(fmt.Sprintf("image %02d\n", i), 16),
		})
	}
	return ret
}

type solidTestCase struct {
	name  string
	path  string
	files []testArchiveFile
}

func solidTestCases(t *testing.T) []solidTestCase {
	t.Helper()

	storedRar := filepath.Join(t.TempDir(), "stored.rar")
	writeTestRar5Files(t, storedRar, manyTestFiles(), true)

	return []solidTestCase{
		{"7z", filepath.Join("testdata", "solid_many.7z"), manyTestFiles()},
		{"rar", filepath.Join("testdata", "solid_many.rar"), manyTestFiles()},
		{"stored rar", storedRar, manyTestFiles()},
	}
}

// setTestDiskCache enables the disk cache in a new directory for the
// duration of the test, returning the directory.
func setTestDiskCache(t *testing.T, maxBytes int64) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "archives")
	SetArchiveDiskCache(dir, maxBytes)
	t.Cleanup(func() { SetArchiveDiskCache("", 0) })
	return dir
}

func solidOf(t *testing.T, zfs *zipFS) *solidReader {
	t.Helper()

	afs, ok := zfs.fsys.(*archiveFS)
	if !ok || afs.solid == nil {
		t.Fatalf("archive has no solid blocks")
	}
	return afs.solid
}

func readArchiveFile(t *testing.T, zfs *zipFS, archivePath string, name string) string {
	t.Helper()

	r, err := zfs.Open(filepath.Join(archivePath, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading %q: %v", name, err)
	}
	return string(data)
}

func checkArchiveFile(t *testing.T, zfs *zipFS, archivePath string, f testArchiveFile) {
	t.Helper()

	if got := readArchiveFile(t, zfs, archivePath, f.name); got != f.content {
		t.Errorf("content of %q = %q, want %q", f.name, got, f.content)
	}
}

// cachedFiles returns the regular files below dir.
func cachedFiles(t *testing.T, dir string) []string {
	t.Helper()

	var ret []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			ret = append(ret, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ret
}

// TestSolidArchive_Sequential checks that reading a solid archive in order,
// as the scanner does, decodes each file once and does not write to disk.
func TestSolidArchive_Sequential(t *testing.T) {
	for _, tt := range solidTestCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			cacheDir := setTestDiskCache(t, 1<<30)

			zfs, err := openTestArchive(t, tt.path)
			if err != nil {
				t.Fatalf("newZipFS: %v", err)
			}
			defer zfs.Close()

			var walked []string
			err = SymWalk(zfs, tt.path, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}

				// the scanner opens each file several times
				for range 3 {
					r, err := zfs.Open(path)
					if err != nil {
						return err
					}
					if _, err := io.Copy(io.Discard, r); err != nil {
						return err
					}
					r.Close()
				}

				rel, _ := filepath.Rel(tt.path, path)
				walked = append(walked, filepath.ToSlash(rel))
				return nil
			})
			if err != nil {
				t.Fatalf("SymWalk: %v", err)
			}

			if len(walked) != len(tt.files) {
				t.Errorf("walked %d files, want %d", len(walked), len(tt.files))
			}
			if got := solidOf(t, zfs).decoded.Load(); got != int64(len(tt.files)) {
				t.Errorf("decoded %d files, want %d", got, len(tt.files))
			}
			if files := cachedFiles(t, cacheDir); len(files) > 0 {
				t.Errorf("sequential reads wrote to the disk cache: %v", files)
			}

			for _, f := range tt.files {
				checkArchiveFile(t, zfs, tt.path, f)
			}
		})
	}
}

// TestSolidArchive_Random checks that reading a solid archive out of order
// extracts its block to the disk cache once.
func TestSolidArchive_Random(t *testing.T) {
	for _, tt := range solidTestCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			cacheDir := setTestDiskCache(t, 1<<30)

			zfs, err := openTestArchive(t, tt.path)
			if err != nil {
				t.Fatalf("newZipFS: %v", err)
			}
			defer zfs.Close()

			for _, f := range slices.Backward(tt.files) {
				checkArchiveFile(t, zfs, tt.path, f)
			}

			if got := solidOf(t, zfs).decoded.Load(); got != int64(len(tt.files)) {
				t.Errorf("decoded %d files, want %d", got, len(tt.files))
			}

			files := cachedFiles(t, cacheDir)
			if len(files) != len(tt.files) {
				t.Errorf("cached %d files, want %d: %v", len(files), len(tt.files), files)
			}

			// files from the disk cache can seek
			r, err := zfs.Open(filepath.Join(tt.path, "gallery", "10.jpg"))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, ok := r.(io.Seeker); !ok {
				t.Errorf("file from disk cache is a %T, want an io.Seeker", r)
			}
			info, err := r.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if info.Name() != "10.jpg" || info.Size() != int64(len(tt.files[10].content)) {
				t.Errorf("Stat() = %s (%d bytes), want the archive entry", info.Name(), info.Size())
			}
		})
	}
}

// TestSolidArchive_NoDiskCache checks that solid archives can be read out of
// order without the disk cache, or when a block does not fit in it.
func TestSolidArchive_NoDiskCache(t *testing.T) {
	for _, tt := range solidTestCases(t) {
		for _, maxBytes := range []int64{0, 10} {
			t.Run(fmt.Sprintf("%s max %d", tt.name, maxBytes), func(t *testing.T) {
				cacheDir := setTestDiskCache(t, maxBytes)

				zfs, err := openTestArchive(t, tt.path)
				if err != nil {
					t.Fatalf("newZipFS: %v", err)
				}
				defer zfs.Close()

				order := rand.New(rand.NewSource(1)).Perm(len(tt.files))
				for _, i := range order {
					checkArchiveFile(t, zfs, tt.path, tt.files[i])
				}

				if files := cachedFiles(t, cacheDir); len(files) > 0 {
					t.Errorf("wrote to the disk cache: %v", files)
				}
			})
		}
	}
}

// TestSolidArchive_Concurrent reads a solid archive from many goroutines at
// once. Run with -race.
func TestSolidArchive_Concurrent(t *testing.T) {
	for _, tt := range solidTestCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			cacheDir := setTestDiskCache(t, 1<<30)

			zfs, err := openTestArchive(t, tt.path)
			if err != nil {
				t.Fatalf("newZipFS: %v", err)
			}
			defer zfs.Close()

			var wg sync.WaitGroup
			for g := range 16 {
				wg.Go(func() {
					rnd := rand.New(rand.NewSource(int64(g)))
					for range 20 {
						checkArchiveFile(t, zfs, tt.path, tt.files[rnd.Intn(len(tt.files))])
					}
				})
			}
			wg.Wait()

			// the block is extracted at most once
			if files := cachedFiles(t, cacheDir); len(files) > len(tt.files) {
				t.Errorf("cached %d files, want at most %d", len(files), len(tt.files))
			}
		})
	}
}

func copyTestFile(t *testing.T, src, dst string) {
	t.Helper()

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDiskCache_Eviction(t *testing.T) {
	files := manyTestFiles()
	var blockSize int64
	for _, f := range files {
		blockSize += int64(len(f.content))
	}

	// room for one block only
	cacheDir := setTestDiskCache(t, blockSize*3/2)

	dir := t.TempDir()
	var archives []*zipFS
	var paths []string
	for _, name := range []string{"a.7z", "b.7z", "c.7z"} {
		p := filepath.Join(dir, name)
		copyTestFile(t, filepath.Join("testdata", "solid_many.7z"), p)

		zfs, err := openTestArchive(t, p)
		if err != nil {
			t.Fatal(err)
		}
		defer zfs.Close()

		archives = append(archives, zfs)
		paths = append(paths, p)
	}

	cached := func() []string {
		t.Helper()
		entries, err := os.ReadDir(cacheDir)
		if err != nil {
			t.Fatal(err)
		}
		var ret []string
		for _, e := range entries {
			ret = append(ret, e.Name())
		}
		return ret
	}
	keyOf := func(i int) string {
		return solidOf(t, archives[i]).cacheKey
	}
	wantCached := func(want ...int) {
		t.Helper()
		var keys []string
		for _, i := range want {
			keys = append(keys, keyOf(i))
		}
		slices.Sort(keys)
		if got := cached(); !slices.Equal(got, keys) {
			t.Errorf("cached archives = %v, want %v", got, keys)
		}
	}

	// reading the last file first extracts the block
	readArchiveFile(t, archives[0], paths[0], files[39].name)
	wantCached(0)

	// a file kept open prevents its block from being evicted, so the next
	// block is evicted once it is no longer in use
	held, err := archives[0].Open(filepath.Join(paths[0], filepath.FromSlash(files[0].name)))
	if err != nil {
		t.Fatal(err)
	}
	readArchiveFile(t, archives[1], paths[1], files[39].name)
	wantCached(0)
	held.Close()
	wantCached(0)

	// otherwise the least recently used block is evicted
	readArchiveFile(t, archives[1], paths[1], files[39].name)
	wantCached(1)
	decoded := solidOf(t, archives[1]).decoded.Load()
	if got := readArchiveFile(t, archives[1], paths[1], files[5].name); got != files[5].content {
		t.Errorf("content = %q, want %q", got, files[5].content)
	}
	if solidOf(t, archives[1]).decoded.Load() != decoded {
		t.Errorf("cached file was decoded again")
	}

	readArchiveFile(t, archives[2], paths[2], files[39].name)
	wantCached(2)

	// a new cache in the same directory picks up the existing block
	SetArchiveDiskCache("", 0)
	SetArchiveDiskCache(cacheDir, blockSize*3/2)
	c := getArchiveDiskCache()
	if len(c.blocks) != 1 || c.total != blockSize {
		t.Errorf("reloaded cache has %d blocks of %d bytes, want 1 of %d", len(c.blocks), c.total, blockSize)
	}

	// shrinking the cache evicts
	SetArchiveDiskCache(cacheDir, 1)
	wantCached()
}

func TestArchive_InvalidPaths(t *testing.T) {
	files := []testArchiveFile{
		{"../evil.jpg", "evil"},
		{"gallery/ok.jpg", "ok"},
		{"/abs.jpg", "abs"},
		{`gallery\back.jpg`, "back"},
	}

	for _, solid := range []bool{false, true} {
		t.Run(fmt.Sprintf("solid %v", solid), func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "evil.rar")
			writeTestRar5Files(t, p, files, solid)

			zfs, err := openTestArchive(t, p)
			if err != nil {
				t.Fatal(err)
			}
			defer zfs.Close()

			var walked []string
			err = SymWalk(zfs, p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(p, path)
				walked = append(walked, filepath.ToSlash(rel))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			want := []string{".", "gallery", "gallery/back.jpg", "gallery/ok.jpg"}
			slices.Sort(walked)
			if !slices.Equal(walked, want) {
				t.Errorf("walked %v, want %v", walked, want)
			}

			checkArchiveFile(t, zfs, p, files[1])
			checkArchiveFile(t, zfs, p, testArchiveFile{"gallery/back.jpg", "back"})

			if !solid {
				return
			}

			// extracting the block writes only the valid files, within its
			// directory
			out := filepath.Join(dir, "out")
			if err := os.Mkdir(out, 0755); err != nil {
				t.Fatal(err)
			}
			s := solidOf(t, zfs)
			if err := s.extractBlock(s.blocks[0], out); err != nil {
				t.Fatal(err)
			}

			got := cachedFiles(t, dir)
			slices.Sort(got)
			want = []string{"evil.rar", "out/gallery/back.jpg", "out/gallery/ok.jpg"}
			if !slices.Equal(got, want) {
				t.Errorf("files after extraction = %v, want %v", got, want)
			}
		})
	}
}

func TestArchiveHandles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gallery.zip")
	writeTestZip(t, p)

	c := newArchiveHandleCache()
	osFS := &OsFS{}

	open := func() *sharedZipFS {
		t.Helper()
		zfs, err := c.open(osFS, p)
		if err != nil {
			t.Fatal(err)
		}
		return zfs.(*sharedZipFS)
	}

	refs := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		if h := c.handles[p]; h != nil {
			return h.refs
		}
		return -1
	}

	a := open()
	b := open()
	if a.zipFS != b.zipFS {
		t.Errorf("archive was opened twice")
	}
	if n := refs(); n != 2 {
		t.Errorf("refs = %d, want 2", n)
	}

	// closing twice releases once
	a.Close()
	a.Close()
	if n := refs(); n != 1 {
		t.Errorf("refs = %d, want 1", n)
	}

	// OpenOnly releases when the file is closed
	r, err := b.OpenOnly(filepath.Join(p, "gallery", "1.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if n := refs(); n != 1 {
		t.Errorf("refs = %d with a file open, want 1", n)
	}
	r.Close()
	if n := refs(); n != 0 {
		t.Errorf("refs = %d, want 0", n)
	}

	// a changed file is reopened
	old := open()
	writeTestRar5(t, p, false)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	reopened := open()
	if reopened.zipFS == old.zipFS {
		t.Errorf("changed archive was not reopened")
	}
	checkArchiveFile(t, reopened.zipFS, p, testArchiveFiles[0])
	old.Close()
	reopened.Close()

	// evicting closes idle archives
	c.evict(dir)
	if n := refs(); n != -1 {
		t.Errorf("archive still cached after eviction")
	}

	// evicting an archive in use closes it when released
	inUse := open()
	c.evict(p)
	if n := refs(); n != -1 {
		t.Errorf("archive still cached after eviction")
	}
	if again := open(); again.zipFS == inUse.zipFS {
		t.Errorf("evicted archive was reused")
	} else {
		again.Close()
	}
	checkArchiveFile(t, inUse.zipFS, p, testArchiveFiles[0])
	inUse.Close()
}

func TestOsFSOpenZip_Shared(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gallery.zip")
	writeTestZip(t, p)
	t.Cleanup(func() { EvictArchives(p) })

	osFS := &OsFS{}
	a, err := osFS.OpenZip(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := osFS.OpenZip(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if a.(*sharedZipFS).zipFS != b.(*sharedZipFS).zipFS {
		t.Errorf("OsFS.OpenZip did not share the open archive")
	}
}
