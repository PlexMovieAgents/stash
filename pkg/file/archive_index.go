package file

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// archiveEntry is a file or directory in a 7z or rar archive.
type archiveEntry struct {
	name    string // slash-separated path relative to the archive root
	size    int64
	modTime time.Time
	mode    fs.FileMode
	isDir   bool

	children []*archiveEntry // directories only

	// open opens a file that is not stored in a solid block
	open func() (io.ReadCloser, error)

	// block is the solid block holding the file, or nil
	block *solidBlock
	// index is the position of the file within its solid block
	index int
}

func (e *archiveEntry) Name() string {
	if e.name == "." {
		return "."
	}
	return path.Base(e.name)
}
func (e *archiveEntry) Size() int64        { return e.size }
func (e *archiveEntry) Mode() fs.FileMode  { return e.mode }
func (e *archiveEntry) ModTime() time.Time { return e.modTime }
func (e *archiveEntry) IsDir() bool        { return e.isDir }
func (e *archiveEntry) Sys() any           { return nil }

// archiveIndex is the directory tree of a 7z or rar archive, built from its
// headers so that listing and stat calls do not need to decompress anything.
type archiveIndex struct {
	entries map[string]*archiveEntry
}

func newArchiveIndex() *archiveIndex {
	root := &archiveEntry{name: ".", isDir: true, mode: fs.ModeDir | 0555}
	return &archiveIndex{
		entries: map[string]*archiveEntry{".": root},
	}
}

// cleanArchivePath converts a name from an archive header to a valid fs.FS
// path, returning false if it is not one. This rejects absolute paths and
// paths that escape the archive root.
func cleanArchivePath(name string) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimSuffix(name, "/")
	if !fs.ValidPath(name) || name == "." {
		return "", false
	}
	return name, true
}

// add adds e to the index, creating any missing parent directories.
// It returns false if e conflicts with an existing entry, in which case e is
// not added.
func (x *archiveIndex) add(e *archiveEntry) bool {
	if existing := x.entries[e.name]; existing != nil {
		if existing.isDir != e.isDir {
			return false
		}
		if e.isDir {
			// keep the existing children, but take the header's metadata
			existing.modTime = e.modTime
			return true
		}
		// later files replace earlier ones with the same name
		parent := x.entries[path.Dir(e.name)]
		for i, c := range parent.children {
			if c == existing {
				parent.children[i] = e
			}
		}
		x.entries[e.name] = e
		return true
	}

	parent := x.dir(path.Dir(e.name))
	if parent == nil {
		return false
	}

	if e.isDir {
		e.mode |= fs.ModeDir
	}
	x.entries[e.name] = e
	parent.children = append(parent.children, e)
	return true
}

// dir returns the directory entry for name, creating it and its parents if
// needed. It returns nil if a file is in the way.
func (x *archiveIndex) dir(name string) *archiveEntry {
	if e := x.entries[name]; e != nil {
		if !e.isDir {
			return nil
		}
		return e
	}

	parent := x.dir(path.Dir(name))
	if parent == nil {
		return nil
	}

	e := &archiveEntry{name: name, isDir: true, mode: fs.ModeDir | 0555}
	x.entries[name] = e
	parent.children = append(parent.children, e)
	return e
}

// archiveFS is the fs.FS of a 7z or rar archive.
type archiveFS struct {
	index *archiveIndex
	solid *solidReader // nil if the archive has no solid blocks
}

func (a *archiveFS) lookup(op, name string) (*archiveEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	e := a.index.entries[name]
	if e == nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return e, nil
}

func (a *archiveFS) Stat(name string) (fs.FileInfo, error) {
	return a.lookup("stat", name)
}

func (a *archiveFS) Open(name string) (fs.File, error) {
	e, err := a.lookup("open", name)
	if err != nil {
		return nil, err
	}

	if e.isDir {
		return &archiveDirFile{entry: e}, nil
	}

	if e.block != nil {
		f, err := a.solid.open(e)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return f, nil
	}

	rc, err := e.open()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &archiveStreamFile{Reader: rc, closer: rc, entry: e}, nil
}

func (a *archiveFS) Close() error {
	if a.solid != nil {
		return a.solid.Close()
	}
	return nil
}

var errNotDir = errors.New("not a directory")

// archiveDirFile is an open directory in an archiveFS.
type archiveDirFile struct {
	entry  *archiveEntry
	sorted []*archiveEntry
	offset int
}

func (d *archiveDirFile) Stat() (fs.FileInfo, error) { return d.entry, nil }
func (d *archiveDirFile) Close() error               { return nil }

func (d *archiveDirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.entry.name, Err: errors.New("is a directory")}
}

func (d *archiveDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.sorted == nil {
		d.sorted = append([]*archiveEntry{}, d.entry.children...)
		sort.Slice(d.sorted, func(i, j int) bool { return d.sorted[i].name < d.sorted[j].name })
	}

	remaining := d.sorted[d.offset:]
	if n > 0 {
		if len(remaining) == 0 {
			return nil, io.EOF
		}
		if n < len(remaining) {
			remaining = remaining[:n]
		}
	}
	d.offset += len(remaining)

	ret := make([]fs.DirEntry, len(remaining))
	for i, e := range remaining {
		ret[i] = fs.FileInfoToDirEntry(e)
	}
	return ret, nil
}

// archiveStreamFile is a file in an archiveFS that can only be read once,
// from start to finish.
type archiveStreamFile struct {
	io.Reader
	closer io.Closer
	entry  *archiveEntry
}

func (f *archiveStreamFile) Stat() (fs.FileInfo, error) { return f.entry, nil }
func (f *archiveStreamFile) Close() error               { return f.closer.Close() }

// archiveMemFile is a file in an archiveFS whose contents are held in memory.
type archiveMemFile struct {
	*bytes.Reader
	entry *archiveEntry
}

func (f *archiveMemFile) Stat() (fs.FileInfo, error) { return f.entry, nil }
func (f *archiveMemFile) Close() error               { return nil }

func (f *archiveMemFile) ReadDir(int) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "readdir", Path: f.entry.name, Err: errNotDir}
}
