package zipfile_test

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/ext/zipfile"
	"github.com/ncruces/go-sqlite3/internal/testcfg"
)

func TestMain(m *testing.M) {
	sqlite3.AutoExtension(zipfile.Register)
	os.Exit(m.Run())
}

func openDB(t *testing.T) *sqlite3.Conn {
	t.Helper()
	db, err := sqlite3.OpenContext(testcfg.Context(t), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// aggregateBlob runs a query that must return a single blob column.
func aggregateBlob(t *testing.T, db *sqlite3.Conn, sql string, args ...any) []byte {
	t.Helper()
	stmt, _, err := db.Prepare(sql)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for i, arg := range args {
		switch v := arg.(type) {
		case string:
			err = stmt.BindText(i+1, v)
		case []byte:
			err = stmt.BindBlob(i+1, v)
		case int64:
			err = stmt.BindInt64(i+1, v)
		case int:
			err = stmt.BindInt(i+1, v)
		case nil:
			err = stmt.BindNull(i + 1)
		default:
			t.Fatalf("unsupported bind type %T", arg)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !stmt.Step() {
		if err := stmt.Err(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("aggregate returned no row")
	}
	if stmt.ColumnType(0) == sqlite3.NULL {
		return nil
	}
	return bytes.Clone(stmt.ColumnBlob(0, nil))
}

type member struct {
	name    string
	mode    int64
	mtime   int64
	sz      int64
	rawdata []byte
	data    []byte
	method  int64
}

// allColumns matches the fixed column positions read by readMembers.
const allColumns = `name, mode, mtime, sz, rawdata, data, method`

func readMembers(t *testing.T, db *sqlite3.Conn, sql string, args ...any) []member {
	t.Helper()
	stmt, _, err := db.Prepare(sql)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for i, arg := range args {
		var err error
		switch v := arg.(type) {
		case string:
			err = stmt.BindText(i+1, v)
		case []byte:
			err = stmt.BindBlob(i+1, v)
		default:
			t.Fatalf("unsupported bind type %T", arg)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	var got []member
	for stmt.Step() {
		m := member{
			name:   stmt.ColumnText(0),
			mode:   stmt.ColumnInt64(1),
			mtime:  stmt.ColumnInt64(2),
			sz:     stmt.ColumnInt64(3),
			method: stmt.ColumnInt64(6),
		}
		if stmt.ColumnType(4) != sqlite3.NULL {
			m.rawdata = bytes.Clone(stmt.ColumnBlob(4, nil))
		}
		if stmt.ColumnType(5) != sqlite3.NULL {
			m.data = bytes.Clone(stmt.ColumnBlob(5, nil))
		}
		got = append(got, m)
	}
	if err := stmt.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAggregateAndTableValuedFunction(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	blob := aggregateBlob(t, db, `
		SELECT zipfile(name, mode, mtime, data, method) FROM (
			SELECT 'hello.txt' AS name, 33188 AS mode, 1700000000 AS mtime,
			       CAST('Hello, world!' AS BLOB) AS data, 8 AS method
			UNION ALL
			SELECT 'dir/', 16877, 1700000001, NULL, NULL
			UNION ALL
			SELECT 'stored.bin', 33188, 1700000002,
			       CAST('abc' AS BLOB), 0
		)`)
	if blob == nil {
		t.Fatal("aggregate returned NULL")
	}

	got := readMembers(t, db,
		`SELECT name, mode, mtime, sz, rawdata, data, method FROM zipfile(?)`, blob)
	if len(got) != 3 {
		t.Fatalf("got %d members, want 3", len(got))
	}

	file := got[0]
	if file.name != "hello.txt" || file.mode != 33188 || file.mtime != 1700000000 {
		t.Errorf("metadata mismatch: %+v", file)
	}
	if file.sz != int64(len("Hello, world!")) {
		t.Errorf("sz = %d, want %d", file.sz, len("Hello, world!"))
	}
	if file.method != 8 {
		t.Errorf("method = %d, want 8 (deflate)", file.method)
	}
	if string(file.data) != "Hello, world!" {
		t.Errorf("data = %q, want %q", file.data, "Hello, world!")
	}
	if len(file.rawdata) == 0 || bytes.Equal(file.rawdata, file.data) {
		t.Error("rawdata should hold deflate-compressed bytes")
	}

	dir := got[1]
	if dir.name != "dir/" || dir.mode != 16877 || dir.mtime != 1700000001 {
		t.Errorf("directory metadata mismatch: %+v", dir)
	}
	if dir.sz != 0 || dir.method != 0 || dir.data != nil || dir.rawdata != nil {
		t.Errorf("directory columns mismatch: %+v", dir)
	}

	stored := got[2]
	if stored.method != 0 {
		t.Errorf("method = %d, want 0 (stored)", stored.method)
	}
	if !bytes.Equal(stored.rawdata, stored.data) || string(stored.data) != "abc" {
		t.Error("stored member rawdata must equal data")
	}
}

func TestAggregateMethodSelection(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	// Explicit methods are honored.
	for _, method := range []int64{0, 8} {
		blob := aggregateBlob(t, db,
			`SELECT zipfile('a.txt', 33188, 1, ?, ?)`,
			[]byte("some content here"), method)
		got := readMembers(t, db, `SELECT `+allColumns+` FROM zipfile(?)`, blob)
		if got[0].method != method {
			t.Errorf("method = %d, want %d", got[0].method, method)
		}
	}

	// Automatic selection stores incompressible data.
	random := aggregateBlob(t, db,
		`SELECT zipfile('rand.bin', 33188, 1, randomblob(1024), NULL)`)
	if got := readMembers(t, db, `SELECT `+allColumns+` FROM zipfile(?)`, random); got[0].method != 0 {
		t.Errorf("random blob method = %d, want 0 (stored)", got[0].method)
	}

	// Automatic selection deflates highly compressible data.
	repetitive := aggregateBlob(t, db,
		`SELECT zipfile('rep.txt', 33188, 1, CAST(hex(zeroblob(1000)) AS BLOB), NULL)`)
	if got := readMembers(t, db, `SELECT `+allColumns+` FROM zipfile(?)`, repetitive); got[0].method != 8 {
		t.Errorf("repetitive blob method = %d, want 8 (deflate)", got[0].method)
	}

	// Unsupported methods are an error.
	stmt, _, err := db.Prepare(
		`SELECT zipfile('a.txt', 33188, 1, X'01', 9)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if stmt.Step() {
		t.Error("unsupported method should fail")
	}
	if err := stmt.Err(); err == nil {
		t.Error("unsupported method should return an error")
	}
}

func TestAggregateFormsAndDefaults(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	before := time.Now().Unix()

	// Two arguments: (name, data); mode and mtime default.
	blob := aggregateBlob(t, db,
		`SELECT zipfile('two.txt', CAST('data' AS BLOB))`)
	got := readMembers(t, db,
		`SELECT `+allColumns+` FROM zipfile(?)`, blob)
	if got[0].name != "two.txt" || got[0].mode != 33188 {
		t.Errorf("2-arg defaults mismatch: %+v", got[0])
	}
	if got[0].mtime < before-1 || got[0].mtime > time.Now().Unix()+1 {
		t.Errorf("default mtime = %d, want ~now", got[0].mtime)
	}
	if string(got[0].data) != "data" {
		t.Errorf("data = %q", got[0].data)
	}

	// Four arguments: (name, mode, mtime, data).
	blob = aggregateBlob(t, db,
		`SELECT zipfile('four.txt', 33206, 12345, CAST('xy' AS BLOB))`)
	got = readMembers(t, db, `SELECT `+allColumns+` FROM zipfile(?)`, blob)
	if got[0].mode != 33206 || got[0].mtime != 12345 {
		t.Errorf("4-arg metadata mismatch: %+v", got[0])
	}

	// NULL data creates a directory.
	blob = aggregateBlob(t, db,
		`SELECT zipfile('newdir', NULL, NULL, NULL)`)
	got = readMembers(t, db,
		`SELECT `+allColumns+` FROM zipfile(?)`, blob)
	if got[0].name != "newdir/" || got[0].mode != 16877 {
		t.Errorf("directory defaults mismatch: %+v", got[0])
	}
	if got[0].data != nil || got[0].rawdata != nil || got[0].method != 0 {
		t.Errorf("directory columns mismatch: %+v", got[0])
	}

	// Unsupported argument counts fail.
	if err := db.Exec(`SELECT zipfile('a', 1, 2)`); err == nil {
		t.Error("3-argument zipfile should fail")
	}
}

func TestAggregateEmptyInput(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	blob := aggregateBlob(t, db, `
		SELECT zipfile(name, mode, mtime, data, method) FROM (
			SELECT 'x' AS name, 33188 AS mode, 0 AS mtime, X'01' AS data, 0 AS method
			WHERE 0
		)`)
	if blob != nil {
		t.Errorf("aggregate over zero rows = %v, want NULL", blob)
	}
}

func TestModeStrings(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	cases := []struct {
		s    string
		mode int64
	}{
		{"-rwxr-xr-x", 0o100755},
		{"-rw-r--r--", 0o100644},
		{"drwxrwxrwx", 0o040777},
		{"lrwxrwxrwx", 0o120777},
		{"-rwsr-xr-x", 0o104755},
	}
	for _, c := range cases {
		blob := aggregateBlob(t, db,
			`SELECT zipfile('f', ?, 0, CAST('x' AS BLOB), 0)`, c.s)
		got := readMembers(t, db, `SELECT `+allColumns+` FROM zipfile(?)`, blob)
		if got[0].mode != c.mode {
			t.Errorf("mode(%q) = %o, want %o", c.s, got[0].mode, c.mode)
		}
	}

	stmt, _, err := db.Prepare(
		`SELECT zipfile('f', 'bogus', 0, X'01', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if stmt.Step() || stmt.Err() == nil {
		t.Error("invalid mode string should fail")
	}
}

func TestTableValuedFunctionErrors(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	// Corrupt blob.
	stmt, _, err := db.Prepare(`SELECT name FROM zipfile(?)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := stmt.BindBlob(1, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if stmt.Step() || stmt.Err() == nil {
		t.Error("corrupt blob should fail")
	}
	stmt.Close()

	// Missing file.
	if err := db.Exec(`SELECT name FROM zipfile('/no/such/archive.zip')`); err == nil {
		t.Error("missing archive should fail")
	}

	// Eponymous scan without a source.
	if err := db.Exec(`SELECT name FROM zipfile`); err == nil {
		t.Error("zipfile without an argument should fail")
	}
}

func TestEmptyBlobIsEmptyArchive(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	got := readMembers(t, db, `SELECT name FROM zipfile(?)`, []byte{})
	if len(got) != 0 {
		t.Errorf("empty blob returned %d rows, want 0", len(got))
	}
}

func TestHiddenColumn(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	blob := aggregateBlob(t, db,
		`SELECT zipfile('a.txt', 33188, 0, CAST('x' AS BLOB), 0)`)

	stmt, _, err := db.Prepare(`SELECT zf, typeof(zf) FROM zipfile(?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if err := stmt.BindBlob(1, blob); err != nil {
		t.Fatal(err)
	}
	if !stmt.Step() {
		t.Fatal(stmt.Err())
	}
	if got := stmt.ColumnText(1); got != "blob" {
		t.Errorf("hidden column type = %q, want blob", got)
	}
	if !bytes.Equal(stmt.ColumnBlob(0, nil), blob) {
		t.Error("hidden blob column should echo the source blob")
	}
}
