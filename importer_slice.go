package trdsql

import "context"

// SliceImporter includes SliceReader.
// SliceImporter can be used as a library from another program.
// It is not used from the command.
// SliceImporter is an importer that reads one slice data.
type SliceImporter struct {
	*SliceReader
	typed bool
}

// NewSliceImporter returns a SliceImporter.
func NewSliceImporter(tableName string, data any) *SliceImporter {
	return &SliceImporter{
		SliceReader: NewSliceReader(tableName, data),
	}
}

// NewSliceImporterWithTypeInfer returns a SliceImporter that infers column
// types from the Go values (int64, float64, string, ...) instead of mapping
// them with SliceReader's coarse int/text rule.
func NewSliceImporterWithTypeInfer(tableName string, data any) *SliceImporter {
	return &SliceImporter{
		SliceReader: NewSliceReader(tableName, data),
		typed:       true,
	}
}

// typedReader returns the reader used for a typed import.
// Type inference is applied to the slice values so that a column that is all
// integers in Go becomes an integer column, and one that contains floats
// becomes numeric, regardless of SliceReader's coarse kind mapping.
func (i *SliceImporter) typedReader() Reader {
	return newInferReader(i.SliceReader)
}

// Import imports data from SliceReader in SliceImporter.
func (i *SliceImporter) Import(db *DB, query string) (string, error) {
	ctx := context.Background()
	return i.ImportContext(ctx, db, query)
}

// ImportContext imports data from SliceReader in SliceImporter with context.
func (i *SliceImporter) ImportContext(ctx context.Context, db *DB, query string) (string, error) {
	reader := Reader(i.SliceReader)
	if i.typed {
		reader = i.typedReader()
	}
	names, err := reader.Names()
	if err != nil {
		return query, err
	}
	types, err := reader.Types()
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
		return query, db.ImportTypedContext(ctx, i.tableName, names, types, true, reader)
	}
	return query, db.ImportContext(ctx, i.tableName, names, reader)
}
