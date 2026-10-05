package file

import (
	"archive/zip"
	"bytes"
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
	// ErrSolidArchive and ErrEncryptedArchive wrap it.
	ErrUnsupportedArchive = errors.New("unsupported archive")
	ErrSolidArchive       = fmt.Errorf("%w: solid archives are not supported", ErrUnsupportedArchive)
	ErrEncryptedArchive   = fmt.Errorf("%w: encrypted archives are not supported", ErrUnsupportedArchive)
)

type archiveFormat int

const (
	archiveZip archiveFormat = iota
	archive7z
	archiveRar
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
// regardless of the file extension. Zip is the default, since zip files may
// have data prepended to them (e.g. self-extracting archives).
func detectArchiveFormat(r io.ReaderAt) archiveFormat {
	header := make([]byte, 8)
	n, _ := r.ReadAt(header, 0)
	header = header[:n]

	for _, s := range archiveSignatures {
		if bytes.HasPrefix(header, s.magic) {
			return s.format
		}
	}

	return archiveZip
}

// ZipFS is a file system backed by an archive file.
// Despite the name, it supports zip, 7z and rar archives.
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
		fsys, err = open7zArchive(asReaderAt, size)
	case archiveRar:
		// rardecode opens the archive (and any further volumes) by name itself
		reader.Close()
		closer = io.NopCloser(nil)
		fsys, err = openRarArchive(f, path)
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

func open7zArchive(r io.ReaderAt, size int64) (fs.FS, error) {
	reader, err := sevenzip.NewReader(r, size)
	if err != nil {
		var readErr *sevenzip.ReadError
		if errors.As(err, &readErr) && readErr.Encrypted {
			return nil, fmt.Errorf("%w: %v", ErrEncryptedArchive, err)
		}
		return nil, err
	}

	// files sharing a compressed stream are stored solid
	streams := make(map[int]bool)
	for _, f := range reader.File {
		if f.FileInfo().IsDir() || f.UncompressedSize == 0 {
			continue
		}
		if streams[f.Stream] {
			return nil, ErrSolidArchive
		}
		streams[f.Stream] = true
	}

	return reader, nil
}

// rarFS adapts models.FS to the fs.FS used by rardecode to open archive volumes.
type rarFS struct {
	fs models.FS
}

func (f rarFS) Open(name string) (fs.File, error) {
	return f.fs.Open(name)
}

func openRarArchive(f models.FS, path string) (fs.FS, error) {
	opt := rardecode.FileSystem(rarFS{f})

	files, err := rardecode.List(path, opt)
	if err != nil {
		if errors.Is(err, rardecode.ErrArchiveEncrypted) {
			return nil, fmt.Errorf("%w: %v", ErrEncryptedArchive, err)
		}
		return nil, err
	}

	for _, f := range files {
		if f.Encrypted {
			return nil, ErrEncryptedArchive
		}
		if f.Solid {
			return nil, ErrSolidArchive
		}
	}

	return rardecode.OpenFS(path, opt)
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
	reader, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return reader.Stat()
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

	return &zipReadDirFile{
		File: r,
	}, nil
}

func (f *zipFS) Close() error {
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
