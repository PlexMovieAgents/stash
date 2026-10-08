package file

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
)

const (
	// solidMemoryLimit is the largest file that is kept in memory after being
	// decoded from a solid block, so that opening it again (to fingerprint it,
	// then read its dimensions, then make a thumbnail) does not decode it again.
	solidMemoryLimit = 32 << 20

	// solidCursorMaxGap is the most files the shared cursor will decode and
	// discard to reach a requested file when the block could be extracted to
	// the disk cache instead.
	solidCursorMaxGap = 16
)

var errSolidBlockEnded = errors.New("solid block ended early")

// blockCursor decodes the files of a solid block in order.
type blockCursor interface {
	// next returns the contents of the next file in the block. The reader is
	// valid until the following call to next or close.
	next() (io.Reader, error)
	close() error
}

// solidBlock is a run of files that are compressed together, so that a file
// can only be decoded after the files before it.
type solidBlock struct {
	id int
	// entries are the files in the order they are decoded. Entries with an
	// invalid name have an empty name and are not in the archive index.
	entries []*archiveEntry
	size    int64

	// mu guards the shared cursor, used to read the block sequentially
	mu     sync.Mutex
	cursor blockCursor
	pos    int // index of the file the cursor returns next
}

// solidReader opens files stored in solid blocks.
//
// Files read in order (as the scanner does) are decoded with a cursor shared
// between calls, without writing to disk. Files read out of order (as when
// browsing a gallery) cause the whole block to be extracted to the disk cache,
// if it is enabled and large enough. Otherwise the block is decoded from its
// start for each file.
type solidReader struct {
	archivePath string
	// cacheKey identifies the archive contents in the disk cache
	cacheKey  string
	blocks    []*solidBlock
	newCursor func(b *solidBlock) (blockCursor, error)

	// the most recently decoded file
	lastMu   sync.Mutex
	last     *archiveEntry
	lastData []byte

	// decoded counts the files decoded, for testing
	decoded atomic.Int64
}

func (s *solidReader) blockKey(b *solidBlock) string {
	return s.cacheKey + "/b" + strconv.Itoa(b.id)
}

func (s *solidReader) recent(e *archiveEntry) ([]byte, bool) {
	s.lastMu.Lock()
	defer s.lastMu.Unlock()

	if s.last == e {
		return s.lastData, true
	}
	return nil, false
}

func (s *solidReader) setRecent(e *archiveEntry, data []byte) {
	s.lastMu.Lock()
	defer s.lastMu.Unlock()

	s.last = e
	s.lastData = data
}

func (s *solidReader) memFile(e *archiveEntry, data []byte) fs.File {
	return &archiveMemFile{Reader: bytes.NewReader(data), entry: e}
}

func (s *solidReader) open(e *archiveEntry) (fs.File, error) {
	if data, ok := s.recent(e); ok {
		return s.memFile(e, data), nil
	}

	b := e.block
	disk := getArchiveDiskCache()
	useDisk := disk != nil && disk.fits(b.size)

	if useDisk {
		if f, ok := disk.open(s.blockKey(b), e); ok {
			return f, nil
		}
	}

	if e.size <= solidMemoryLimit && b.mu.TryLock() {
		data, ok, err := s.readWithCursor(b, e, useDisk)
		b.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if ok {
			return s.memFile(e, data), nil
		}
	}

	if useDisk {
		key := s.blockKey(b)
		err := disk.extract(key, b.size, func(dir string) error {
			return s.extractBlock(b, dir)
		})
		if err == nil {
			if f, ok := disk.open(key, e); ok {
				return f, nil
			}
		} else {
			logger.Warnf("Extracting solid block of %q to cache: %v", s.archivePath, err)
		}
	}

	return s.decodeFromStart(b, e)
}

// readWithCursor reads e using the shared cursor of its block, if that is
// cheaper than the alternatives. b.mu must be held.
func (s *solidReader) readWithCursor(b *solidBlock, e *archiveEntry, useDisk bool) ([]byte, bool, error) {
	gap := e.index - b.pos
	if gap < 0 {
		if useDisk {
			// extracting the block is no more work than rewinding, and
			// makes later out-of-order reads cheap
			return nil, false, nil
		}
		s.resetCursor(b)
		gap = e.index
	}
	if useDisk && gap > solidCursorMaxGap {
		return nil, false, nil
	}

	data, err := s.advance(b, e)
	if err != nil {
		s.resetCursor(b)
		return nil, false, err
	}

	if b.pos == len(b.entries) {
		// release the decoder at the end of the block
		s.resetCursor(b)
	}

	s.setRecent(e, data)
	return data, true, nil
}

func (s *solidReader) resetCursor(b *solidBlock) {
	if b.cursor != nil {
		_ = b.cursor.close()
	}
	b.cursor = nil
	b.pos = 0
}

// advance decodes the files of b up to and including e. b.mu must be held.
func (s *solidReader) advance(b *solidBlock, e *archiveEntry) ([]byte, error) {
	if b.cursor == nil {
		c, err := s.newCursor(b)
		if err != nil {
			return nil, err
		}
		b.cursor = c
		b.pos = 0
	}

	for b.pos < e.index {
		if err := s.skip(b.cursor); err != nil {
			return nil, err
		}
		b.pos++
	}

	r, err := b.cursor.next()
	if err != nil {
		return nil, err
	}
	b.pos++
	s.decoded.Add(1)

	return readLimited(r)
}

func (s *solidReader) skip(c blockCursor) error {
	r, err := c.next()
	if err != nil {
		return err
	}
	s.decoded.Add(1)
	_, err = io.Copy(io.Discard, r)
	return err
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, solidMemoryLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > solidMemoryLimit {
		return nil, fmt.Errorf("file is larger than its header states")
	}
	return data, nil
}

// decodeFromStart decodes b from its start up to e, using a new cursor.
func (s *solidReader) decodeFromStart(b *solidBlock, e *archiveEntry) (fs.File, error) {
	c, err := s.newCursor(b)
	if err != nil {
		return nil, err
	}

	for i := 0; i < e.index; i++ {
		if err := s.skip(c); err != nil {
			_ = c.close()
			return nil, err
		}
	}

	r, err := c.next()
	if err != nil {
		_ = c.close()
		return nil, err
	}
	s.decoded.Add(1)

	if e.size > solidMemoryLimit {
		return &archiveStreamFile{Reader: r, closer: cursorCloser{c}, entry: e}, nil
	}

	data, err := readLimited(r)
	_ = c.close()
	if err != nil {
		return nil, err
	}

	s.setRecent(e, data)
	return s.memFile(e, data), nil
}

type cursorCloser struct{ c blockCursor }

func (c cursorCloser) Close() error { return c.c.close() }

// extractBlock decodes every file of b into dir.
func (s *solidReader) extractBlock(b *solidBlock, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	c, err := s.newCursor(b)
	if err != nil {
		return err
	}
	defer c.close()

	for _, e := range b.entries {
		r, err := c.next()
		if err != nil {
			return err
		}
		s.decoded.Add(1)

		if e.name == "" {
			if _, err := io.Copy(io.Discard, r); err != nil {
				return err
			}
			continue
		}

		if err := writeRootFile(root, e.name, r); err != nil {
			return fmt.Errorf("writing %q: %w", e.name, err)
		}
	}

	return nil
}

// writeRootFile writes r to the slash-separated name within root. root
// prevents name from escaping the directory.
func writeRootFile(root *os.Root, name string, r io.Reader) error {
	if d := path.Dir(name); d != "." {
		if err := root.MkdirAll(filepath.FromSlash(d), 0755); err != nil {
			return err
		}
	}

	f, err := root.Create(filepath.FromSlash(name))
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *solidReader) Close() error {
	for _, b := range s.blocks {
		b.mu.Lock()
		s.resetCursor(b)
		b.mu.Unlock()
	}
	return nil
}

// sevenZipCursor decodes the files of a 7z folder in order. sevenzip keeps
// the decoder of a partly read folder after a file is closed, and reuses it to
// open the next file, so this decodes the folder once.
type sevenZipCursor struct {
	files []*sevenzip.File
	pos   int
	rc    io.ReadCloser
}

func (c *sevenZipCursor) next() (io.Reader, error) {
	if err := c.close(); err != nil {
		return nil, err
	}
	if c.pos >= len(c.files) {
		return nil, errSolidBlockEnded
	}

	rc, err := c.files[c.pos].Open()
	if err != nil {
		return nil, err
	}
	c.pos++
	c.rc = rc
	return rc, nil
}

func (c *sevenZipCursor) close() error {
	if c.rc == nil {
		return nil
	}
	err := c.rc.Close()
	c.rc = nil
	return err
}

// rarCursor decodes the files of a solid rar archive in order.
type rarCursor struct {
	rc *rardecode.ReadCloser
}

func newRarCursor(f models.FS, path string) (*rarCursor, error) {
	rc, err := rardecode.OpenReader(path, rardecode.FileSystem(rarFS{f}))
	if err != nil {
		return nil, err
	}
	return &rarCursor{rc: rc}, nil
}

func (c *rarCursor) next() (io.Reader, error) {
	for {
		h, err := c.rc.Next()
		if errors.Is(err, io.EOF) {
			return nil, errSolidBlockEnded
		}
		if err != nil {
			return nil, err
		}
		// directories are not part of the block
		if !h.IsDir {
			return c.rc, nil
		}
	}
}

func (c *rarCursor) close() error {
	return c.rc.Close()
}
