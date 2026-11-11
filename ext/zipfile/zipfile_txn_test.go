//go:build !js

package zipfile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

// writeArchive builds an archive with the aggregate function
// and writes it to path.
func writeArchive(t *testing.T, db *sqlite3.Conn, path string) {
	t.Helper()
	blob := aggregateBlob(t, db, `
		SELECT zipfile(name, mode, mtime, data, method) FROM (
			SELECT 'a.txt' AS name, 33188 AS mode, 1700000000 AS mtime,
			       CAST('alpha' AS BLOB) AS data, 8 AS method
			UNION ALL
			SELECT 'b.txt', 33188, 1700000001, CAST('beta' AS BLOB), 0
			UNION ALL
			SELECT 'd/', 16877, 1700000002, NULL, NULL
		)`)
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

func createVTab(t *testing.T, db *sqlite3.Conn, path string) {
	t.Helper()
	if err := db.Exec(
		`CREATE VIRTUAL TABLE z USING zipfile(` + sqlite3.Quote(path) + `)`); err != nil {
		t.Fatal(err)
	}
}

func TestVTabRead(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := filepath.Join(t.TempDir(), "test.zip")
	writeArchive(t, db, path)
	createVTab(t, db, path)

	got := readMembers(t, db,
		`SELECT name, mode, mtime, sz, rawdata, data, method FROM z ORDER BY rowid`)
	if len(got) != 3 {
		t.Fatalf("got %d members, want 3", len(got))
	}
	if got[0].name != "a.txt" || string(got[0].data) != "alpha" || got[0].method != 8 {
		t.Errorf("row 0 mismatch: %+v", got[0])
	}
	if got[1].name != "b.txt" || string(got[1].data) != "beta" || got[1].method != 0 {
		t.Errorf("row 1 mismatch: %+v", got[1])
	}
	if got[2].name != "d/" || got[2].data != nil {
		t.Errorf("row 2 mismatch: %+v", got[2])
	}

	// The same archive is readable through the table-valued function.
	pathMembers := readMembers(t, db,
		`SELECT `+allColumns+` FROM zipfile(?) ORDER BY rowid`, path)
	if len(pathMembers) != 3 || string(pathMembers[0].data) != "alpha" {
		t.Errorf("path TVF read mismatch: %+v", pathMembers)
	}
}

func TestVTabCommit(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := filepath.Join(t.TempDir(), "test.zip")
	writeArchive(t, db, path)
	createVTab(t, db, path)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO z(name, mode, mtime, data, method)
		VALUES ('c.txt', 33188, 1700000003, CAST('gamma' AS BLOB), 0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE z SET data = CAST('ALPHA' AS BLOB) WHERE name = 'a.txt'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`DELETE FROM z WHERE name = 'b.txt'`); err != nil {
		t.Fatal(err)
	}

	// Uncommitted changes must not reach disk.
	if mid, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(mid, before) {
		t.Fatal("archive changed before COMMIT")
	}

	// The virtual table itself sees the buffered changes.
	if n := countRows(t, db, `SELECT COUNT(*) FROM z`); n != 3 {
		t.Errorf("uncommitted row count = %d, want 3", n)
	}

	if err := db.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, before) {
		t.Fatal("archive unchanged after COMMIT")
	}

	got := readMembers(t, db,
		`SELECT `+allColumns+` FROM zipfile(?) ORDER BY rowid`, after)
	want := map[string]string{"a.txt": "ALPHA", "c.txt": "gamma", "d/": ""}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d", len(got), len(want))
	}
	for _, m := range got {
		if string(m.data) != want[m.name] {
			t.Errorf("member %q data = %q, want %q", m.name, m.data, want[m.name])
		}
	}
}

func TestVTabRollback(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := filepath.Join(t.TempDir(), "test.zip")
	writeArchive(t, db, path)
	createVTab(t, db, path)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO z(name, data) VALUES ('new.txt', CAST('new' AS BLOB))`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE z SET mode = 0700 WHERE name = 'a.txt'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`DELETE FROM z WHERE name = 'b.txt'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}

	// The archive on disk must be byte-for-byte identical.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("archive changed after ROLLBACK")
	}

	// The buffered in-memory changes must also be gone.
	if n := countRows(t, db, `SELECT COUNT(*) FROM z`); n != 3 {
		t.Errorf("row count after rollback = %d, want 3", n)
	}
	got := readMembers(t, db, `SELECT `+allColumns+` FROM z ORDER BY rowid`)
	if got[0].name != "a.txt" || got[0].mode != 33188 || string(got[0].data) != "alpha" {
		t.Errorf("rolled back row mismatch: %+v", got[0])
	}
}

func TestVTabAutocommit(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := filepath.Join(t.TempDir(), "test.zip")
	writeArchive(t, db, path)
	createVTab(t, db, path)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Exec(`
		INSERT INTO z(name, data) VALUES ('one.txt', CAST('one' AS BLOB))`); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, before) {
		t.Fatal("autocommit statement did not write the archive")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM zipfile(?)`, after); n != 4 {
		t.Errorf("row count = %d, want 4", n)
	}
}

func TestVTabUpdateAndColumnRules(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := filepath.Join(t.TempDir(), "test.zip")
	writeArchive(t, db, path)
	createVTab(t, db, path)

	if err := db.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}

	// Rename, retime and change compression method.
	if err := db.Exec(`
		UPDATE z SET name = 'renamed.txt', mtime = 1800000000, method = 0
		WHERE name = 'a.txt'`); err != nil {
		t.Fatal(err)
	}

	// sz and rawdata cannot be assigned.
	if err := db.Exec(`UPDATE z SET sz = 1 WHERE name = 'b.txt'`); err == nil {
		t.Error("setting sz should fail")
	}
	if err := db.Exec(`UPDATE z SET rawdata = X'00' WHERE name = 'b.txt'`); err == nil {
		t.Error("setting rawdata should fail")
	}

	// Duplicate names are rejected.
	if err := db.Exec(`
		INSERT INTO z(name, data) VALUES ('b.txt', CAST('x' AS BLOB))`); err == nil {
		t.Error("duplicate name should fail")
	}

	// NULL name is rejected.
	if err := db.Exec(`INSERT INTO z(name) VALUES (NULL)`); err == nil {
		t.Error("NULL name should fail")
	}

	// A failed statement must not poison the transaction.
	if err := db.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}

	got := readMembers(t, db,
		`SELECT `+allColumns+` FROM zipfile(?) ORDER BY rowid`,
		mustReadFile(t, path))
	if got[0].name != "renamed.txt" || got[0].mtime != 1800000000 || got[0].method != 0 {
		t.Errorf("updated row mismatch: %+v", got[0])
	}
	if string(got[0].data) != "alpha" {
		t.Errorf("data changed unexpectedly: %q", got[0].data)
	}
}

func TestVTabNewArchive(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := filepath.Join(t.TempDir(), "new.zip")

	createVTab(t, db, path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("archive should not exist before first commit, stat err = %v", err)
	}

	if err := db.Exec(`
		INSERT INTO z(name, data) VALUES ('first.txt', CAST('hello' AS BLOB))`); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("archive was not created on commit: %v", err)
	}
	got := readMembers(t, db, `SELECT `+allColumns+` FROM zipfile(?)`, mustReadFile(t, path))
	if len(got) != 1 || got[0].name != "first.txt" || string(got[0].data) != "hello" {
		t.Errorf("new archive mismatch: %+v", got[0])
	}
}

func TestVTabDeleteAll(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := filepath.Join(t.TempDir(), "test.zip")
	writeArchive(t, db, path)
	createVTab(t, db, path)

	if err := db.Exec(`DELETE FROM z`); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM zipfile(?)`, mustReadFile(t, path)); n != 0 {
		t.Errorf("row count = %d, want 0", n)
	}
}

func TestVTabRollbackAfterError(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := filepath.Join(t.TempDir(), "test.zip")
	writeArchive(t, db, path)
	createVTab(t, db, path)

	before := mustReadFile(t, path)

	if err := db.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO z(name, data) VALUES ('ok.txt', CAST('ok' AS BLOB))`); err != nil {
		t.Fatal(err)
	}
	// A bad method aborts the statement but the transaction stays open.
	if err := db.Exec(`
		INSERT INTO z(name, data, method) VALUES ('bad.txt', X'01', 9)`); err == nil {
		t.Error("bad method should fail")
	}
	if err := db.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}

	if after := mustReadFile(t, path); !bytes.Equal(after, before) {
		t.Fatal("archive changed after ROLLBACK")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM z`); n != 3 {
		t.Errorf("row count = %d, want 3", n)
	}
}

func TestEponymousTableIsReadOnly(t *testing.T) {
	t.Parallel()
	db := openDB(t)

	// The eponymous table has no backing archive: writes are rejected
	// at xUpdate even though the statement can be prepared.
	stmt, _, err := db.Prepare(
		`INSERT INTO zipfile(name, data) VALUES ('b.txt', X'01')`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if stmt.Step() || stmt.Err() == nil {
		t.Error("writing to an eponymous table should fail")
	}
}

func countRows(t *testing.T, db *sqlite3.Conn, sql string, args ...any) int64 {
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
		default:
			t.Fatalf("unsupported bind type %T", arg)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !stmt.Step() {
		t.Fatal(stmt.Err())
	}
	return stmt.ColumnInt64(0)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
