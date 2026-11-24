package trdsql

// Typed import: the import path used when InTypeInfer is enabled.
//
// It writes each value with the Go type of its column and promotes a column
// explicitly when a value does not fit. Promotion rebuilds the table inside
// the running transaction:
//
//	CREATE new table with the widened declaration
//	INSERT ... SELECT CAST(col AS newtype) FROM old   -- keeps earlier rows
//	DROP old
//	ALTER TABLE new RENAME TO old
//
// Earlier rows therefore survive a promotion, and the table ends up with a
// declaration that matches the data. Nothing is left to the driver or to
// SQLite type affinity.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"unicode/utf8"
)

// dbTypeName maps an inference type to the type name declared in the target
// database.
//
// The inference names are portable in spirit but not in width:
//
//   - PostgreSQL INTEGER is int4, which overflows the int64 values trdsql
//     parses, so integers must be declared BIGINT.
//   - MySQL INTEGER is INT (32-bit) and NUMERIC means DECIMAL(10,0), which
//     would silently truncate the fractional part of a promoted column, so
//     they must be BIGINT and DECIMAL(65,30).
//   - SQLite and the other drivers accept the portable names as written.
//
// Keeping the widths right here is what makes the promotion honest on every
// driver instead of only on SQLite.
func (db *DB) dbTypeName(inferenceType string) string {
	switch db.driver {
	case "postgres":
		if canonicalType(inferenceType) == TypeInteger {
			return "bigint"
		}
	case "mysql":
		switch canonicalType(inferenceType) {
		case TypeInteger:
			return "bigint"
		case TypeNumeric:
			return "decimal(65,30)"
		}
	}
	return inferenceType
}

type Promotion struct {
	// Table is the quoted table name.
	Table string
	// Column is the quoted column name.
	Column string
	// From is the declared type before the promotion.
	From string
	// To is the declared type after the promotion.
	To string
}

// Promotions returns the column type widenings applied on this connection,
// in the order they happened.
func (db *DB) Promotions() []Promotion {
	return db.promotions
}

// portableTypes maps inference type names to the names declared in the target
// database. Types outside the inference family are returned unchanged.
func (db *DB) portableTypes(types []string) []string {
	out := make([]string, len(types))
	for i, t := range types {
		if canonicalType(t) == "" {
			out[i] = t
			continue
		}
		out[i] = db.dbTypeName(t)
	}
	return out
}

// ImportTypedContext imports data using explicit column type conversion and
// promotion. It is used instead of ImportContext when InTypeInfer is set.
//
// columnTypes are the declared types from CREATE TABLE. They are updated in
// place when a column is promoted, so the caller can read the final types
// after import.
func (db *DB) ImportTypedContext(ctx context.Context, tableName string, columnNames []string, columnTypes []string, isTemporary bool, reader Reader) error {
	if db.Tx == nil {
		return ErrNoTransaction
	}
	if reader == nil {
		return ErrNilReader
	}
	if len(columnNames) != len(columnTypes) {
		return ErrInvalidTypes
	}
	if len(columnNames) == 0 {
		return ErrInvalidNames
	}

	columns := make([]string, len(columnNames))
	for i := range columnNames {
		columns[i] = db.QuotedName(columnNames[i])
	}

	table := &importTable{
		tableName: tableName,
		columns:   columns,
		row:       make([]any, len(columnNames)),
		// columnTypes stays in inference vocabulary (integer/numeric/text) or
		// in the source's own vocabulary for types outside that family. The
		// per-driver storage names are derived only when emitting DDL, so that
		// promotion bookkeeping and logging are driver independent.
		columnTypes: append([]string(nil), columnTypes...),
		isTemporary: isTemporary,
	}

	if db.driver == "postgres" {
		return db.typedCopyImport(ctx, table, reader)
	}
	return db.typedInsertImport(ctx, table, reader)
}

// batchSize returns the number of values to buffer in one INSERT statement.
func (db *DB) batchSize(columnNum int) int {
	if columnNum > db.maxBulk {
		return columnNum
	}
	return (db.maxBulk / columnNum) * columnNum
}

// typedInsertImport imports rows with multi-row INSERT statements.
func (db *DB) typedInsertImport(ctx context.Context, table *importTable, reader Reader) error {
	var (
		stmt      *sql.Stmt
		stmtCount int
	)
	closeStmt := func() {
		db.stmtClose(stmt)
		stmt = nil
		stmtCount = 0
	}
	defer closeStmt()

	batch := db.batchSize(len(table.row))
	bulk := make([]any, 0, batch)

	flush := func() error {
		if len(bulk) == 0 {
			return nil
		}
		if stmt == nil || stmtCount != table.count {
			closeStmt()
			var err error
			stmt, err = db.insertPrepare(ctx, table)
			if err != nil {
				return err
			}
			stmtCount = table.count
		}
		if _, err := stmt.ExecContext(ctx, bulk...); err != nil {
			return err
		}
		bulk = bulk[:0]
		table.count = 0
		return nil
	}
	// reset is used by a promotion: it flushes the buffered rows and closes the
	// statement, which is bound to the table that is about to be dropped.
	reset := func() error {
		if err := flush(); err != nil {
			return err
		}
		closeStmt()
		return nil
	}

	push := func(row []any) error {
		converted, err := db.typedConvertRow(ctx, table, row, reset)
		if err != nil {
			return err
		}
		bulk = append(bulk, converted...)
		table.count++
		if len(bulk) >= batch {
			return flush()
		}
		return nil
	}

	for _, row := range reader.PreReadRow() {
		if row == nil {
			continue
		}
		if err := push(row); err != nil {
			return fmt.Errorf("INSERT preread: %w", err)
		}
	}

	for {
		row, err := reader.ReadRow(table.row)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("INSERT read: %w", err)
		}
		if len(row) == 0 {
			continue
		}
		if err := push(row); err != nil {
			return fmt.Errorf("INSERT: %w", err)
		}
	}
	return flush()
}

// typedCopyImport imports rows with PostgreSQL COPY.
// A promotion finalizes the current COPY (which writes its rows to the table),
// rebuilds the table, and starts a new COPY against the renamed table.
func (db *DB) typedCopyImport(ctx context.Context, table *importTable, reader Reader) error {
	var stmt *sql.Stmt
	openStmt := func() error {
		query := queryCopy(table)
		debug.Print(query)
		var err error
		stmt, err = db.Tx.PrepareContext(ctx, query)
		if err != nil {
			return fmt.Errorf("COPY prepare: %w", err)
		}
		return nil
	}
	// finish writes the rows buffered by the current COPY and closes it, so
	// that the table can safely be rebuilt.
	finish := func() error {
		if stmt == nil {
			return nil
		}
		_, err := stmt.ExecContext(ctx)
		db.stmtClose(stmt)
		stmt = nil
		return err
	}
	defer func() {
		_ = finish()
	}()

	push := func(row []any) error {
		converted, err := db.typedConvertRow(ctx, table, row, finish)
		if err != nil {
			return err
		}
		if stmt == nil {
			if err := openStmt(); err != nil {
				return err
			}
		}
		if _, err := stmt.ExecContext(ctx, converted...); err != nil {
			return err
		}
		return nil
	}

	for _, row := range reader.PreReadRow() {
		if row == nil {
			continue
		}
		if err := push(row); err != nil {
			return fmt.Errorf("COPY preread: %w", err)
		}
	}
	for {
		row, err := reader.ReadRow(table.row)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("COPY read: %w", err)
		}
		if len(row) == 0 {
			continue
		}
		if err := push(row); err != nil {
			return fmt.Errorf("COPY: %w", err)
		}
	}
	return finish()
}

// typedConvertRow converts every value of row to the Go type of its column.
// When a value does not fit, the column is promoted and the table rebuilt;
// conversion then restarts from the original values, because the promotion
// changes how every column of this row must be written (an int64 column
// accepts values as int64, a text column accepts them as strings, ...).
//
// reset is called before a rebuild to persist buffered rows under the old
// declaration; those rows are what the rebuild carries over.
func (db *DB) typedConvertRow(ctx context.Context, table *importTable, row []any, reset func() error) ([]any, error) {
	converted := make([]any, len(table.columns))
	for {
		rebuild := -1
		for i := range table.columns {
			var original any
			if i < len(row) {
				original = row[i]
			}
			value, ok := convertForType(original, table.columnTypes[i])
			if !ok {
				rebuild = i
				break
			}
			converted[i] = value
		}
		if rebuild < 0 {
			return converted, nil
		}

		var original any
		if rebuild < len(row) {
			original = row[rebuild]
		}
		if err := reset(); err != nil {
			return nil, err
		}
		to := PromoteType(table.columnTypes[rebuild], InferValueType(original))
		if canonicalType(table.columnTypes[rebuild]) == "" || typeRank(to) <= typeRank(table.columnTypes[rebuild]) {
			to = TypeText
		}
		if err := db.rebuildTable(ctx, table, rebuild, to); err != nil {
			return nil, err
		}
	}
}

// rebuildTable widens column colIdx to toType, preserving all rows already in
// the table.
func (db *DB) rebuildTable(ctx context.Context, table *importTable, colIdx int, toType string) error {
	fromType := table.columnTypes[colIdx]
	table.rebuilds++
	scratch := db.scratchTableName(table.tableName, table.rebuilds)

	newTypes := append([]string(nil), table.columnTypes...)
	newTypes[colIdx] = toType

	// DDL uses the driver's real storage names; the table keeps the inference
	// names in columnTypes.
	ddlTypes := make([]string, len(newTypes))
	for i, t := range newTypes {
		if canonicalType(t) == "" {
			ddlTypes[i] = t
			continue
		}
		ddlTypes[i] = db.dbTypeName(t)
	}

	create := db.queryCreateTableQuoted(scratch, table.columns, ddlTypes, table.isTemporary)
	debug.Print(create)
	if _, err := db.Tx.ExecContext(ctx, create); err != nil {
		return fmt.Errorf("promote create: %w", err)
	}

	copyQuery := db.queryCopyCast(scratch, table, colIdx, toType)
	debug.Print(copyQuery)
	if _, err := db.Tx.ExecContext(ctx, copyQuery); err != nil {
		return fmt.Errorf("promote copy: %w", err)
	}

	if _, err := db.Tx.ExecContext(ctx, "DROP TABLE "+table.tableName); err != nil {
		return fmt.Errorf("promote drop: %w", err)
	}
	if _, err := db.Tx.ExecContext(ctx, "ALTER TABLE "+scratch+" RENAME TO "+table.tableName); err != nil {
		return fmt.Errorf("promote rename: %w", err)
	}

	table.columnTypes = newTypes
	table.lastCount = 0
	// The promotion is reported on the error stream (through log) rather than
	// applied silently: the point of explicit promotion is that a column that
	// started as integer really became numeric, and the user should be able to
	// see when and why.
	log.Printf("trdsql: column %s promoted %s -> %s (earlier rows preserved)", table.columns[colIdx], fromType, toType)
	db.promotions = append(db.promotions, Promotion{
		Table:  table.tableName,
		Column: table.columns[colIdx],
		From:   fromType,
		To:     toType,
	})
	debug.Printf("promote: %s %s %s -> %s", table.tableName, table.columns[colIdx], fromType, toType)
	return nil
}

// queryCreateTableQuoted is queryCreateTable for already-quoted column names.
func (db *DB) queryCreateTableQuoted(tableName string, quotedNames []string, columnTypes []string, isTemporary bool) string {
	var buf strings.Builder
	if isTemporary {
		buf.WriteString("CREATE TEMPORARY TABLE ")
	} else {
		buf.WriteString("CREATE TABLE ")
	}
	buf.WriteString(tableName)
	buf.WriteString(" ( ")
	buf.WriteString(quotedNames[0])
	buf.WriteString(" ")
	buf.WriteString(columnTypes[0])
	for i, name := range quotedNames[1:] {
		buf.WriteString(", ")
		buf.WriteString(name)
		buf.WriteString(" ")
		buf.WriteString(columnTypes[i+1])
	}
	buf.WriteString(" );")
	return buf.String()
}

// queryCopyCast builds the INSERT ... SELECT that carries the existing rows
// into the rebuilt table. Only the promoted column is cast, and it is cast
// explicitly so that no driver has to guess how to convert it. Every other
// column is copied as-is: its declared type is unchanged, its values were
// already accepted under that declaration, and casting a pass-through type
// such as a TBLN "date" would corrupt it on SQLite, whose CAST applies
// NUMERIC affinity to unrecognized type names.
func (db *DB) queryCopyCast(scratch string, table *importTable, colIdx int, toType string) string {
	var buf strings.Builder
	buf.WriteString("INSERT INTO ")
	buf.WriteString(scratch)
	buf.WriteString(" (")
	buf.WriteString(table.columns[0])
	for _, column := range table.columns[1:] {
		buf.WriteString(", ")
		buf.WriteString(column)
	}
	buf.WriteString(") SELECT ")
	buf.WriteString(db.copySelectExpr(table.columns[0], 0 == colIdx, table.columnTypes[0], toType))
	for i := 1; i < len(table.columns); i++ {
		buf.WriteString(", ")
		buf.WriteString(db.copySelectExpr(table.columns[i], i == colIdx, table.columnTypes[i], toType))
	}
	buf.WriteString(" FROM ")
	buf.WriteString(table.tableName)
	buf.WriteString(";")
	return buf.String()
}

// copySelectExpr returns the SELECT expression for one column of a rebuild.
func (db *DB) copySelectExpr(column string, promoted bool, fromType string, toType string) string {
	if !promoted {
		return column
	}
	return db.castExpr(column, fromType, db.dbTypeName(toType))
}

// castExpr returns the SQL expression that converts column from fromType to
// the given target type. target is the driver's storage name (bigint,
// decimal(65,30), text, ...).
//
// MySQL spells casts differently from the SQL standard: it has no CAST to an
// arbitrary type name, only to SIGNED/UNSIGNED/DECIMAL/CHAR/DATE/... so the
// target name is mapped back onto MySQL's cast vocabulary. Every other driver
// (SQLite, PostgreSQL) accepts CAST(x AS <type>) directly.
func (db *DB) castExpr(column string, fromType string, target string) string {
	if db.driver == "mysql" {
		switch canonicalType(target) {
		case TypeInteger:
			return "CAST(" + column + " AS SIGNED)"
		case TypeNumeric:
			return "CAST(" + column + " AS DECIMAL(65,30))"
		default:
			// MySQL renders a DECIMAL with all 30 fractional digits, so an
			// earlier value of 20 would come out as 20.000...0 after a
			// promotion to text. Only a numeric source needs trimming.
			if canonicalType(fromType) == TypeNumeric {
				return "TRIM(TRAILING '.' FROM TRIM(TRAILING '0' FROM CAST(" + column + " AS CHAR)))"
			}
			return "CAST(" + column + " AS CHAR)"
		}
	}
	return "CAST(" + column + " AS " + target + ")"
}

// identifierLimit returns the maximum length, in bytes, that the target
// database allows for an identifier, or 0 when it allows arbitrary lengths.
func (db *DB) identifierLimit() int {
	switch db.driver {
	case "postgres":
		return 63
	case "mysql":
		return 64
	default:
		return 0
	}
}

// scratchTableName returns a unique quoted name for a rebuild scratch table.
//
// The suffix is short, but a plain source name + suffix is not enough on
// PostgreSQL: the server truncates identifiers to 63 bytes, so a long source
// name and its suffixed form would both truncate to the *same* name and the
// CREATE would collide with the table being rebuilt. The base name is
// therefore shortened first, so that the suffixed result fits the driver's
// identifier limit and stays distinct from the truncated source name.
func (db *DB) scratchTableName(tableName string, n int) string {
	suffix := "_p" + strconv.Itoa(n)
	quote := db.quote

	base := tableName
	if strings.HasPrefix(base, quote) && strings.HasSuffix(base, quote) && len(base) >= 2*len(quote) {
		base = base[len(quote) : len(base)-len(quote)]
	}

	if limit := db.identifierLimit(); limit > 0 {
		if room := limit - len(suffix); room <= 0 {
			base = ""
		} else if len(base) > room {
			base = base[:room]
			// Do not split a multi-byte UTF-8 sequence: the server counts bytes.
			for len(base) > 0 && !utf8.ValidString(base) {
				base = base[:len(base)-1]
			}
		}
	}
	return db.QuotedName(base + suffix)
}
