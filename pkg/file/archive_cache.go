package file

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stashapp/stash/pkg/models"
	"golang.org/x/sync/singleflight"
)

const (
	// archiveHandleIdleTimeout is how long an archive stays open after it was
	// last used.
	archiveHandleIdleTimeout = time.Minute

	// archiveHandleMaxEntries is the most archives kept open while idle.
	archiveHandleMaxEntries = 32
)

// archiveHandles keeps recently used archives open, so that reading files
// from the same archive (such as viewing the images of a gallery) reads and
// parses the archive's index once.
var archiveHandles = newArchiveHandleCache()

type archiveHandle struct {
	path     string
	info     fs.FileInfo
	fs       *zipFS
	refs     int
	stale    bool
	lastUsed time.Time
	timer    *time.Timer
}

type archiveHandleCache struct {
	mu      sync.Mutex
	handles map[string]*archiveHandle
	group   singleflight.Group
}

func newArchiveHandleCache() *archiveHandleCache {
	return &archiveHandleCache{
		handles: make(map[string]*archiveHandle),
	}
}

// matches returns whether h was opened from the file described by info.
func (h *archiveHandle) matches(info fs.FileInfo) bool {
	return os.SameFile(h.info, info) && h.info.Size() == info.Size() && h.info.ModTime().Equal(info.ModTime())
}

func (c *archiveHandleCache) open(f models.FS, path string) (models.ZipFS, error) {
	info, err := f.Stat(path)
	if err != nil {
		return nil, err
	}

	// retry in the unlikely case that the archive opened is evicted or
	// replaced before it can be used
	for range 3 {
		if h := c.acquire(path, info); h != nil {
			return c.share(h), nil
		}

		_, err, _ := c.group.Do(path, func() (any, error) {
			c.mu.Lock()
			h := c.handles[path]
			c.mu.Unlock()
			if h != nil && h.matches(info) {
				return nil, nil
			}

			zfs, err := newZipFS(f, path, info.Size())
			if err != nil {
				return nil, err
			}

			c.insert(&archiveHandle{path: path, info: info, fs: zfs})
			return nil, nil
		})
		if err != nil {
			return nil, err
		}
	}

	return newZipFS(f, path, info.Size())
}

// acquire returns the open archive at path, if it matches info, marking it
// as in use.
func (c *archiveHandleCache) acquire(path string, info fs.FileInfo) *archiveHandle {
	var toClose *archiveHandle

	c.mu.Lock()
	h := c.handles[path]
	if h != nil {
		if h.matches(info) {
			h.refs++
			if h.timer != nil {
				h.timer.Stop()
				h.timer = nil
			}
		} else {
			// the file has changed since it was opened
			toClose = c.removeLocked(h)
			h = nil
		}
	}
	c.mu.Unlock()

	if toClose != nil {
		toClose.fs.Close()
	}
	return h
}

func (c *archiveHandleCache) insert(h *archiveHandle) {
	var toClose []*archiveHandle

	c.mu.Lock()
	if old := c.handles[h.path]; old != nil {
		if old := c.removeLocked(old); old != nil {
			toClose = append(toClose, old)
		}
	}
	c.handles[h.path] = h
	c.idleLocked(h)
	toClose = append(toClose, c.evictLocked()...)
	c.mu.Unlock()

	for _, h := range toClose {
		h.fs.Close()
	}
}

// idleLocked starts the timer to close h once it has been idle for long enough.
func (c *archiveHandleCache) idleLocked(h *archiveHandle) {
	h.lastUsed = time.Now()
	h.timer = time.AfterFunc(archiveHandleIdleTimeout, func() {
		c.mu.Lock()
		expired := h.refs == 0 && c.handles[h.path] == h
		if expired {
			delete(c.handles, h.path)
		}
		c.mu.Unlock()

		if expired {
			h.fs.Close()
		}
	})
}

// removeLocked removes h from the cache. It returns h if it should be closed
// now, or nil if it is in use, in which case it is closed when released.
func (c *archiveHandleCache) removeLocked(h *archiveHandle) *archiveHandle {
	h.stale = true
	if c.handles[h.path] == h {
		delete(c.handles, h.path)
	}
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	if h.refs == 0 {
		return h
	}
	return nil
}

// evictLocked removes the least recently used idle archives while there are
// too many open.
func (c *archiveHandleCache) evictLocked() []*archiveHandle {
	var ret []*archiveHandle

	for len(c.handles) > archiveHandleMaxEntries {
		var oldest *archiveHandle
		for _, h := range c.handles {
			if h.refs == 0 && (oldest == nil || h.lastUsed.Before(oldest.lastUsed)) {
				oldest = h
			}
		}
		if oldest == nil {
			break
		}
		ret = append(ret, c.removeLocked(oldest))
	}

	return ret
}

func (c *archiveHandleCache) release(h *archiveHandle) {
	closeNow := false

	c.mu.Lock()
	h.refs--
	if h.refs == 0 {
		if h.stale {
			closeNow = true
		} else {
			c.idleLocked(h)
		}
	}
	c.mu.Unlock()

	if closeNow {
		h.fs.Close()
	}
}

// evict closes the idle archives at or below path, and ensures that archives
// in use are reopened by the next caller and closed once released.
func (c *archiveHandleCache) evict(path string) {
	var toClose []*archiveHandle

	prefix := path + string(filepath.Separator)

	c.mu.Lock()
	for p, h := range c.handles {
		if p == path || strings.HasPrefix(p, prefix) {
			if h := c.removeLocked(h); h != nil {
				toClose = append(toClose, h)
			}
		}
	}
	c.mu.Unlock()

	for _, h := range toClose {
		h.fs.Close()
	}
}

func (c *archiveHandleCache) share(h *archiveHandle) *sharedZipFS {
	return &sharedZipFS{
		zipFS:   h.fs,
		release: func() { c.release(h) },
	}
}

// EvictArchives closes any open archives at or below path, so that they can
// be moved or deleted. This is required on Windows.
func EvictArchives(path string) {
	archiveHandles.evict(path)
}

// sharedZipFS is a zipFS from archiveHandles. Closing it releases it back to
// the cache.
type sharedZipFS struct {
	*zipFS
	once    sync.Once
	release func()
}

func (f *sharedZipFS) Close() error {
	f.once.Do(f.release)
	return nil
}

// OpenOnly returns a ReadCloser where calling Close will close the zip fs as well.
func (f *sharedZipFS) OpenOnly(name string) (io.ReadCloser, error) {
	r, err := f.Open(name)
	if err != nil {
		f.Close()
		return nil, err
	}

	if rs, ok := r.(io.ReadSeekCloser); ok {
		return &wrappedReadSeekCloser{ReadSeekCloser: rs, outer: f}, nil
	}

	return &wrappedReadCloser{
		ReadCloser: r,
		outer:      f,
	}, nil
}

type wrappedReadSeekCloser struct {
	io.ReadSeekCloser
	outer io.Closer
}

func (f *wrappedReadSeekCloser) Close() error {
	_ = f.ReadSeekCloser.Close()
	return f.outer.Close()
}
