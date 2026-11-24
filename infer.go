package trdsql

// Column type inference and promotion.
//
// Type inference assigns a column the narrowest type that can represent every
// value observed for that column. Observable types are ordered by a fixed
// widening sequence:
//
//	unknown (empty/NULL) -> integer -> numeric -> text
//
// A type only ever moves to the right. A column first seen as integer can be
// promoted to numeric or text by a later value, but a numeric or text column
// is never narrowed again. Promotion is monotonic, so every value that fitted
// a column before a promotion still fits it afterwards.
//
// Promotion is applied in two places and both are explicit:
//
//   - Inference (opt-in with InTypeInfer) decides the column types used for
//     CREATE TABLE from the pre-read rows.
//   - The importer converts every value to the Go type of its column before
//     sending it to the database. When a value does not fit the declared
//     column type, the column is promoted and the table is rebuilt with the
//     widened declaration (see DB.rebuildTable). Rows imported before the
//     promotion are carried over, so no data is lost.
//
// trdsql never relies on the database driver or on SQLite type affinity to
// coerce a value into the declared type: a decimal in an integer column is
// detected in Go and turns into an explicit promotion instead of a silently
// mismatched declaration.

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Column type names produced by inference and used in promotion DDL.
// They are valid in SQLite, MySQL and PostgreSQL.
const (
	// TypeUnknown is the type of a column that no value has constrained yet.
	// It is used internally and never becomes a declared column type.
	TypeUnknown = ""

	// TypeInteger is a signed 64-bit integer column.
	TypeInteger = "integer"

	// TypeNumeric is a decimal/floating point column. It is also the target
	// when an integer literal does not fit in int64.
	TypeNumeric = "numeric"

	// TypeText is the widest type. Every value fits in a text column.
	TypeText = "text"
)

// integerLiteral matches a signed decimal integer with no digit decoration.
// Leading zeros pass the pattern and are rejected by isIntegerLiteral.
var integerLiteral = regexp.MustCompile(`^[+-]?[0-9]+$`)

// decimalLiteral matches a plain decimal literal: optional sign, integer part,
// optional fraction, optional exponent. Hexadecimal floats, "NaN", "Inf" and
// digit separators are rejected on purpose, because they are not
// unambiguously numeric in a tabular text file.
var decimalLiteral = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// canonicalType maps a declared SQL type name to one of the inference types.
// It returns "" for types outside the inference family (for example a date or
// a boolean declared by a TBLN definition). Those types are passed through
// untouched: trdsql does not try to promote or convert them.
//
// Type parameters are ignored, so "decimal(65,30)" and "varchar(10)" resolve
// like "decimal" and "varchar".
func canonicalType(columnType string) string {
	name := strings.ToLower(strings.TrimSpace(columnType))
	if i := strings.IndexByte(name, '('); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	// Collapse runs of whitespace so that "character  varying" still matches.
	name = strings.Join(strings.Fields(name), " ")
	switch name {
	case "int", "integer", "int2", "int4", "int8", "mediumint", "smallint", "bigint", "tinyint",
		"serial", "smallserial", "bigserial":
		return TypeInteger
	case "numeric", "decimal", "dec", "real", "double", "double precision", "float", "float4", "float8":
		return TypeNumeric
	case "text", "string", "char", "varchar", "character", "character varying", "nchar", "nvarchar",
		"clob", "tinytext", "mediumtext", "longtext":
		return TypeText
	default:
		return ""
	}
}

// typeRank returns the position of a declared type in the widening order.
// Types outside the inference family rank with text: they are never narrowed
// and never promoted by inference.
func typeRank(columnType string) int {
	switch canonicalType(columnType) {
	case TypeUnknown:
		return 0
	case TypeInteger:
		return 1
	case TypeNumeric:
		return 2
	default:
		return 3
	}
}

// PromoteType returns the wider of two column types.
// Promotion is one-way: the result never ranks below either argument.
func PromoteType(current string, next string) string {
	if typeRank(next) > typeRank(current) {
		return next
	}
	return current
}

// hasLeadingZero reports whether the unsigned integer digits carry a leading
// zero ("0" alone is fine, "007" is not).
func hasLeadingZero(digits string) bool {
	return len(digits) > 1 && digits[0] == '0'
}

// isIntegerLiteral reports whether s is a plain integer.
// Integers with leading zeros are rejected so that identifiers such as zip
// codes and fixed-width ids keep their zeros and stay text.
func isIntegerLiteral(s string) bool {
	if !integerLiteral.MatchString(s) {
		return false
	}
	return !hasLeadingZero(strings.TrimLeft(s, "+-"))
}

// isDecimalLiteral reports whether s is a plain decimal.
func isDecimalLiteral(s string) bool {
	if !decimalLiteral.MatchString(s) {
		return false
	}
	// Check the leading zero rule on the integer part only.
	unsigned := strings.TrimLeft(s, "+-")
	intPart := unsigned
	if i := strings.IndexAny(unsigned, ".eE"); i >= 0 {
		intPart = unsigned[:i]
	}
	return !hasLeadingZero(intPart)
}

// typeValueString converts a reader value to the string used for inference
// and conversion. The second return value is false only for nil (SQL NULL).
func typeValueString(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case []byte:
		return string(t), true
	default:
		return ValString(t), true
	}
}

// inferLiteralType returns the narrowest type for a single text value.
// nil, empty strings and leading-zero identifiers do not constrain a column
// to a numeric type.
func inferLiteralType(s string) string {
	if s == "" {
		return TypeUnknown
	}
	if isIntegerLiteral(s) {
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return TypeInteger
		}
		// An integer literal too large for int64 is a valid number, but it
		// cannot be stored exactly as an integer. Treat it as numeric.
		return TypeNumeric
	}
	if isDecimalLiteral(s) {
		return TypeNumeric
	}
	return TypeText
}

// InferValueType returns the narrowest column type that can represent v.
// nil and empty strings do not constrain a column.
func InferValueType(v any) string {
	s, ok := typeValueString(v)
	if !ok {
		return TypeUnknown
	}
	return inferLiteralType(s)
}

// convertForType converts a value to the Go representation of columnType.
// It reports false when the value does not fit columnType; the caller must
// promote the column instead of letting the database coerce the value.
//
// nil stays NULL. An empty string in an integer or numeric column becomes
// NULL: an empty field carries no number, and storing it as text inside a
// numeric column would corrupt the declared type without a visible error.
//
// A column type outside the inference family is returned unchanged.
func convertForType(v any, columnType string) (any, bool) {
	s, ok := typeValueString(v)
	if !ok {
		return nil, true
	}
	switch canonicalType(columnType) {
	case TypeInteger:
		if s == "" {
			return nil, true
		}
		if !isIntegerLiteral(s) {
			return nil, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		return n, true
	case TypeNumeric:
		if s == "" {
			return nil, true
		}
		if !isDecimalLiteral(s) {
			return nil, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, false
		}
		return f, true
	case TypeText:
		return s, true
	default:
		return v, true
	}
}

// promoteForValue returns the column type needed to hold v.
// It is a no-op for column types outside the inference family.
func promoteForValue(current string, v any) string {
	if current != TypeUnknown && canonicalType(current) == "" {
		return current
	}
	return PromoteType(current, InferValueType(v))
}

// inferColumnTypesFromRows infers column types from pre-read rows.
// Columns that no value constrained are left as DefaultDBType ("text").
// A short row is treated as having no value for the columns it lacks.
func inferColumnTypesFromRows(rows [][]any, columnNum int) []string {
	types := make([]string, columnNum)
	for _, row := range rows {
		for i := 0; i < columnNum && i < len(row); i++ {
			types[i] = promoteForValue(types[i], row[i])
		}
	}
	for i := range types {
		if types[i] == TypeUnknown {
			types[i] = DefaultDBType
		}
	}
	return types
}

// TypeDeclarer is optionally implemented by a Reader whose Types are declared
// by the source data (a TBLN definition, Go values) instead of defaulting
// every column to DefaultDBType.
//
// When a Reader implements it and returns true, type inference keeps those
// declarations: they are authoritative. Other readers are inferred from their
// pre-read values.
type TypeDeclarer interface {
	DeclaresTypes() bool
}

// inferSampleRows is the minimum number of data rows type inference must see.
//
// The pre-read window can contain no data rows at all: with a CSV header and
// the default InPreRead of 1, the header consumes the single pre-read slot.
// Inferring from an empty window would declare every column text and make
// InTypeInfer a silent no-op, so inference reads ahead until it has seen at
// least this many rows. The rows it reads are cached and replayed through
// PreReadRow: they are still imported, never skipped.
const inferSampleRows = 1

// inferReader adds optional column type inference to a Reader.
//
// It materializes the pre-read rows once, infers the column types from them
// and keeps the rows so that PreReadRow can be called any number of times.
// ReadRow is passed through unchanged: values that do not fit a column are
// promoted by the importer, which converts values when InTypeInfer is set.
type inferReader struct {
	Reader
	once  sync.Once
	types []string
	pre   [][]any
}

// newInferReader wraps r with type inference.
func newInferReader(r Reader) Reader {
	return &inferReader{Reader: r}
}

// load reads the pre-read rows once and infers the column types.
func (r *inferReader) load() {
	r.pre = r.Reader.PreReadRow()

	names, _ := r.Reader.Names()
	columnNum := len(names)
	if columnNum == 0 && len(r.pre) > 0 {
		columnNum = len(r.pre[0])
	}

	if d, ok := r.Reader.(TypeDeclarer); ok && d.DeclaresTypes() {
		if declared, err := r.Reader.Types(); err == nil && len(declared) == columnNum {
			r.types = declared
			return
		}
	}

	// Guarantee a sample even when the pre-read window held nothing but the
	// header. The rows read here are appended to pre and replayed to the
	// importer, so they are not consumed twice or lost.
	for columnNum > 0 && len(r.pre) < inferSampleRows {
		row := make([]any, columnNum)
		row, err := r.Reader.ReadRow(row)
		if err != nil {
			break // io.EOF (or a read error) means there is nothing left to sample.
		}
		if len(row) == 0 {
			continue
		}
		r.pre = append(r.pre, row)
	}

	r.types = inferColumnTypesFromRows(r.pre, columnNum)
}

// Types returns the inferred column types.
func (r *inferReader) Types() ([]string, error) {
	r.once.Do(r.load)
	if len(r.types) == 0 {
		return r.types, ErrNoRows
	}
	return r.types, nil
}

// PreReadRow returns the cached pre-read rows.
func (r *inferReader) PreReadRow() [][]any {
	r.once.Do(r.load)
	return r.pre
}
