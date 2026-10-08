package file

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stashapp/stash/pkg/logger"
	"golang.org/x/sync/singleflight"
)

// archiveCacheTouchInterval limits how often the modification time of a
// cached block is updated to record its use. The modification time is used to
// restore the least-recently-used order on startup.
const archiveCacheTouchInterval = time.Minute

const archiveCacheTmpSuffix = ".tmp-"

var archiveDisk atomic.Pointer[archiveDiskCache]

func getArchiveDiskCache() *archiveDiskCache {
	return archiveDisk.Load()
}

// SetArchiveDiskCache sets the directory where solid blocks of 7z and rar
// archives are extracted for random access, and the maximum total size of the
// extracted files. An empty dir or a maxBytes <= 0 disables the cache.
func SetArchiveDiskCache(dir string, maxBytes int64) {
	if dir == "" || maxBytes <= 0 {
		archiveDisk.Store(nil)
		return
	}

	if c := archiveDisk.Load(); c != nil && c.dir == dir {
		c.setMaxBytes(maxBytes)
		return
	}

	c, err := newArchiveDiskCache(dir, maxBytes)
	if err != nil {
		logger.Warnf("Archive cache disabled: %v", err)
		archiveDisk.Store(nil)
		return
	}
	archiveDisk.Store(c)
}

// archiveDiskCache holds extracted solid blocks, evicting the least recently
// used when it exceeds its maximum size. Each block is stored in
// <dir>/<archive key>/b<block id>.
type archiveDiskCache struct {
	dir string

	mu       sync.Mutex
	maxBytes int64
	blocks   map[string]*diskBlock // keyed by "<archive key>/b<block id>"
	total    int64

	group singleflight.Group
}

type diskBlock struct {
	size     int64
	lastUsed time.Time
	touched  time.Time
	refs     int
}

func newArchiveDiskCache(dir string, maxBytes int64) (*archiveDiskCache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	c := &archiveDiskCache{
		dir:      dir,
		maxBytes: maxBytes,
		blocks:   make(map[string]*diskBlock),
	}

	if err := c.load(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	evicted := c.evictLocked(0)
	c.mu.Unlock()
	c.remove(evicted)

	return c, nil
}

// load reads the blocks extracted by previous runs.
func (c *archiveDiskCache) load() error {
	archives, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}

	for _, a := range archives {
		if !a.IsDir() {
			continue
		}

		archiveDir := filepath.Join(c.dir, a.Name())
		blocks, err := os.ReadDir(archiveDir)
		if err != nil {
			logger.Warnf("Reading archive cache %s: %v", archiveDir, err)
			continue
		}

		for _, b := range blocks {
			blockDir := filepath.Join(archiveDir, b.Name())
			if strings.Contains(b.Name(), archiveCacheTmpSuffix) || !b.IsDir() {
				// left over from an interrupted extraction
				_ = os.RemoveAll(blockDir)
				continue
			}

			info, err := b.Info()
			if err != nil {
				continue
			}

			size, err := dirSize(blockDir)
			if err != nil {
				logger.Warnf("Reading archive cache %s: %v", blockDir, err)
				continue
			}

			c.blocks[a.Name()+"/"+b.Name()] = &diskBlock{
				size:     size,
				lastUsed: info.ModTime(),
				touched:  info.ModTime(),
			}
			c.total += size
		}

		// remove the archive directory if it is now empty
		_ = os.Remove(archiveDir)
	}

	return nil
}

func dirSize(dir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (c *archiveDiskCache) blockDir(key string) string {
	return filepath.Join(c.dir, filepath.FromSlash(key))
}

func (c *archiveDiskCache) setMaxBytes(maxBytes int64) {
	c.mu.Lock()
	c.maxBytes = maxBytes
	evicted := c.evictLocked(0)
	c.mu.Unlock()
	c.remove(evicted)
}

// fits returns whether a block of the given size can be cached.
func (c *archiveDiskCache) fits(size int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return size <= c.maxBytes
}

// evictLocked removes least recently used blocks that are not in use until
// need more bytes fit within the maximum size. It returns the directories to
// delete once c.mu is released.
func (c *archiveDiskCache) evictLocked(need int64) []string {
	var dirs []string

	for c.total+need > c.maxBytes {
		var oldestKey string
		var oldest *diskBlock
		for k, b := range c.blocks {
			if b.refs == 0 && (oldest == nil || b.lastUsed.Before(oldest.lastUsed)) {
				oldestKey, oldest = k, b
			}
		}
		if oldest == nil {
			break
		}

		delete(c.blocks, oldestKey)
		c.total -= oldest.size
		dirs = append(dirs, c.blockDir(oldestKey))
	}

	return dirs
}

func (c *archiveDiskCache) remove(dirs []string) {
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			logger.Debugf("Removing cached archive block %s: %v", d, err)
		}
		// remove the archive directory if it is now empty
		_ = os.Remove(filepath.Dir(d))
	}
}

// acquire marks a cached block as in use, returning its directory, or false
// if the block is not cached.
func (c *archiveDiskCache) acquire(key string) (string, bool) {
	now := time.Now()
	touch := false

	c.mu.Lock()
	b := c.blocks[key]
	if b != nil {
		b.refs++
		b.lastUsed = now
		if now.Sub(b.touched) >= archiveCacheTouchInterval {
			b.touched = now
			touch = true
		}
	}
	c.mu.Unlock()

	if b == nil {
		return "", false
	}

	dir := c.blockDir(key)
	if touch {
		_ = os.Chtimes(dir, now, now)
	}
	return dir, true
}

func (c *archiveDiskCache) release(key string) {
	c.mu.Lock()
	if b := c.blocks[key]; b != nil {
		b.refs--
	}
	evicted := c.evictLocked(0)
	c.mu.Unlock()
	c.remove(evicted)
}

// forget removes a cached block that is no longer usable.
func (c *archiveDiskCache) forget(key string) {
	c.mu.Lock()
	b := c.blocks[key]
	if b != nil {
		delete(c.blocks, key)
		c.total -= b.size
	}
	c.mu.Unlock()

	if b != nil {
		c.remove([]string{c.blockDir(key)})
	}
}

// open opens the file of e from its cached block.
func (c *archiveDiskCache) open(key string, e *archiveEntry) (fs.File, bool) {
	dir, ok := c.acquire(key)
	if !ok {
		return nil, false
	}

	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(e.name)))
	if err != nil {
		logger.Warnf("Opening cached archive file: %v", err)
		c.release(key)
		c.forget(key)
		return nil, false
	}

	return &archiveDiskFile{
		File:    f,
		entry:   e,
		release: func() { c.release(key) },
	}, true
}

// extract calls fill to populate the block's directory, unless the block is
// already cached. Concurrent calls for the same block extract it once.
func (c *archiveDiskCache) extract(key string, size int64, fill func(dir string) error) error {
	_, err, _ := c.group.Do(key, func() (any, error) {
		c.mu.Lock()
		if c.blocks[key] != nil {
			c.mu.Unlock()
			return nil, nil
		}
		evicted := c.evictLocked(size)
		c.mu.Unlock()
		c.remove(evicted)

		dir := c.blockDir(key)
		if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
			return nil, err
		}

		tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+archiveCacheTmpSuffix)
		if err != nil {
			return nil, err
		}

		if err := fill(tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}

		// remove anything left from a previous run that was not loaded
		_ = os.RemoveAll(dir)
		if err := os.Rename(tmp, dir); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}

		now := time.Now()
		b := &diskBlock{
			size:     size,
			lastUsed: now,
			touched:  now,
			// don't evict the new block to make room for itself
			refs: 1,
		}
		c.mu.Lock()
		c.blocks[key] = b
		c.total += size
		evicted = c.evictLocked(0)
		b.refs--
		c.mu.Unlock()
		c.remove(evicted)

		return nil, nil
	})

	return err
}

// archiveDiskFile is a file in an archiveFS read from the disk cache.
type archiveDiskFile struct {
	*os.File
	entry   *archiveEntry
	once    sync.Once
	release func()
}

func (f *archiveDiskFile) Stat() (fs.FileInfo, error) { return f.entry, nil }

func (f *archiveDiskFile) Close() error {
	err := f.File.Close()
	f.once.Do(f.release)
	return err
}
