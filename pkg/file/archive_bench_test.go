package file

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const (
	benchPages    = 300
	benchPageSize = 100 << 10
)

// writeBenchArchives creates archives of benchPages incompressible files,
// similar to a gallery of JPEG images, using the 7z and rar commands.
func writeBenchArchives(b *testing.B) (map[string]string, []string) {
	b.Helper()

	for _, cmd := range []string{"7z", "rar"} {
		if _, err := exec.LookPath(cmd); err != nil {
			b.Skipf("%s command not found", cmd)
		}
	}

	dir := b.TempDir()
	src := filepath.Join(dir, "src")
	rnd := rand.New(rand.NewSource(1))
	var names []string
	for i := range benchPages {
		name := fmt.Sprintf("gallery/%03d.jpg", i)
		data := make([]byte, benchPageSize)
		rnd.Read(data)
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0644); err != nil {
			b.Fatal(err)
		}
		names = append(names, name)
	}

	archives := map[string]struct {
		file string
		args []string
	}{
		"zip":          {"store.zip", []string{"7z", "a", "-tzip", "-mx=0"}},
		"zip deflated": {"deflate.zip", []string{"7z", "a", "-tzip", "-mx=5"}},
		"7z":           {"nonsolid.7z", []string{"7z", "a", "-ms=off"}},
		"7z solid":     {"solid.7z", []string{"7z", "a", "-ms=on"}},
		"rar":          {"nonsolid.rar", []string{"rar", "a", "-idq", "-s-"}},
		"rar solid":    {"solid.rar", []string{"rar", "a", "-idq", "-s"}},
	}

	ret := make(map[string]string)
	for name, a := range archives {
		out := filepath.Join(dir, a.file)
		cmd := exec.Command(a.args[0], append(a.args[1:], out, "gallery")...)
		cmd.Dir = src
		if output, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("%v: %v\n%s", a.args, err, output)
		}
		ret[name] = out
	}

	return ret, names
}

func readBenchFile(b *testing.B, f interface {
	Open(string) (io.ReadCloser, error)
}, name string) {
	r, err := f.Open(name)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		b.Fatal(err)
	}
	r.Close()
}

func openBenchArchive(b *testing.B, archive string) *zipFS {
	info, err := os.Stat(archive)
	if err != nil {
		b.Fatal(err)
	}
	zfs, err := newZipFS(&OsFS{}, archive, info.Size())
	if err != nil {
		b.Fatal(err)
	}
	return zfs
}

type zipFSOpener struct{ fs *zipFS }

func (o zipFSOpener) Open(name string) (io.ReadCloser, error) { return o.fs.Open(name) }

type osFSOpener struct{ archive string }

func (o osFSOpener) Open(name string) (io.ReadCloser, error) {
	zfs, err := (&OsFS{}).OpenZip(o.archive, 0)
	if err != nil {
		return nil, err
	}
	return zfs.OpenOnly(name)
}

// BenchmarkArchive compares the formats for:
//   - open per page: opening the archive for every page read, without the
//     disk cache. This is how every page was read before archives were
//     cached.
//   - browse: reading random pages through OsFS.OpenZip, as when viewing a
//     gallery, with the disk cache enabled.
//   - scan: reading every page in order from one open archive.
func BenchmarkArchive(b *testing.B) {
	archives, names := writeBenchArchives(b)
	middle := names[len(names)/2]

	for _, format := range []string{"zip", "zip deflated", "7z", "7z solid", "rar", "rar solid"} {
		archive := archives[format]
		join := func(name string) string { return filepath.Join(archive, filepath.FromSlash(name)) }

		b.Run(format+"/open per page", func(b *testing.B) {
			SetArchiveDiskCache("", 0)
			for b.Loop() {
				zfs := openBenchArchive(b, archive)
				readBenchFile(b, zipFSOpener{zfs}, join(middle))
				zfs.Close()
			}
		})

		b.Run(format+"/browse", func(b *testing.B) {
			SetArchiveDiskCache(filepath.Join(b.TempDir(), "archives"), 1<<30)
			defer SetArchiveDiskCache("", 0)
			defer EvictArchives(archive)

			rnd := rand.New(rand.NewSource(1))
			o := osFSOpener{archive}
			for b.Loop() {
				readBenchFile(b, o, join(names[rnd.Intn(len(names))]))
			}
		})

		b.Run(format+"/scan", func(b *testing.B) {
			SetArchiveDiskCache(filepath.Join(b.TempDir(), "archives"), 1<<30)
			defer SetArchiveDiskCache("", 0)

			for b.Loop() {
				zfs := openBenchArchive(b, archive)
				for _, name := range names {
					readBenchFile(b, zipFSOpener{zfs}, join(name))
				}
				zfs.Close()
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N)/benchPages, "µs/page")
		})
	}
}
