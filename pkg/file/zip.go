package file

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
	"github.com/xWTF/chardet"

	"golang.org/x/net/html/charset"
	"golang.org/x/text/transform"
)

var (
	ErrNotReaderAt  = errors.New("invalid reader: does not implement io.ReaderAt")
	errZipFSOpenZip = errors.New("cannot open zip file inside zip file")

	// ErrUnsupportedArchive is returned when a gallery archive cannot be read.
	// ErrEncryptedArchive wraps it.
	ErrUnsupportedArchive = errors.New("unsupported archive")
	ErrEncryptedArchive   = fmt.Errorf("%w: encrypted archives are not supported", ErrUnsupportedArchive)
)

type archiveFormat int

const (
	archiveZip archiveFormat = iota
	archive7z
	archiveRar
	archiveISO
)

var archiveSignatures = []struct {
	format archiveFormat
	magic  []byte
}{
	{archive7z, []byte("7z\xbc\xaf\x27\x1c")},
	{archiveRar, []byte("Rar!\x1a\x07\x00")},     // RAR 1.5-4.x
	{archiveRar, []byte("Rar!\x1a\x07\x01\x00")}, // RAR 5.0
}

// detectArchiveFormat determines the archive format from its leading bytes,
// or for ISO images from their first volume descriptor, regardless of the file
// extension. Zip is the default, since zip files may have data prepended to
// them (e.g. self-extracting archives).
func detectArchiveFormat(r io.ReaderAt) archiveFormat {
	header := make([]byte, 8)
	n, _ := r.ReadAt(header, 0)
	header = header[:n]

	for _, s := range archiveSignatures {
		if bytes.HasPrefix(header, s.magic) {
			return s.format
		}
	}

	if !bytes.HasPrefix(header, []byte("PK")) && isISOImage(r) {
		return archiveISO
	}

	return archiveZip
}

// ZipFS is a file system backed by an archive file.
// Despite the name, it supports zip, 7z and rar archives and ISO images.
type zipFS struct {
	fsys          fs.FS
	zipFileCloser io.Closer
	zipPath       string
}

func newZipFS(f models.FS, path string, size int64) (*zipFS, error) {
	reader, err := f.Open(path)
	if err != nil {
		return nil, err
	}

	asReaderAt, _ := reader.(io.ReaderAt)
	if asReaderAt == nil {
		reader.Close()
		return nil, ErrNotReaderAt
	}

	var fsys fs.FS
	var closer io.Closer = reader

	switch detectArchiveFormat(asReaderAt) {
	case archive7z:
		fsys, err = open7zArchive(asReaderAt, size, path, archiveCacheKey(f, path, size))
	case archiveRar:
		// rardecode opens the archive (and any further volumes) by name itself
		reader.Close()
		closer = io.NopCloser(nil)
		fsys, err = openRarArchive(f, path, archiveCacheKey(f, path, size))
	case archiveISO:
		fsys, err = openISOArchive(asReaderAt, size, path)
	default:
		fsys, err = openZipArchive(asReaderAt, size, path)
	}

	if err != nil {
		closer.Close()
		return nil, err
	}

	return &zipFS{
		fsys:          fsys,
		zipFileCloser: closer,
		zipPath:       path,
	}, nil
}

func openZipArchive(r io.ReaderAt, size int64, path string) (fs.FS, error) {
	zipReader, err := zip.NewReader(r, size)
	if err != nil {
		if errors.Is(err, zip.ErrFormat) {
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedArchive, err)
		}
		return nil, err
	}

	// Concat all Name and Comment for better detection result
	var buffer bytes.Buffer
	for _, f := range zipReader.File {
		buffer.WriteString(f.Name)
		buffer.WriteString(f.Comment)
	}
	buffer.WriteString(zipReader.Comment)

	// Detect encoding
	d, err := chardet.NewTextDetector().DetectBest(buffer.Bytes())
	if err != nil {
		// If we can't detect the encoding, just assume it's UTF8
		logger.Warnf("Unable to detect decoding for %s: %w", path, err)
	}

	// If the charset is not UTF8, decode'em
	if d != nil && d.Charset != "UTF-8" {
		logger.Debugf("Detected non-utf8 zip charset %s (%s): %s", d.Charset, d.Language, path)

		e, _ := charset.Lookup(d.Charset)
		if e == nil {
			// if we can't find the encoding, just assume it's UTF8
			logger.Warnf("Failed to lookup charset %s, language %s", d.Charset, d.Language)
		} else {
			decoder := e.NewDecoder()
			for _, f := range zipReader.File {
				newName, _, err := transform.String(decoder, f.Name)
				if err != nil {
					logger.Warnf("Failed to decode %v: %v", []byte(f.Name), err)
				} else {
					f.Name = newName
				}
				// Comments are not decoded cuz stash doesn't use that
			}
		}
	}

	return zipReader, nil
}

func open7zArchive(r io.ReaderAt, size int64, path string, cacheKey string) (fs.FS, error) {
	reader, err := sevenzip.NewReader(r, size)
	if err != nil {
		var readErr *sevenzip.ReadError
		if errors.As(err, &readErr) && readErr.Encrypted {
			return nil, fmt.Errorf("%w: %v", ErrEncryptedArchive, err)
		}
		return nil, err
	}

	hasData := func(f *sevenzip.File) bool {
		return !f.FileInfo().IsDir() && f.UncompressedSize > 0
	}

	// files sharing a compressed stream are stored solid
	filesPerStream := make(map[int]int)
	for _, f := range reader.File {
		if hasData(f) {
			filesPerStream[f.Stream]++
		}
	}

	index := newArchiveIndex()
	blocks := make(map[int]*solidBlock)
	blockFiles := make(map[*solidBlock][]*sevenzip.File)
	solid := &solidReader{archivePath: path, cacheKey: cacheKey}

	for _, f := range reader.File {
		info := f.FileInfo()
		e := &archiveEntry{
			size:    int64(f.UncompressedSize), //nolint:gosec
			modTime: f.Modified,
			mode:    info.Mode(),
			isDir:   info.IsDir(),
		}

		if hasData(f) && filesPerStream[f.Stream] > 1 {
			b := blocks[f.Stream]
			if b == nil {
				b = &solidBlock{id: f.Stream}
				blocks[f.Stream] = b
				solid.blocks = append(solid.blocks, b)
			}
			e.block = b
			e.index = len(b.entries)
			b.entries = append(b.entries, e)
			b.size += e.size
			blockFiles[b] = append(blockFiles[b], f)
		} else {
			e.open = f.Open
		}

		addArchiveEntry(index, e, f.Name, path)
	}

	ret := &archiveFS{index: index}
	if len(solid.blocks) > 0 {
		solid.newCursor = func(b *solidBlock) (blockCursor, error) {
			return &sevenZipCursor{files: blockFiles[b]}, nil
		}
		ret.solid = solid
	}

	return ret, nil
}

// addArchiveEntry adds e to index under the name from the archive header.
// Files with invalid names, such as those that would escape the archive, are
// skipped. If they are in a solid block they are still decoded, but never
// written to disk.
func addArchiveEntry(index *archiveIndex, e *archiveEntry, name string, archivePath string) {
	clean, ok := cleanArchivePath(name)
	if ok {
		e.name = clean
		ok = index.add(e)
	}

	if !ok {
		e.name = ""
		if name != "" && name != "/" {
			logger.Warnf("Skipping %q in %q: invalid path", name, archivePath)
		}
	}
}

// rarFS adapts models.FS to the fs.FS used by rardecode to open archive volumes.
type rarFS struct {
	fs models.FS
}

func (f rarFS) Open(name string) (fs.File, error) {
	return f.fs.Open(name)
}

func openRarArchive(f models.FS, path string, cacheKey string) (fs.FS, error) {
	files, err := rardecode.List(path, rardecode.FileSystem(rarFS{f}))
	if err != nil {
		if errors.Is(err, rardecode.ErrArchiveEncrypted) {
			return nil, fmt.Errorf("%w: %v", ErrEncryptedArchive, err)
		}
		return nil, err
	}

	var block *solidBlock
	for _, rf := range files {
		if rf.Encrypted {
			return nil, ErrEncryptedArchive
		}
		if rf.Solid && block == nil {
			// a solid rar archive can only be decoded from its start
			block = &solidBlock{}
		}
	}

	index := newArchiveIndex()
	for _, rf := range files {
		e := &archiveEntry{
			size:    rf.UnPackedSize,
			modTime: rf.ModificationTime,
			mode:    rf.Mode(),
			isDir:   rf.IsDir,
		}

		if !e.isDir {
			if block != nil {
				e.block = block
				e.index = len(block.entries)
				block.entries = append(block.entries, e)
				block.size += e.size
			} else {
				e.open = rf.Open
			}
		}

		addArchiveEntry(index, e, rf.Name, path)
	}

	ret := &archiveFS{index: index}
	if block != nil {
		ret.solid = &solidReader{
			archivePath: path,
			cacheKey:    cacheKey,
			blocks:      []*solidBlock{block},
			newCursor: func(*solidBlock) (blockCursor, error) {
				return newRarCursor(f, path)
			},
		}
	}

	return ret, nil
}

// archiveCacheKey identifies the contents of the archive at path, for the
// disk cache of extracted solid blocks.
func archiveCacheKey(f models.FS, path string, size int64) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s\x00%d", path, size)
	if info, err := f.Stat(path); err == nil {
		fmt.Fprintf(h, "\x00%d", info.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (f *zipFS) rel(name string) (string, error) {
	if f.zipPath == name {
		return ".", nil
	}

	relName, err := filepath.Rel(f.zipPath, name)
	if err != nil {
		// if the path is not relative to the zip path, then it's not found in the zip file,
		// so treat this as a file not found
		return "", fs.ErrNotExist
	}

	// convert relName to use slash, since zip files do so regardless
	// of os
	relName = filepath.ToSlash(relName)

	return relName, nil
}

func (f *zipFS) Stat(name string) (fs.FileInfo, error) {
	relName, err := f.rel(name)
	if err != nil {
		return nil, err
	}

	// 7z and rar archives implement fs.StatFS, so that files in solid blocks
	// are not decoded just to stat them
	return fs.Stat(f.fsys, relName)
}

func (f *zipFS) Lstat(name string) (fs.FileInfo, error) {
	return f.Stat(name)
}

func (f *zipFS) OpenZip(name string, size int64) (models.ZipFS, error) {
	return nil, errZipFSOpenZip
}

func (f *zipFS) IsPathCaseSensitive(path string) (bool, error) {
	return true, nil
}

type zipReadDirFile struct {
	fs.File
}

func (f *zipReadDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	asReadDirFile, _ := f.File.(fs.ReadDirFile)
	if asReadDirFile == nil {
		return nil, fmt.Errorf("internal error: not a ReadDirFile")
	}

	return asReadDirFile.ReadDir(n)
}

func (f *zipFS) Open(name string) (fs.ReadDirFile, error) {
	relName, err := f.rel(name)
	if err != nil {
		return nil, err
	}

	r, err := f.fsys.Open(relName)
	if err != nil {
		return nil, err
	}

	// return files that implement ReadDirFile as they are, so that any
	// io.Seeker implementation remains visible
	if rdf, ok := r.(fs.ReadDirFile); ok {
		return rdf, nil
	}

	return &zipReadDirFile{
		File: r,
	}, nil
}

func (f *zipFS) Close() error {
	if c, ok := f.fsys.(io.Closer); ok {
		_ = c.Close()
	}
	return f.zipFileCloser.Close()
}

// openOnly returns a ReadCloser where calling Close will close the zip fs as well.
func (f *zipFS) OpenOnly(name string) (io.ReadCloser, error) {
	r, err := f.Open(name)
	if err != nil {
		return nil, err
	}

	return &wrappedReadCloser{
		ReadCloser: r,
		outer:      f,
	}, nil
}

type wrappedReadCloser struct {
	io.ReadCloser
	outer io.Closer
}

func (f *wrappedReadCloser) Close() error {
	_ = f.ReadCloser.Close()
	return f.outer.Close()
}
