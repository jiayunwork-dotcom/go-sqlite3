package zipfile

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/internal/errutil"
)

// https://pkware.cachefly.net/webdocs/casestudies/APPNOTE.TXT
const (
	methodStore   = 0
	methodDeflate = 8

	extTimeExtraID = 0x5455
)

// zipEntry is a single member of a ZIP archive.
//
// Raw (possibly compressed) bytes and uncompressed data are both
// resolved lazily; directories hold neither.
type zipEntry struct {
	name    string
	mode    uint32 // UNIX st_mode
	mtime   int64  // UTC seconds since the UNIX epoch
	method  uint16 // 0: stored, 8: deflate
	crc     uint32 // CRC-32 of the uncompressed data
	usize   uint64 // uncompressed size
	raw     []byte // compressed bytes
	data    []byte // uncompressed bytes
	hasRaw  bool
	hasData bool
	dir     bool
	rowid   int64 // stable identity, independent of the entry's position
}

// parseArchive parses the ZIP image in src.
// An empty image is treated as an empty archive.
func parseArchive(src []byte) ([]*zipEntry, error) {
	if len(src) == 0 {
		return nil, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return nil, err
	}

	entries := make([]*zipEntry, 0, len(zr.File))
	for i, f := range zr.File {
		e := &zipEntry{
			name:   f.Name,
			mode:   entryMode(&f.FileHeader),
			mtime:  f.Modified.UTC().Unix(),
			method: f.Method,
			crc:    f.CRC32,
			usize:  f.UncompressedSize64,
			dir:    f.Mode().IsDir() || strings.HasSuffix(f.Name, "/"),
			rowid:  int64(i + 1),
		}
		if !e.dir {
			off, err := f.DataOffset()
			if err != nil {
				return nil, err
			}
			end := off + int64(f.CompressedSize64)
			if end > int64(len(src)) {
				return nil, zip.ErrFormat
			}
			e.raw = src[off:end]
			e.hasRaw = true
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// getData returns the uncompressed member data.
// Directories and members using an unsupported compression method return nil.
func (e *zipEntry) getData() ([]byte, error) {
	if e.hasData {
		return e.data, nil
	}
	if e.dir || !e.hasRaw {
		return nil, nil
	}

	switch e.method {
	case methodStore:
		e.data = bytes.Clone(e.raw)
	case methodDeflate:
		fr := flate.NewReader(bytes.NewReader(e.raw))
		b, err := io.ReadAll(fr)
		if err2 := fr.Close(); err == nil {
			err = err2
		}
		if err != nil {
			return nil, err
		}
		e.data = b
	default:
		// Unsupported compression method: the data column is NULL.
		return nil, nil
	}
	e.hasData = true
	return e.data, nil
}

// getRaw returns the raw (possibly compressed) member data.
func (e *zipEntry) getRaw() ([]byte, error) {
	if e.hasRaw {
		return e.raw, nil
	}
	if e.dir || !e.hasData {
		return nil, nil
	}

	switch e.method {
	case methodStore:
		e.raw = bytes.Clone(e.data)
	case methodDeflate:
		raw, err := deflateBytes(e.data)
		if err != nil {
			return nil, err
		}
		e.raw = raw
	default:
		return nil, fmt.Errorf("zipfile: unsupported compression method: %d", e.method)
	}
	e.hasRaw = true
	return e.raw, nil
}

// serialize builds a ZIP image from entries.
func serialize(entries []*zipEntry) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for _, e := range entries {
		fh := &zip.FileHeader{
			Name:     e.name,
			Method:   e.method,
			Modified: time.Unix(e.mtime, 0).UTC(),
		}
		fh.SetMode(unixModeToFS(e.mode))
		fh.CRC32 = e.crc
		fh.Extra = appendExtraTime(fh.Extra, uint32(e.mtime))
		fh.ModifiedDate, fh.ModifiedTime = msDosTime(fh.Modified)
		if isUTF8(fh.Name) {
			fh.Flags |= 0x800 // UTF-8 file name
		}

		if e.dir {
			if !strings.HasSuffix(fh.Name, "/") {
				fh.Name += "/"
			}
			fh.Method = methodStore
		} else {
			raw, err := e.getRaw()
			if err != nil {
				return nil, err
			}
			fh.CompressedSize64 = uint64(len(raw))
			fh.UncompressedSize64 = e.usize
		}

		w, err := zw.CreateRaw(fh)
		if err != nil {
			return nil, err
		}
		if !e.dir {
			raw, err := e.getRaw()
			if err != nil {
				return nil, err
			}
			if _, err := w.Write(raw); err != nil {
				return nil, err
			}
		}
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// makeEntry builds a zipEntry from resolved SQL values.
//
// A nil data value marks a directory entry; its name is made to end with "/".
// When hasMethod is false, the compression method is chosen automatically:
// deflate is used only when it produces fewer bytes than storing the data.
func makeEntry(name string, mode uint32, mtime int64, data []byte, method int, hasMethod bool) (*zipEntry, error) {
	if name == "" {
		return nil, errutil.ErrorString("zipfile: name must not be empty")
	}

	isDir := data == nil
	if isDir {
		if !strings.HasSuffix(name, "/") {
			name += "/"
		}
		if typ := mode & 0o170000; typ != 0 && typ != 0o040000 {
			return nil, errutil.ErrorString("zipfile: mode is inconsistent with a directory")
		}
	}

	e := &zipEntry{
		name:  name,
		mode:  mode,
		mtime: mtime,
		dir:   isDir,
	}
	if isDir {
		e.method = methodStore
		e.hasRaw = true
		e.hasData = true
		return e, nil
	}

	e.data = data
	e.hasData = true
	e.usize = uint64(len(data))
	e.crc = crc32.ChecksumIEEE(data)

	switch {
	case !hasMethod:
		comp, err := deflateBytes(data)
		if err != nil {
			return nil, err
		}
		if len(comp) < len(data) {
			e.method, e.raw = methodDeflate, comp
		} else {
			e.method, e.raw = methodStore, bytes.Clone(data)
		}
	case method == methodStore:
		e.method = methodStore
		e.raw = bytes.Clone(data)
	case method == methodDeflate:
		e.method = methodDeflate
		comp, err := deflateBytes(data)
		if err != nil {
			return nil, err
		}
		e.raw = comp
	default:
		return nil, fmt.Errorf("zipfile: unsupported compression method: %d", method)
	}
	e.hasRaw = true
	return e, nil
}

// valueMode resolves the mode column of an INSERT or UPDATE.
func valueMode(v sqlite3.Value, isDir bool) (uint32, error) {
	switch v.Type() {
	case sqlite3.NULL:
		if isDir {
			return 0o040755, nil
		}
		return 0o100644, nil
	case sqlite3.INTEGER:
		return uint32(v.Int64()), nil
	case sqlite3.TEXT:
		s := v.Text()
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return uint32(n), nil
		}
		return parseModeString(s)
	default:
		return 0, errutil.ErrorString("zipfile: invalid mode")
	}
}

// valueMtime resolves the mtime column of an INSERT or UPDATE.
func valueMtime(v sqlite3.Value) int64 {
	if v.Type() == sqlite3.NULL {
		return time.Now().Unix()
	}
	return v.Int64()
}

// valueData resolves the data column of an INSERT or UPDATE.
// A NULL value becomes nil and marks a directory.
func valueData(v sqlite3.Value) []byte {
	if v.Type() == sqlite3.NULL {
		return nil
	}
	if v.Type() == sqlite3.BLOB {
		return bytes.Clone(v.RawBlob())
	}
	return bytes.Clone(v.RawText())
}

// valueMethod resolves the method column of an INSERT or UPDATE.
func valueMethod(v sqlite3.Value) (method int, has bool, err error) {
	switch v.Type() {
	case sqlite3.NULL:
		return 0, false, nil
	case sqlite3.INTEGER:
		method = v.Int()
	default:
		n, err := strconv.Atoi(v.Text())
		if err != nil {
			return 0, false, errutil.ErrorString("zipfile: invalid method")
		}
		method = n
	}
	if method != methodStore && method != methodDeflate {
		return 0, false, fmt.Errorf("zipfile: unsupported compression method: %d", method)
	}
	return method, true, nil
}

// entryMode recovers the UNIX st_mode of an archive member.
func entryMode(h *zip.FileHeader) uint32 {
	if m := h.ExternalAttrs >> 16; m != 0 {
		return m
	}
	m := uint32(h.Mode().Perm())
	switch {
	case h.Mode().IsDir():
		m |= 0o040000
	case h.Mode()&fs.ModeSymlink != 0:
		m |= 0o120000
	default:
		m |= 0o100000
	}
	return m
}

// unixModeToFS converts a UNIX st_mode into an [fs.FileMode].
func unixModeToFS(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & 0o170000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	case 0o001000:
		mode |= fs.ModeNamedPipe
	case 0o002000:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 0o006000:
		mode |= fs.ModeDevice
	case 0o014000:
		mode |= fs.ModeSocket
	}
	if m&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// parseModeString parses permission strings such as "-rw-r--r--".
func parseModeString(s string) (uint32, error) {
	if len(s) != 10 {
		return 0, fmt.Errorf("zipfile: invalid mode: %q", s)
	}

	var mode uint32
	switch s[0] {
	case '-':
		mode = 0o100000
	case 'd':
		mode = 0o040000
	case 'l':
		mode = 0o120000
	default:
		return 0, fmt.Errorf("zipfile: invalid mode: %q", s)
	}

	for g := 0; g < 3; g++ {
		r, w, x := s[1+g*3], s[2+g*3], s[3+g*3]
		bits := uint32(1 << (8 - g*3)) // 0400, 0040, 0004

		switch r {
		case 'r':
			mode |= bits
		case '-':
		default:
			return 0, fmt.Errorf("zipfile: invalid mode: %q", s)
		}
		switch w {
		case 'w':
			mode |= bits >> 1
		case '-':
		default:
			return 0, fmt.Errorf("zipfile: invalid mode: %q", s)
		}

		switch x {
		case 'x':
			mode |= bits >> 2
		case '-':
		case 's', 'S':
			// s/S mark setuid (user) or setgid (group) only.
			if g == 2 {
				return 0, fmt.Errorf("zipfile: invalid mode: %q", s)
			}
			mode |= []uint32{0o4000, 0o2000}[g]
			if x == 's' {
				mode |= bits >> 2
			}
		case 't', 'T':
			// t/T marks the sticky bit on the other class only.
			if g != 2 {
				return 0, fmt.Errorf("zipfile: invalid mode: %q", s)
			}
			mode |= 0o1000
			if x == 't' {
				mode |= bits >> 2
			}
		default:
			return 0, fmt.Errorf("zipfile: invalid mode: %q", s)
		}
	}
	return mode, nil
}

// deflateBytes compresses data with raw deflate.
func deflateBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// appendExtraTime appends an extended timestamp extra field
// so that modification times survive with one-second precision.
func appendExtraTime(extra []byte, mtime uint32) []byte {
	var b [9]byte
	binary.LittleEndian.PutUint16(b[0:2], extTimeExtraID)
	binary.LittleEndian.PutUint16(b[2:4], 5)
	b[4] = 1 // modification time present
	binary.LittleEndian.PutUint32(b[5:9], mtime)
	return append(extra, b[:]...)
}

// msDosTime converts t into MS-DOS date and time fields.
func msDosTime(t time.Time) (date, clock uint16) {
	t = t.UTC()
	if y := t.Year(); y < 1980 {
		t = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)
	} else if y > 2107 {
		t = time.Date(2107, time.December, 31, 23, 59, 58, 0, time.UTC)
	}
	date = uint16(t.Year()-1980)<<9 | uint16(t.Month())<<5 | uint16(t.Day())
	clock = uint16(t.Hour())<<11 | uint16(t.Minute())<<5 | uint16(t.Second()/2)
	return date, clock
}

// isUTF8 reports whether s contains non-ASCII bytes
// while remaining valid UTF-8.
func isUTF8(s string) bool {
	hasNonASCII := false
	for _, r := range s {
		if r > 127 {
			hasNonASCII = true
		}
		if r == 0xFFFD {
			return false
		}
	}
	return hasNonASCII
}

// cloneEntries deep-copies entries so a transaction backup
// stays independent of later in-memory modifications.
func cloneEntries(in []*zipEntry) []*zipEntry {
	out := make([]*zipEntry, len(in))
	for i, e := range in {
		cp := *e
		if e.raw != nil {
			cp.raw = bytes.Clone(e.raw)
		}
		if e.data != nil {
			cp.data = bytes.Clone(e.data)
		}
		out[i] = &cp
	}
	return out
}
