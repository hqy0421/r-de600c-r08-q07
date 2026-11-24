package trdsql

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestInferLiteralType(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"", TypeUnknown},
		{"0", TypeInteger},
		{"-1", TypeInteger},
		{"+42", TypeInteger},
		{"9223372036854775807", TypeInteger},
		{"9223372036854775808", TypeNumeric}, // overflows int64
		{"1.5", TypeNumeric},
		{"-0.5", TypeNumeric},
		{".5", TypeNumeric},
		{"1e10", TypeNumeric},
		{"1.2E-3", TypeNumeric},
		{"007", TypeText}, // leading zero identifier
		{"001.5", TypeText},
		{"abc", TypeText},
		{"NaN", TypeText},
		{"Inf", TypeText},
		{"0x10", TypeText},
		{"1 000", TypeText},
		{"1,5", TypeText},
		{"", TypeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := inferLiteralType(tt.value); got != tt.want {
				t.Errorf("inferLiteralType(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestPromoteType(t *testing.T) {
	tests := []struct {
		current string
		next    string
		want    string
	}{
		{TypeUnknown, TypeInteger, TypeInteger},
		{TypeUnknown, TypeNumeric, TypeNumeric},
		{TypeUnknown, TypeText, TypeText},
		{TypeInteger, TypeInteger, TypeInteger},
		{TypeInteger, TypeNumeric, TypeNumeric},
		{TypeInteger, TypeText, TypeText},
		{TypeNumeric, TypeInteger, TypeNumeric},
		{TypeNumeric, TypeNumeric, TypeNumeric},
		{TypeNumeric, TypeText, TypeText},
		{TypeText, TypeInteger, TypeText},
		{TypeText, TypeNumeric, TypeText},
		{"bigint", TypeNumeric, TypeNumeric},
		{"text", TypeInteger, "text"},
	}
	for _, tt := range tests {
		t.Run(tt.current+"+"+tt.next, func(t *testing.T) {
			if got := PromoteType(tt.current, tt.next); got != tt.want {
				t.Errorf("PromoteType(%q, %q) = %q, want %q", tt.current, tt.next, got, tt.want)
			}
		})
	}
}

func TestConvertForType(t *testing.T) {
	tests := []struct {
		value      any
		columnType string
		want       any
		ok         bool
	}{
		{"1", TypeInteger, int64(1), true},
		{"0", TypeInteger, int64(0), true},
		{"", TypeInteger, nil, true},
		{nil, TypeInteger, nil, true},
		{"1.5", TypeInteger, nil, false},
		{"abc", TypeInteger, nil, false},
		{"007", TypeInteger, nil, false},
		{"9223372036854775808", TypeInteger, nil, false},
		{"1.5", TypeNumeric, 1.5, true},
		{"1", TypeNumeric, 1.0, true},
		{"", TypeNumeric, nil, true},
		{"abc", TypeNumeric, nil, false},
		{"007", TypeNumeric, nil, false},
		{"abc", TypeText, "abc", true},
		{"1", TypeText, "1", true},
		{int64(5), TypeInteger, int64(5), true},
		{2.5, TypeNumeric, 2.5, true},
		// A type outside the inference family is passed through untouched.
		{"x", "date", "x", true},
	}
	for _, tt := range tests {
		t.Run(tt.columnType+"/"+ValString(tt.value), func(t *testing.T) {
			got, ok := convertForType(tt.value, tt.columnType)
			if ok != tt.ok {
				t.Fatalf("convertForType() ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("convertForType() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestInferColumnTypesFromRows(t *testing.T) {
	rows := [][]any{
		{"1", "10", "ok", ""},
		{"2", "20.5", "ok", ""},
	}
	got := inferColumnTypesFromRows(rows, 4)
	want := []string{TypeInteger, TypeNumeric, TypeText, DefaultDBType}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("inferColumnTypesFromRows() = %v, want %v", got, want)
	}
}

// partialReader returns a Reader that reports text types but not all rows at
// once, to exercise the wrapping behavior.
type partialReader struct {
	names []string
	pre   [][]any
	rest  [][]any
}

func (r *partialReader) Names() ([]string, error) { return r.names, nil }
func (r *partialReader) Types() ([]string, error) {
	types := make([]string, len(r.names))
	for i := range types {
		types[i] = DefaultDBType
	}
	return types, nil
}
func (r *partialReader) PreReadRow() [][]any { return r.pre }
func (r *partialReader) ReadRow([]any) ([]any, error) {
	if len(r.rest) == 0 {
		return nil, io.EOF
	}
	row := r.rest[0]
	r.rest = r.rest[1:]
	return row, nil
}

func TestInferReaderTypes(t *testing.T) {
	r := &partialReader{
		names: []string{"id", "qc"},
		pre:   [][]any{{"1", "10"}, {"2", "20"}},
	}
	ir := newInferReader(r)
	types, err := ir.Types()
	if err != nil {
		t.Fatal(err)
	}
	if types[0] != TypeInteger || types[1] != TypeInteger {
		t.Errorf("inferReader.Types() = %v, want both integer", types)
	}
	// PreReadRow is stable across calls.
	if len(ir.PreReadRow()) != 2 || len(ir.PreReadRow()) != 2 {
		t.Errorf("inferReader.PreReadRow() is not stable")
	}
}

func TestInferReaderSamplesWhenPreReadEmpty(t *testing.T) {
	// An empty pre-read window (a CSV header with the default -ir 1) must not
	// leave every column text: inference reads one row ahead and replays it.
	r := &partialReader{
		names: []string{"id", "qc"},
		pre:   nil,
		rest:  [][]any{{"1", "10"}, {"2", "20"}},
	}
	ir := newInferReader(r)
	types, err := ir.Types()
	if err != nil {
		t.Fatal(err)
	}
	if types[0] != TypeInteger || types[1] != TypeInteger {
		t.Errorf("inferReader.Types() = %v, want both integer from the sampled row", types)
	}
	// The sampled row is replayed, not swallowed.
	pre := ir.PreReadRow()
	if len(pre) != 1 || pre[0][0] != "1" {
		t.Errorf("PreReadRow() = %v, want the sampled first row", pre)
	}
	// The reader is positioned after the sampled row.
	row, err := ir.ReadRow(make([]any, 2))
	if err != nil {
		t.Fatal(err)
	}
	if row[0] != "2" {
		t.Errorf("ReadRow() = %v, want the second data row", row)
	}
}

func TestInferReaderEmptyInput(t *testing.T) {
	// No rows at all: nothing to go on, and no error beyond io.EOF.
	r := &partialReader{names: []string{"c1"}}
	ir := newInferReader(r)
	if _, err := ir.Types(); err != nil {
		t.Fatalf("Types() error = %v", err)
	}
	if got := ir.PreReadRow(); len(got) != 0 {
		t.Errorf("PreReadRow() = %v, want empty", got)
	}
}

func TestTypeInferencePromotesLaterDecimal(t *testing.T) {
	// The QC column is all integers in the pre-read window, then a decimal
	// appears. The column must end up numeric, earlier rows intact.
	data := "id,qc\n1,10\n2,20\n3,30.5\n4,40\n"
	reader, err := NewCSVReader(strings.NewReader(data), NewReadOpts(InHeader(true), InPreRead(2)))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Connect("sqlite", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Tx, err = db.Begin(); err != nil {
		t.Fatal(err)
	}
	names, _ := reader.Names()
	types := []string{TypeInteger, TypeInteger}
	if err := db.CreateTable("qc", names, db.portableTypes(types), true); err != nil {
		t.Fatal(err)
	}
	if err := db.ImportTypedContext(t.Context(), "qc", names, types, true, reader); err != nil {
		t.Fatal(err)
	}

	promotions := db.Promotions()
	if len(promotions) != 1 {
		t.Fatalf("Promotions() = %v, want exactly one promotion", promotions)
	}
	if promotions[0].From != TypeInteger || promotions[0].To != TypeNumeric {
		t.Errorf("promotion = %+v, want integer -> numeric", promotions[0])
	}

	rows, err := db.Tx.Query("SELECT id, qc FROM qc ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int64
		var qc float64
		if err := rows.Scan(&id, &qc); err != nil {
			t.Fatal(err)
		}
		got = append(got, ValString(id)+"="+ValString(qc))
	}
	want := []string{"1=10", "2=20", "3=30.5", "4=40"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rows = %v, want %v", got, want)
	}

	// The declared type must report numeric, not integer.
	ctype, err := db.Tx.Query("SELECT qc FROM qc")
	if err != nil {
		t.Fatal(err)
	}
	defer ctype.Close()
	cts, err := ctype.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ToLower(cts[0].DatabaseTypeName()); got != "numeric" {
		t.Errorf("declared qc type = %q, want numeric", got)
	}
}

func TestTypeInferenceNonNumericPromotesToText(t *testing.T) {
	data := "id,qc\n1,10\n2,oops\n"
	reader, err := NewCSVReader(strings.NewReader(data), NewReadOpts(InHeader(true), InPreRead(1)))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Connect("sqlite", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Tx, err = db.Begin(); err != nil {
		t.Fatal(err)
	}
	names, _ := reader.Names()
	types := []string{TypeInteger, TypeInteger}
	if err := db.CreateTable("t", names, types, true); err != nil {
		t.Fatal(err)
	}
	if err := db.ImportTypedContext(t.Context(), "t", names, types, true, reader); err != nil {
		t.Fatal(err)
	}
	p := db.Promotions()
	if len(p) != 1 || p[0].To != TypeText {
		t.Fatalf("Promotions() = %v, want one promotion to text", p)
	}
	var qc string
	if err := db.Tx.QueryRow("SELECT qc FROM t WHERE id = 1").Scan(&qc); err != nil {
		t.Fatal(err)
	}
	if qc != "10" {
		t.Errorf("qc = %q, want %q (integer kept as text, not converted to 10.0)", qc, "10")
	}
}

func TestTypeInferenceEndToEnd(t *testing.T) {
	out := new(bytes.Buffer)
	importer := NewImporter(InFormat(CSV), InHeader(true), InPreRead(2), InTypeInfer(true))
	exporter := NewExporter(NewWriter(OutFormat(TBLN), OutStream(out)))
	trd := NewTRDSQL(importer, exporter)

	fileName := filepath.Join(dataDir, "header.csv")
	if err := trd.Exec("SELECT * FROM " + fileName); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"; type: | int | text |", "| 1 | Orange |"} {
		if !strings.Contains(got, want) {
			t.Errorf("TBLN output missing %q in:\n%s", want, got)
		}
	}
}

func TestTypeInferenceDisabledByDefault(t *testing.T) {
	out := new(bytes.Buffer)
	importer := NewImporter(InFormat(CSV), InHeader(true), InPreRead(2))
	exporter := NewExporter(NewWriter(OutFormat(TBLN), OutStream(out)))
	trd := NewTRDSQL(importer, exporter)

	if err := trd.Exec("SELECT * FROM " + filepath.Join(dataDir, "header.csv")); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "; type: | text | text |") {
		t.Errorf("default TBLN output changed, want text types:\n%s", got)
	}
}

func TestAnalyzeReportsInferredTypes(t *testing.T) {
	var buf bytes.Buffer
	opts := &AnalyzeOpts{Command: AppName, Quote: "`", Detail: true, OutStream: &buf}
	readOpts := NewReadOpts(InFormat(CSV), InHeader(true), InPreRead(2), InTypeInfer(true))
	if err := Analyze(filepath.Join(dataDir, "header.csv"), opts, readOpts); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "integer") {
		t.Errorf("Analyze did not report the inferred integer type:\n%s", got)
	}
}

func TestCanonicalType(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"INTEGER", TypeInteger},
		{"bigint", TypeInteger},
		{"int8", TypeInteger},
		{"numeric", TypeNumeric},
		{"decimal(65,30)", TypeNumeric},
		{"double precision", TypeNumeric},
		{"varchar(10)", TypeText},
		{"TEXT", TypeText},
		{"date", ""},
		{"bool", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := canonicalType(tt.in); got != tt.want {
				t.Errorf("canonicalType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDBTypeName(t *testing.T) {
	tests := []struct {
		driver string
		in     string
		want   string
	}{
		{"sqlite", TypeInteger, "integer"},
		{"sqlite", TypeNumeric, "numeric"},
		{"postgres", TypeInteger, "bigint"},
		{"postgres", TypeNumeric, "numeric"},
		{"mysql", TypeInteger, "bigint"},
		{"mysql", TypeNumeric, "decimal(65,30)"},
		{"postgres", "date", "date"},
	}
	for _, tt := range tests {
		t.Run(tt.driver+"/"+tt.in, func(t *testing.T) {
			db := &DB{driver: tt.driver}
			if got := db.dbTypeName(tt.in); got != tt.want {
				t.Errorf("dbTypeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDBQueryCopyCast(t *testing.T) {
	db := &DB{driver: "sqlite", quote: "`"}
	table := &importTable{
		tableName:   "`t`",
		columns:     []string{"`a`", "`b`"},
		columnTypes: []string{TypeInteger, TypeText},
	}
	// Only the promoted column is cast; the untouched one is copied as-is.
	got := db.queryCopyCast("`t_p1`", table, 0, TypeNumeric)
	want := "INSERT INTO `t_p1` (`a`, `b`) SELECT CAST(`a` AS numeric), `b` FROM `t`;"
	if got != want {
		t.Errorf("queryCopyCast() =\n%q\nwant\n%q", got, want)
	}

	// A pass-through type (TBLN date) in another column stays uncast even
	// while a neighbour is promoted.
	table.columnTypes = []string{TypeText, "date"}
	got = db.queryCopyCast("`t_p1`", table, 0, TypeNumeric)
	want = "INSERT INTO `t_p1` (`a`, `b`) SELECT CAST(`a` AS numeric), `b` FROM `t`;"
	if got != want {
		t.Errorf("queryCopyCast() with pass-through =\n%q\nwant\n%q", got, want)
	}

	mysql := &DB{driver: "mysql", quote: "`"}
	table.columnTypes = []string{TypeInteger, TypeText}
	got = mysql.queryCopyCast("`t_p1`", table, 1, TypeInteger)
	want = "INSERT INTO `t_p1` (`a`, `b`) SELECT `a`, CAST(`b` AS SIGNED) FROM `t`;"
	if got != want {
		t.Errorf("mysql queryCopyCast() =\n%q\nwant\n%q", got, want)
	}

	// MySQL pads DECIMAL(65,30) values, so a numeric -> text promotion trims
	// the padding instead of turning 20 into 20.000...0.
	table.columnTypes = []string{TypeNumeric, TypeText}
	got = mysql.queryCopyCast("`t_p1`", table, 0, TypeText)
	want = "INSERT INTO `t_p1` (`a`, `b`) SELECT TRIM(TRAILING '.' FROM TRIM(TRAILING '0' FROM CAST(`a` AS CHAR))), `b` FROM `t`;"
	if got != want {
		t.Errorf("mysql numeric->text queryCopyCast() =\n%q\nwant\n%q", got, want)
	}
}

func TestScratchTableName(t *testing.T) {
	sqlite := &DB{driver: "sqlite", quote: "`"}
	if got := sqlite.scratchTableName("`t`", 1); got != "`t_p1`" {
		t.Errorf("sqlite scratch = %q, want `t_p1`", got)
	}
	// Unquoted input is quoted too.
	if got := sqlite.scratchTableName("t", 2); got != "`t_p2`" {
		t.Errorf("sqlite scratch unquoted = %q, want `t_p2`", got)
	}

	// PostgreSQL truncates identifiers to 63 bytes. A long name must not
	// truncate to the same identifier as the table it is replacing.
	pg := &DB{driver: "postgres", quote: `"`}
	long := strings.Repeat("abcdefghij", 9) // 90 bytes
	scratch := pg.scratchTableName(`"`+long+`"`, 3)
	base := strings.Trim(scratch, `"`)
	if len(base) > 63 {
		t.Errorf("postgres scratch name is %d bytes, want <= 63: %q", len(base), base)
	}
	if base == long[:63] {
		t.Errorf("scratch name collides with truncated source: %q", base)
	}
	if !strings.HasSuffix(base, "_p3") {
		t.Errorf("scratch name lost its suffix: %q", base)
	}

	// The truncation must not split a multi-byte character.
	utf8Name := strings.Repeat("日", 30) // 90 bytes
	scratch = pg.scratchTableName(`"`+utf8Name+`"`, 1)
	if !utf8.ValidString(strings.Trim(scratch, `"`)) {
		t.Errorf("scratch name split a UTF-8 sequence: %q", scratch)
	}
	if len(strings.Trim(scratch, `"`)) > 63 {
		t.Errorf("utf8 scratch name exceeds 63 bytes: %q", scratch)
	}
}

func TestSliceImporterWithTypeInfer(t *testing.T) {
	data := [][]any{
		{int64(1), int64(10), "ok"},
		{int64(2), 20.5, "ok"},
	}
	db, err := Connect(DefaultDriver, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Tx, err = db.Begin(); err != nil {
		t.Fatal(err)
	}
	i := NewSliceImporterWithTypeInfer("s", data)
	if _, err := i.Import(db, ""); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Tx.Query("SELECT c1, c2, c3 FROM s ORDER BY c1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	gotTypes := make([]string, len(cts))
	for i, ct := range cts {
		gotTypes[i] = strings.ToLower(ct.DatabaseTypeName())
	}
	wantTypes := []string{"integer", "numeric", "text"}
	if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
		t.Errorf("slice declared types = %v, want %v", gotTypes, wantTypes)
	}

	var n int
	for rows.Next() {
		var a int64
		var b float64
		var c string
		if err := rows.Scan(&a, &b, &c); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("imported %d rows, want 2", n)
	}
}
