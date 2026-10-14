package file

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/stashapp/stash/pkg/logger"
	"github.com/xWTF/chardet"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/transform"
)

// ISO 9660 disc images are read directly: files in an image are stored
// uncompressed in one piece, so they are served from the image itself.
//
// Names are taken from the Rock Ridge extension if present, then from Joliet,
// then from the plain ISO 9660 names. UDF is not read, but images with UDF
// usually also have an ISO 9660 file system (a "UDF bridge"), which is used
// instead.
//
// See ECMA-119, the Joliet specification, SUSP (IEEE P1281) and Rock Ridge
// (IEEE P1282).

const (
	isoSectorSize = 2048
	// volume descriptors start after the 16 sector system area
	isoDescriptorStart = 16 * isoSectorSize

	// limits against corrupt or malicious images
	isoMaxDescriptors   = 64
	isoMaxDirSize       = 64 << 20
	isoMaxDepth         = 64
	isoMaxEntries       = 1 << 20
	isoMaxContinuations = 32

	isoFlagDirectory   = 0x02
	isoFlagAssociated  = 0x04
	isoFlagMultiExtent = 0x80
)

var (
	isoMagic = []byte("CD001")
	// udfMagic starts the volume recognition sequence of an image with only UDF
	udfMagic = []byte("BEA01")
)

// isISOImage reports whether r holds an ISO 9660 or UDF image, from the
// identifier of its first volume descriptor.
func isISOImage(r io.ReaderAt) bool {
	id := make([]byte, 5)
	if _, err := r.ReadAt(id, isoDescriptorStart+1); err != nil {
		return false
	}
	return bytes.Equal(id, isoMagic) || bytes.Equal(id, udfMagic)
}

type isoRecord struct {
	offset  int64 // of the data in the image
	size    int64
	modTime time.Time
	flags   byte
	ident   []byte
	susp    []byte // system use area
	// interleaved files are not stored in one piece
	interleaved bool
}

func (r *isoRecord) isDir() bool { return r.flags&isoFlagDirectory != 0 }

// isSelfOrParent reports whether r is the "." or ".." record of a directory.
func (r *isoRecord) isSelfOrParent() bool {
	return len(r.ident) == 1 && r.ident[0] <= 1
}

func parseISORecord(b []byte, blockSize int64) (isoRecord, bool) {
	n := int(b[0])
	if n < 34 || n > len(b) {
		return isoRecord{}, false
	}
	identLen := int(b[32])
	if 33+identLen > n {
		return isoRecord{}, false
	}

	extAttrLen := int64(b[1])
	rec := isoRecord{
		offset:      (int64(binary.LittleEndian.Uint32(b[2:])) + extAttrLen) * blockSize,
		size:        int64(binary.LittleEndian.Uint32(b[10:])),
		modTime:     isoRecordTime(b[18:25]),
		flags:       b[25],
		ident:       b[33 : 33+identLen],
		interleaved: b[26] != 0 || b[27] != 0,
	}

	// the identifier is padded to an even length
	sys := 33 + identLen + (identLen+1)%2
	if sys < n {
		rec.susp = b[sys:n]
	}
	return rec, true
}

func isoRecordTime(b []byte) time.Time {
	if b[1] == 0 || b[2] == 0 {
		// not specified
		return time.Time{}
	}
	// offset from GMT in 15 minute intervals
	offset := int(int8(b[6]))
	if offset < -48 || offset > 52 {
		offset = 0
	}
	loc := time.FixedZone("", offset*15*60)
	return time.Date(1900+int(b[0]), time.Month(b[1]), int(b[2]), int(b[3]), int(b[4]), int(b[5]), 0, loc)
}

type isoVolume struct {
	blockSize int64
	root      isoRecord
}

func parseISOVolume(d []byte) (*isoVolume, error) {
	blockSize := int64(binary.LittleEndian.Uint16(d[128:]))
	switch blockSize {
	case 512, 1024, 2048:
	default:
		return nil, fmt.Errorf("invalid logical block size %d", blockSize)
	}

	root, ok := parseISORecord(d[156:190], blockSize)
	if !ok || !root.isDir() {
		return nil, errors.New("invalid root directory record")
	}
	return &isoVolume{blockSize: blockSize, root: root}, nil
}

// readISOVolumes returns the primary volume of the image, and its Joliet
// volume if it has one.
func readISOVolumes(r io.ReaderAt) (primary, joliet *isoVolume, err error) {
	d := make([]byte, isoSectorSize)
	for i := 0; i < isoMaxDescriptors; i++ {
		if _, err := r.ReadAt(d, isoDescriptorStart+int64(i)*isoSectorSize); err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(d[1:6], isoMagic) {
			// UDF descriptors may follow
			break
		}

		switch d[0] {
		case 1:
			if primary == nil {
				if primary, err = parseISOVolume(d); err != nil {
					return nil, nil, err
				}
			}
		case 2:
			// version 2 is the enhanced volume descriptor of ISO 9660:1999
			escapes := d[88:120]
			isJoliet := d[6] == 1 && (bytes.Contains(escapes, []byte("%/@")) ||
				bytes.Contains(escapes, []byte("%/C")) ||
				bytes.Contains(escapes, []byte("%/E")))
			if isJoliet && joliet == nil {
				// an unreadable Joliet volume is not fatal
				joliet, _ = parseISOVolume(d)
			}
		case 255:
			return primary, joliet, nil
		}
	}

	return primary, joliet, nil
}

type isoReader struct {
	r         io.ReaderAt
	size      int64
	blockSize int64
	path      string // of the image, for logging

	joliet     bool
	rockRidge  bool
	suspSkip   int
	entryCount int
}

// isoNode is a file or directory read from the image, before its name is decoded.
type isoNode struct {
	parent *isoNode
	name   []byte
	rec    isoRecord
	path   string
	// skipped is set for nodes with invalid names, and their children
	skipped bool
}

func (x *isoReader) readDir(dir isoRecord) ([]isoRecord, error) {
	if dir.size > isoMaxDirSize || dir.offset+dir.size > x.size {
		return nil, fmt.Errorf("invalid directory size %d", dir.size)
	}
	data := make([]byte, dir.size)
	if _, err := x.r.ReadAt(data, dir.offset); err != nil {
		return nil, err
	}

	var ret []isoRecord
	for off := 0; off < len(data); {
		if data[off] == 0 {
			// records do not cross sector boundaries, so the rest of the
			// sector is padding
			abs := dir.offset + int64(off)
			off += int(isoSectorSize - abs%isoSectorSize)
			continue
		}
		rec, ok := parseISORecord(data[off:], x.blockSize)
		if !ok {
			return nil, errors.New("invalid directory record")
		}
		ret = append(ret, rec)
		off += int(data[off])
	}
	return ret, nil
}

// suspEntries calls fn for each System Use Sharing Protocol entry of rec,
// following continuation areas.
func (x *isoReader) suspEntries(rec isoRecord, skip int, fn func(sig string, data []byte)) {
	area := rec.susp
	if skip > len(area) {
		return
	}
	area = area[skip:]

	for i := 0; i < isoMaxContinuations; i++ {
		var next []byte
		for len(area) >= 4 {
			n := int(area[2])
			if n < 4 || n > len(area) {
				break
			}
			sig, data := string(area[:2]), area[:n]
			area = area[n:]

			switch sig {
			case "ST":
				area = nil
			case "CE":
				if len(data) >= 28 {
					block := int64(binary.LittleEndian.Uint32(data[4:]))
					offset := int64(binary.LittleEndian.Uint32(data[12:]))
					length := int64(binary.LittleEndian.Uint32(data[20:]))
					if length <= isoSectorSize {
						next = make([]byte, length)
						if _, err := x.r.ReadAt(next, block*x.blockSize+offset); err != nil {
							next = nil
						}
					}
				}
			default:
				fn(sig, data)
			}
		}

		if next == nil {
			return
		}
		area = next
	}
}

// detectRockRidge checks for the SUSP indicator at the start of the system use
// area of the root directory's "." record.
func (x *isoReader) detectRockRidge(root []isoRecord) {
	if len(root) == 0 || !root[0].isSelfOrParent() {
		return
	}
	s := root[0].susp
	if len(s) >= 7 && s[0] == 'S' && s[1] == 'P' && s[4] == 0xbe && s[5] == 0xef {
		x.rockRidge = true
		x.suspSkip = int(s[6])
	}
}

// rockRidgeInfo returns the Rock Ridge name of rec (nil if none), whether rec
// is a relocated directory, which is listed where its child link is instead,
// and whether rec is compressed with zisofs, which is not supported.
// If rec is a child link, the record of the directory it links to replaces it.
func (x *isoReader) rockRidgeInfo(rec *isoRecord) (name []byte, relocated bool, compressed bool) {
	var link int64 = -1
	x.suspEntries(*rec, x.suspSkip, func(sig string, data []byte) {
		switch sig {
		case "NM":
			// flags: 1 continues in the next entry, 2 and 4 are "." and ".."
			if len(data) >= 5 && data[4]&0x06 == 0 {
				name = append(name, data[5:]...)
			}
		case "RE":
			relocated = true
		case "ZF":
			compressed = true
		case "CL":
			if len(data) >= 8 {
				link = int64(binary.LittleEndian.Uint32(data[4:])) * x.blockSize
			}
		}
	})

	if link >= 0 {
		// the size of the linked directory is in its "." record
		d := make([]byte, isoSectorSize)
		if _, err := x.r.ReadAt(d, link); err == nil {
			if self, ok := parseISORecord(d, x.blockSize); ok && self.isDir() {
				rec.offset = link
				rec.size = self.size
				rec.flags |= isoFlagDirectory
			}
		}
	}
	return name, relocated, compressed
}

// recordName returns the name of rec as stored in the image.
func (x *isoReader) recordName(rec isoRecord) []byte {
	name := rec.ident
	if x.joliet {
		u := make([]uint16, len(name)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(name[2*i:])
		}
		name = []byte(string(utf16.Decode(u)))
	}

	// remove the version number of files
	if !rec.isDir() {
		if i := bytes.LastIndexByte(name, ';'); i >= 0 {
			name = name[:i]
		}
		if !x.joliet {
			// files without an extension end with a dot
			name = bytes.TrimSuffix(name, []byte("."))
		}
	}
	return name
}

// walk reads the directory tree below dir, appending its nodes to nodes in
// depth-first order. visited holds the offsets of the directories being read,
// to stop at loops.
func (x *isoReader) walk(dir *isoNode, records []isoRecord, depth int, visited map[int64]bool, nodes []*isoNode) ([]*isoNode, error) {
	if depth > isoMaxDepth {
		return nodes, errors.New("directories nested too deeply")
	}

	var pending *isoRecord // multi-extent file
	for _, rec := range records {
		if rec.isSelfOrParent() || rec.flags&isoFlagAssociated != 0 {
			continue
		}

		// a file over 4GiB is recorded in several extents, each with its own
		// record
		if pending != nil && !bytes.Equal(rec.ident, pending.ident) {
			logger.Warnf("Skipping %q in %q: incomplete multi-extent file", pending.ident, x.path)
			pending = nil
		}
		if pending != nil {
			if rec.offset != pending.offset+pending.size {
				pending.interleaved = true
			}
			pending.size += rec.size
			if rec.flags&isoFlagMultiExtent != 0 {
				continue
			}
			rec.offset, rec.size, rec.interleaved = pending.offset, pending.size, pending.interleaved
			rec.susp = pending.susp
			pending = nil
		} else if rec.flags&isoFlagMultiExtent != 0 && !rec.isDir() {
			p := rec
			pending = &p
			continue
		}

		var name []byte
		if x.rockRidge {
			var relocated, compressed bool
			name, relocated, compressed = x.rockRidgeInfo(&rec)
			if relocated {
				continue
			}
			if compressed && !rec.isDir() {
				logger.Warnf("Skipping %q in %q: zisofs compression is not supported", name, x.path)
				continue
			}
		}
		if name == nil {
			name = x.recordName(rec)
		}

		x.entryCount++
		if x.entryCount > isoMaxEntries {
			return nodes, errors.New("too many entries")
		}

		node := &isoNode{parent: dir, name: name, rec: rec}
		nodes = append(nodes, node)

		if rec.isDir() {
			if visited[rec.offset] {
				logger.Warnf("Skipping %q in %q: directory loop", name, x.path)
				continue
			}
			children, err := x.readDir(rec)
			if err != nil {
				logger.Warnf("Skipping the contents of %q in %q: %v", name, x.path, err)
				continue
			}
			visited[rec.offset] = true
			nodes, err = x.walk(node, children, depth+1, visited, nodes)
			delete(visited, rec.offset)
			if err != nil {
				return nodes, err
			}
		}
	}

	if pending != nil {
		logger.Warnf("Skipping %q in %q: incomplete multi-extent file", pending.ident, x.path)
	}

	return nodes, nil
}

// decodeISONames converts the names of nodes to UTF-8. Joliet names are
// always UTF-16, but other names may be in a local encoding such as Shift_JIS,
// which is detected as for zip files.
func decodeISONames(nodes []*isoNode, imagePath string) {
	var buffer bytes.Buffer
	valid := true
	for _, n := range nodes {
		valid = valid && utf8.Valid(n.name)
		buffer.Write(n.name)
	}
	if valid {
		return
	}

	d, err := chardet.NewTextDetector().DetectBest(buffer.Bytes())
	if err != nil || d == nil || d.Charset == "UTF-8" {
		return
	}
	e, _ := charset.Lookup(d.Charset)
	if e == nil {
		logger.Warnf("Failed to lookup charset %s, language %s", d.Charset, d.Language)
		return
	}

	logger.Debugf("Detected non-utf8 ISO charset %s (%s): %s", d.Charset, d.Language, imagePath)
	decoder := e.NewDecoder()
	for _, n := range nodes {
		if utf8.Valid(n.name) {
			continue
		}
		if decoded, _, err := transform.Bytes(decoder, n.name); err == nil {
			n.name = decoded
		}
	}
}

func openISOArchive(r io.ReaderAt, size int64, path string) (fs.FS, error) {
	unsupported := func(err error) error {
		return fmt.Errorf("%w: ISO image: %v", ErrUnsupportedArchive, err)
	}

	primary, joliet, err := readISOVolumes(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, unsupported(err)
		}
		return nil, err
	}
	if primary == nil {
		return nil, unsupported(errors.New("UDF images without an ISO 9660 file system are not supported"))
	}

	vol := primary
	x := &isoReader{r: r, size: size, blockSize: vol.blockSize, path: path}
	root, err := x.readDir(vol.root)
	if err != nil {
		return nil, unsupported(err)
	}

	// Rock Ridge names are preferred to Joliet's, which are limited to 64
	// characters
	x.detectRockRidge(root)
	if !x.rockRidge && joliet != nil {
		jx := &isoReader{r: r, size: size, blockSize: joliet.blockSize, path: path, joliet: true}
		if jroot, err := jx.readDir(joliet.root); err == nil {
			vol, x, root = joliet, jx, jroot
		} else {
			logger.Warnf("Ignoring Joliet names of %q: %v", path, err)
		}
	}

	top := &isoNode{}
	nodes, err := x.walk(top, root, 0, map[int64]bool{vol.root.offset: true}, nil)
	if err != nil {
		return nil, unsupported(err)
	}
	if !x.joliet {
		decodeISONames(nodes, path)
	}

	index := newArchiveIndex()
	for _, n := range nodes {
		name := string(n.name)
		if n.parent.skipped {
			n.skipped = true
			continue
		}
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			logger.Warnf("Skipping %q in %q: invalid name", name, path)
			n.skipped = true
			continue
		}
		if n.parent.path != "" {
			name = n.parent.path + "/" + name
		}
		n.path = name

		e := &archiveEntry{
			size:    n.rec.size,
			modTime: n.rec.modTime,
			mode:    0444,
			isDir:   n.rec.isDir(),
		}
		if e.isDir {
			e.size = 0
			e.mode = 0555
		} else {
			if n.rec.size == 0 {
				// the extent of an empty file is meaningless
				n.rec.offset = 0
			}
			if n.rec.interleaved || n.rec.offset+n.rec.size > size {
				logger.Warnf("Skipping %q in %q: not stored in one piece within the image", name, path)
				continue
			}
			e.data = r
			e.offset = n.rec.offset
		}

		addArchiveEntry(index, e, name, path)
	}

	return &archiveFS{index: index}, nil
}
