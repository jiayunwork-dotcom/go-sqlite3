// Package zipfile provides the zipfile virtual table, table-valued
// function and aggregate function.
//
// The table-valued function lists the members of a ZIP archive
// stored on disk or passed as a blob:
//
//	SELECT * FROM zipfile('test.zip')
//	SELECT * FROM zipfile(readfile('test.zip'))
//
// A writable virtual table is bound to an archive on disk:
//
//	CREATE VIRTUAL TABLE z USING zipfile('test.zip')
//
// Changes made through such a table are buffered in memory and only
// written back to disk when the enclosing transaction commits;
// rolling the transaction back leaves the archive untouched.
//
// The aggregate function builds an archive image in memory:
//
//	SELECT zipfile(name, mode, mtime, data, method) FROM files
//
// https://sqlite.org/zipfile.html
package zipfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/internal/errutil"
	"github.com/ncruces/go-sqlite3/util/sql3util"
)

const vtabSchema = `CREATE TABLE x(
	name TEXT,
	mode INT,
	mtime INT,
	sz INT,
	rawdata BLOB,
	data BLOB,
	method INT,
	zf HIDDEN
)`

const (
	colName = iota
	colMode
	colMtime
	colSz
	colRaw
	colData
	colMethod
	colFile
)

// Register registers the zipfile virtual table module and
// the zipfile aggregate function.
func Register(db *sqlite3.Conn) error {
	// The constructor is passed as xConnect only. zipTab implements
	// VTabDestroyer, so the shim aliases xCreate to the same function
	// pointer; SQLite then accepts the module both as a table-valued
	// function (FROM zipfile(...)) and in CREATE VIRTUAL TABLE.
	return errors.Join(
		sqlite3.CreateModule(db, "zipfile", nil, connect),
		db.CreateWindowFunction("zipfile", -1, 0, newZipAggregate))
}

type zipTab struct {
	path    string      // "" for an eponymous, blob- or path-backed scan
	entries []*zipEntry // working set of a disk-bound table
	nextID  int64       // next stable rowid
	txn     bool
	dirty   bool        // an Update modified the working set during the txn
	backup  []*zipEntry // snapshot taken at Begin, restored on Rollback
	tmp     string      // temp file written by Sync, renamed on Commit
}

func connect(db *sqlite3.Conn, _, _, _ string, arg ...string) (*zipTab, error) {
	t := &zipTab{}
	if len(arg) > 0 {
		// CREATE VIRTUAL TABLE ... USING zipfile('path.zip')
		t.path = sql3util.Unquote(arg[0])

		src, err := os.ReadFile(t.path)
		switch {
		case err == nil:
			t.entries, err = parseArchive(src)
			if err != nil {
				return nil, fmt.Errorf("zipfile: %w", err)
			}
		case errors.Is(err, fs.ErrNotExist):
			// A missing archive starts empty and is created on commit.
		default:
			return nil, fmt.Errorf("zipfile: %w", err)
		}
		t.nextID = int64(len(t.entries)) + 1
	}

	// The table has no side effects beyond an explicit commit, so it is
	// safe to invoke from views and triggers like the C extension.
	if err := db.VTabConfig(sqlite3.VTAB_INNOCUOUS); err != nil {
		return nil, err
	}

	if err := db.DeclareVTab(vtabSchema); err != nil {
		return nil, err
	}
	return t, nil
}

// Destroy drops a virtual table. The archive on disk is owned by the
// user, so dropping the table leaves it in place.
func (t *zipTab) Destroy() error { return nil }

func (t *zipTab) BestIndex(idx *sqlite3.IndexInfo) error {
	idx.EstimatedCost = 1e6
	for i, cst := range idx.Constraint {
		if cst.Column == colFile && cst.Usable && cst.Op == sqlite3.INDEX_CONSTRAINT_EQ {
			idx.ConstraintUsage[i] = sqlite3.IndexConstraintUsage{
				Omit:      true,
				ArgvIndex: 1,
			}
			idx.IdxNum = 1
			return nil
		}
	}
	// An eponymous scan needs its source passed as the hidden argument.
	if t.path == "" {
		return sqlite3.CONSTRAINT
	}
	return nil
}

func (t *zipTab) Open() (sqlite3.VTabCursor, error) {
	return &zipCursor{tab: t}, nil
}

type zipCursor struct {
	tab     *zipTab
	entries []*zipEntry
	row     int
	eof     bool

	hidSet  bool
	hidText string
	hidBlob []byte
	hidType sqlite3.Datatype
}

func (c *zipCursor) Close() error { return nil }

func (c *zipCursor) Filter(idxNum int, _ string, arg ...sqlite3.Value) error {
	c.row = 0
	c.eof = false
	c.hidSet = false

	if idxNum == 1 {
		v := arg[0]
		c.hidSet = true
		c.hidType = v.Type()
		switch v.Type() {
		case sqlite3.BLOB:
			// The SQLite-owned buffer must not outlive the call.
			src := bytes.Clone(v.RawBlob())
			c.hidBlob = src
			entries, err := parseArchive(src)
			if err != nil {
				return fmt.Errorf("zipfile: %w", err)
			}
			c.entries = entries
		case sqlite3.TEXT:
			path := v.Text()
			c.hidText = path
			src, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("zipfile: %w", err)
			}
			entries, err := parseArchive(src)
			if err != nil {
				return fmt.Errorf("zipfile: %w", err)
			}
			c.entries = entries
		default:
			return errutil.ErrorString("zipfile: argument must be a text path or blob")
		}
	} else {
		c.entries = c.tab.entries
	}

	if len(c.entries) == 0 {
		c.eof = true
	}
	return nil
}

func (c *zipCursor) Next() error {
	c.row++
	if c.row >= len(c.entries) {
		c.eof = true
	}
	return nil
}

func (c *zipCursor) EOF() bool { return c.eof }

func (c *zipCursor) RowID() (int64, error) {
	return c.entries[c.row].rowid, nil
}

func (c *zipCursor) Column(ctx sqlite3.Context, n int) error {
	// During an UPDATE, columns that are not modified arrive with the
	// no-change marker; leaving the result untouched lets SQLite pass a
	// value for which Value.NoChange() reports true in Update.
	if ctx.VTabNoChange() {
		return nil
	}
	e := c.entries[c.row]
	switch n {
	case colName:
		ctx.ResultText(e.name)

	case colMode:
		ctx.ResultInt64(int64(e.mode))

	case colMtime:
		ctx.ResultInt64(e.mtime)

	case colSz:
		if e.dir {
			ctx.ResultInt64(0)
		} else {
			ctx.ResultInt64(int64(e.usize))
		}

	case colRaw:
		if e.dir {
			ctx.ResultNull()
			return nil
		}
		raw, err := e.getRaw()
		if err != nil {
			return err
		}
		ctx.ResultBlob(raw)

	case colData:
		if e.dir || (e.method != methodStore && e.method != methodDeflate) {
			ctx.ResultNull()
			return nil
		}
		data, err := e.getData()
		if err != nil {
			return err
		}
		ctx.ResultBlob(data)

	case colMethod:
		ctx.ResultInt64(int64(e.method))

	case colFile:
		switch {
		case c.hidSet && c.hidType == sqlite3.BLOB:
			ctx.ResultBlob(c.hidBlob)
		case c.hidSet && c.hidType == sqlite3.TEXT:
			ctx.ResultText(c.hidText)
		default:
			ctx.ResultText(c.tab.path)
		}
	}
	return nil
}

// Update implements [sqlite3.VTabUpdater].
func (t *zipTab) Update(arg ...sqlite3.Value) (rowid int64, err error) {
	if t.path == "" {
		return 0, errutil.ErrorString("zipfile: cannot modify an eponymous table")
	}

	// DELETE: a single argument holds the old rowid.
	if len(arg) == 1 {
		idx := t.indexOfRowid(arg[0].Int64())
		if idx < 0 {
			return 0, errutil.ErrorString("zipfile: invalid rowid")
		}
		t.entries = append(t.entries[:idx], t.entries[idx+1:]...)
		t.dirty = true
		return arg[0].Int64(), nil
	}

	if arg[0].Type() == sqlite3.NULL {
		// INSERT: arg[1] is the requested rowid (ignored).
		e, err := t.buildEntry(arg[2:], nil)
		if err != nil {
			return 0, err
		}
		if t.findEntry(e.name) >= 0 {
			return 0, fmt.Errorf("zipfile: duplicate name: %q", e.name)
		}
		e.rowid = t.nextID
		t.nextID++
		t.entries = append(t.entries, e)
		t.dirty = true
		return e.rowid, nil
	}

	// UPDATE: arg[0] is the old rowid, arg[1] the new rowid.
	idx := t.indexOfRowid(arg[0].Int64())
	if idx < 0 {
		return 0, errutil.ErrorString("zipfile: invalid rowid")
	}
	e, err := t.buildEntry(arg[2:], t.entries[idx])
	if err != nil {
		return 0, err
	}
	if j := t.findEntry(e.name); j >= 0 && j != idx {
		return 0, fmt.Errorf("zipfile: duplicate name: %q", e.name)
	}
	e.rowid = t.entries[idx].rowid
	t.entries[idx] = e
	t.dirty = true
	return e.rowid, nil
}

func (t *zipTab) indexOfRowid(id int64) int {
	for i, e := range t.entries {
		if e.rowid == id {
			return i
		}
	}
	return -1
}

func (t *zipTab) findEntry(name string) int {
	for i, e := range t.entries {
		if e.name == name {
			return i
		}
	}
	return -1
}

// buildEntry resolves the column values of an INSERT or UPDATE.
// On an UPDATE, columns flagged NoChange keep their old value;
// on an INSERT every column carries an explicit value (NULL when
// the column was omitted from the column list).
func (t *zipTab) buildEntry(vals []sqlite3.Value, old *zipEntry) (*zipEntry, error) {
	unchanged := func(i int) bool {
		return old != nil && vals[i].NoChange()
	}

	// sz and rawdata are derived values: they may never be assigned.
	if !unchanged(colSz) && vals[colSz].Type() != sqlite3.NULL {
		return nil, errutil.ErrorString("zipfile: sz must be NULL")
	}
	if !unchanged(colRaw) && vals[colRaw].Type() != sqlite3.NULL {
		return nil, errutil.ErrorString("zipfile: rawdata must be NULL")
	}

	var name string
	switch {
	case unchanged(colName):
		name = old.name
	case vals[colName].Type() == sqlite3.NULL:
		return nil, errutil.ErrorString("zipfile: name must not be NULL")
	default:
		name = vals[colName].Text()
	}

	var data []byte
	if unchanged(colData) {
		// Parsed entries load their bytes lazily, so recover the
		// uncompressed content instead of trusting the cached flag.
		d, err := old.getData()
		if err != nil {
			return nil, err
		}
		data = d
	} else {
		data = valueData(vals[colData])
	}
	isDir := data == nil

	var mode uint32
	if unchanged(colMode) {
		mode = old.mode
	} else {
		m, err := valueMode(vals[colMode], isDir)
		if err != nil {
			return nil, err
		}
		mode = m
	}

	var mtime int64
	if unchanged(colMtime) {
		mtime = old.mtime
	} else {
		mtime = valueMtime(vals[colMtime])
	}

	method, hasMethod := 0, false
	if unchanged(colMethod) {
		method, hasMethod = int(old.method), true
	} else {
		m, has, err := valueMethod(vals[colMethod])
		if err != nil {
			return nil, err
		}
		method, hasMethod = m, has
	}
	if isDir {
		method, hasMethod = methodStore, true
	}

	return makeEntry(name, mode, mtime, data, method, hasMethod)
}

// Begin snapshots the working set so it can be restored on Rollback.
// SQLite also wraps CREATE VIRTUAL TABLE in a transaction, so a txn
// without any Update must leave the archive untouched.
func (t *zipTab) Begin() error {
	if t.txn {
		return nil
	}
	t.backup = cloneEntries(t.entries)
	t.dirty = false
	t.txn = true
	return nil
}

// Sync serializes the working set to a temp file next to the archive.
// The archive itself is not touched until Commit.
func (t *zipTab) Sync() error {
	if !t.dirty {
		return nil
	}

	img, err := serialize(t.entries)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(t.path), ".zipfile-*")
	if err != nil {
		return err
	}
	tmp := f.Name()

	_, err = f.Write(img)
	if err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}

	if t.tmp != "" {
		os.Remove(t.tmp)
	}
	t.tmp = tmp
	return nil
}

// Commit atomically replaces the archive with the serialized image.
func (t *zipTab) Commit() error {
	if t.dirty && t.tmp != "" {
		if err := os.Rename(t.tmp, t.path); err != nil {
			return err
		}
		t.tmp = ""
	}
	t.backup = nil
	t.dirty = false
	t.txn = false
	return nil
}

// Rollback discards buffered changes and any temp file.
// The archive on disk is left byte-for-byte untouched.
func (t *zipTab) Rollback() error {
	if t.tmp != "" {
		os.Remove(t.tmp)
		t.tmp = ""
	}
	if t.dirty {
		t.entries = t.backup
	}
	t.backup = nil
	t.dirty = false
	t.txn = false
	return nil
}
