package trdsql

import (
	"context"
	"io"
)

// BufferImporter includes tableName and Reader.
type BufferImporter struct {
	Reader
	tableName string
	typed     bool
}

// NewBufferImporter returns a BufferImporter.
func NewBufferImporter(tableName string, r io.Reader, options ...ReadOpt) (*BufferImporter, error) {
	readOpts := NewReadOpts(options...)
	readOpts.realFormat = readOpts.InFormat
	reader, err := NewReader(r, readOpts)
	if err != nil {
		return nil, err
	}
	if readOpts.InTypeInfer {
		reader = newInferReader(reader)
	}
	return &BufferImporter{
		tableName: tableName,
		Reader:    reader,
		typed:     readOpts.InTypeInfer,
	}, nil
}

// Import imports data from Reader in BufferImporter.
func (i *BufferImporter) Import(db *DB, query string) (string, error) {
	ctx := context.Background()
	return i.ImportContext(ctx, db, query)
}

// ImportContext imports data from Reader in BufferImporter with context.
func (i *BufferImporter) ImportContext(ctx context.Context, db *DB, query string) (string, error) {
	names, err := i.Names()
	if err != nil {
		return query, err
	}
	types, err := i.Types()
	if err != nil {
		return query, err
	}
	createTypes := types
	if i.typed {
		createTypes = db.portableTypes(types)
	}
	if err := db.CreateTable(i.tableName, names, createTypes, true); err != nil {
		return query, err
	}
	if i.typed {
		return query, db.ImportTypedContext(ctx, i.tableName, names, types, true, i.Reader)
	}
	return query, db.ImportContext(ctx, i.tableName, names, i.Reader)
}
