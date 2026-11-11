package zipfile

import (
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/internal/errutil"
)

// newZipAggregate builds the aggregate function state.
func newZipAggregate() sqlite3.AggregateFunction {
	return &zipAggregate{}
}

type zipAggregate struct {
	entries []*zipEntry
}

// Step accepts 2, 4 or 5 arguments:
//
//	zipfile(name, data)
//	zipfile(name, mode, mtime, data)
//	zipfile(name, mode, mtime, data, method)
//
// Missing middle arguments default to NULL, as in the C extension.
func (a *zipAggregate) Step(ctx sqlite3.Context, arg ...sqlite3.Value) {
	var (
		nameV, modeV, mtimeV, dataV, methodV sqlite3.Value
		hasMode, hasMtime, hasMethod         bool
	)

	switch len(arg) {
	case 2:
		nameV, dataV = arg[0], arg[1]
	case 4:
		nameV, modeV, mtimeV, dataV = arg[0], arg[1], arg[2], arg[3]
		hasMode, hasMtime = true, true
	case 5:
		nameV, modeV, mtimeV, dataV, methodV = arg[0], arg[1], arg[2], arg[3], arg[4]
		hasMode, hasMtime, hasMethod = true, true, true
	default:
		ctx.ResultError(errutil.ErrorString("zipfile: expects 2, 4 or 5 arguments"))
		return
	}

	if nameV.Type() == sqlite3.NULL {
		ctx.ResultError(errutil.ErrorString("zipfile: name must not be NULL"))
		return
	}
	name := nameV.Text()

	data := valueData(dataV)
	isDir := data == nil

	var mode uint32
	if hasMode {
		m, err := valueMode(modeV, isDir)
		if err != nil {
			ctx.ResultError(err)
			return
		}
		mode = m
	} else if isDir {
		mode = 0o040755
	} else {
		mode = 0o100644
	}

	var mtime int64
	if hasMtime {
		mtime = valueMtime(mtimeV)
	} else {
		mtime = time.Now().Unix()
	}

	method, hasMethodResolved := 0, false
	if hasMethod {
		m, has, err := valueMethod(methodV)
		if err != nil {
			ctx.ResultError(err)
			return
		}
		method, hasMethodResolved = m, has
	}

	e, err := makeEntry(name, mode, mtime, data, method, hasMethodResolved)
	if err != nil {
		ctx.ResultError(err)
		return
	}
	a.entries = append(a.entries, e)
}

// Value returns the assembled ZIP image, or NULL with no input rows.
func (a *zipAggregate) Value(ctx sqlite3.Context) {
	if len(a.entries) == 0 {
		ctx.ResultNull()
		return
	}
	img, err := serialize(a.entries)
	if err != nil {
		ctx.ResultError(err)
		return
	}
	ctx.ResultBlob(img)
}
