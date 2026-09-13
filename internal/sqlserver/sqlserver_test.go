package sqlserver

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"godbsync/internal/dialect"
	"godbsync/internal/types"
)

// ---- Normalize ----

func TestNormalize_BasicTypes(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
	}{
		{"nil", nil},
		{"int", int(1)},
		{"int64", int64(1)},
		{"float64", float64(1.5)},
		{"string", "hello"},
		{"bool", true},
		{"time", time.Now()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeSQLServer(tt.input, "")
			if !reflect.DeepEqual(got, tt.input) {
				t.Errorf("normalizeSQLServer(%v) = %v (%T), want equal", tt.input, got, got)
			}
		})
	}
}

func TestNormalize_ByteToString(t *testing.T) {
	got := normalizeSQLServer([]byte("hello"), "")
	if got != "hello" {
		t.Errorf("normalizeSQLServer([]byte) = %v (%T), want \"hello\" string", got, got)
	}
}

func TestNormalize_CustomTypeToString(t *testing.T) {
	type customType struct{}
	got := normalizeSQLServer(customType{}, "")
	s, ok := got.(string)
	if !ok {
		t.Errorf("normalizeSQLServer(customType) 应返回 string, got %T", got)
	}
	if s == "" {
		t.Error("字符串表示不应为空")
	}
}

// TestNormalize_UniqueIdentifierToGUID 验证 SQL Server uniqueidentifier 的 16 字节
// []byte 被规整为规范 GUID 字符串（写入 MySQL VARCHAR(36) 不会损坏）。
func TestNormalize_UniqueIdentifierToGUID(t *testing.T) {
	// 选取一组 16 字节，校验输出为 8-4-4-4-12 规范形式。
	b := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF, 0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF}
	got := normalizeSQLServer(b, "UNIQUEIDENTIFIER")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("uniqueidentifier 应转为 string, got %T", got)
	}
	want := "01234567-89AB-CDEF-0123-456789ABCDEF"
	if s != want {
		t.Errorf("GUID 字符串不正确: got %q, want %q", s, want)
	}

	// 非 uniqueidentifier 的 []byte 不应被当 GUID（避免破坏 VARBINARY）。
	got2 := normalizeSQLServer(b, "VARBINARY")
	if got2 != string(b) {
		t.Errorf("非 uniqueidentifier 的 []byte 应保持原样, got %v", got2)
	}
}

// ---- IsTimeType / IsNumericType ----

func TestIsTimeType(t *testing.T) {
	for _, typ := range []string{"DATE", "DATETIME", "DATETIME2", "SMALLDATETIME", "DATETIMEOFFSET", "TIME"} {
		if !isSQLServerTimeType(typ) {
			t.Errorf("%s 应被识别为时间类型", typ)
		}
	}
	for _, typ := range []string{"INT", "VARCHAR", "BIT"} {
		if isSQLServerTimeType(typ) {
			t.Errorf("%s 不应被识别为时间类型", typ)
		}
	}
}

func TestIsNumericType(t *testing.T) {
	for _, typ := range []string{"INT", "BIGINT", "SMALLINT", "TINYINT", "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY", "FLOAT", "REAL", "BIT"} {
		if !isSQLServerNumericType(typ) {
			t.Errorf("%s 应被识别为数值类型", typ)
		}
	}
	for _, typ := range []string{"VARCHAR", "DATE", "TEXT"} {
		if isSQLServerNumericType(typ) {
			t.Errorf("%s 不应被识别为数值类型", typ)
		}
	}
}

// ---- Reader ----

func TestReader_Dialect(t *testing.T) {
	var r dialect.Reader = &Reader{}
	if r.Dialect() != "sqlserver" {
		t.Errorf("Dialect 应为 sqlserver, got %q", r.Dialect())
	}
}

func TestReader_Registered(t *testing.T) {
	r, err := dialect.NewReader("sqlserver")
	if err != nil {
		t.Fatalf("NewReader(sqlserver) 应成功注册, got %v", err)
	}
	if r.Dialect() != "sqlserver" {
		t.Errorf("注册实例 Dialect 应为 sqlserver, got %q", r.Dialect())
	}
}

func TestReader_Placeholder(t *testing.T) {
	r := &Reader{}
	if got := r.Placeholder(1); got != "@p1" {
		t.Errorf("Placeholder(1) = %q, want @p1", got)
	}
	if got := r.Placeholder(5); got != "@p5" {
		t.Errorf("Placeholder(5) = %q, want @p5", got)
	}
}

func TestReader_QuoteIdentifier(t *testing.T) {
	r := &Reader{}
	if got := r.QuoteIdentifier("col"); got != "[col]" {
		t.Errorf("QuoteIdentifier = %q, want [col]", got)
	}
}

func TestReader_SupportsRowID(t *testing.T) {
	r := &Reader{}
	if r.SupportsRowID() {
		t.Error("SQL Server 不应支持 ROWID")
	}
}

func TestReader_NormalizeByteToString(t *testing.T) {
	r := &Reader{}
	got := r.Normalize([]byte("test"), "")
	if got != "test" {
		t.Errorf("Normalize([]byte) = %v, want \"test\"", got)
	}
	got2 := r.Normalize("already-string", "")
	if got2 != "already-string" {
		t.Errorf("Normalize(string) = %v, want unchanged", got2)
	}
}

// ---- Writer ----

func TestWriter_Dialect(t *testing.T) {
	var w dialect.Writer = &Writer{}
	if w.Dialect() != "sqlserver" {
		t.Errorf("Dialect 应为 sqlserver, got %q", w.Dialect())
	}
}

func TestWriter_Registered(t *testing.T) {
	w, err := dialect.NewWriter("sqlserver")
	if err != nil {
		t.Fatalf("NewWriter(sqlserver) 应成功注册, got %v", err)
	}
	if w.Dialect() != "sqlserver" {
		t.Errorf("注册实例 Dialect 应为 sqlserver, got %q", w.Dialect())
	}
}

func TestWriter_Placeholder(t *testing.T) {
	w := &Writer{}
	if got := w.Placeholder(1); got != "@p1" {
		t.Errorf("Placeholder(1) = %q, want @p1", got)
	}
	if got := w.Placeholder(3); got != "@p3" {
		t.Errorf("Placeholder(3) = %q, want @p3", got)
	}
}

func TestWriter_QuoteIdentifier(t *testing.T) {
	w := &Writer{}
	if got := w.QuoteIdentifier("col"); got != "[col]" {
		t.Errorf("QuoteIdentifier = %q, want [col]", got)
	}
}

// ---- Type mapping (sqlserverType: MySQL source -> SQL Server target) ----

func TestSQLServerType_Mapping(t *testing.T) {
	tests := []struct {
		col  types.Column
		want string
	}{
		{types.Column{DBType: "VARCHAR", Length: 100}, "NVARCHAR(100)"},
		{types.Column{DBType: "VARCHAR", Length: 5000}, "NVARCHAR(MAX)"},
		{types.Column{DBType: "VARCHAR", Length: 0}, "NVARCHAR(4000)"},
		{types.Column{DBType: "INT"}, "INT"},
		{types.Column{DBType: "BIGINT"}, "BIGINT"},
		{types.Column{DBType: "SMALLINT"}, "SMALLINT"},
		{types.Column{DBType: "TINYINT", Length: 1}, "BIT"},
		{types.Column{DBType: "TINYINT", Length: 0}, "SMALLINT"},
		{types.Column{DBType: "DECIMAL", Precision: 10, Scale: 2}, "DECIMAL(10,2)"},
		{types.Column{DBType: "DOUBLE"}, "FLOAT(53)"},
		{types.Column{DBType: "FLOAT"}, "REAL"},
		{types.Column{DBType: "DATETIME"}, "DATETIME2"},
		{types.Column{DBType: "TEXT"}, "NVARCHAR(MAX)"},
		{types.Column{DBType: "BLOB"}, "VARBINARY(MAX)"},
		{types.Column{DBType: "JSON"}, "NVARCHAR(MAX)"},
		{types.Column{DBType: "BOOLEAN"}, "BIT"},
		{types.Column{DBType: "ENUM"}, "NVARCHAR(255)"},
		{types.Column{DBType: "YEAR"}, "SMALLINT"},
		{types.Column{DBType: "UNIQUEIDENTIFIER"}, "UNIQUEIDENTIFIER"},
	}
	for _, tt := range tests {
		got, err := sqlserverType(tt.col)
		if err != nil {
			t.Fatalf("sqlserverType(%+v) error: %v", tt.col, err)
		}
		if got != tt.want {
			t.Errorf("sqlserverType(%+v) = %q, want %q", tt.col, got, tt.want)
		}
	}
}

func TestSQLServerType_Unknown(t *testing.T) {
	_, err := sqlserverType(types.Column{DBType: "UNKNOWN_TYPE", Name: "x"})
	if err == nil {
		t.Error("未知类型应返回错误")
	}
}

// ---- GenerateCreate ----

func TestSQLServerGenerateCreate_Basic(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "INT", Nullable: false},
		{Name: "name", DBType: "VARCHAR", Length: 100, Nullable: true},
	}
	pk := []string{"id"}
	got, err := sqlserverGenerateCreate(nil, "users", cols, pk)
	if err != nil {
		t.Fatalf("sqlserverGenerateCreate error: %v", err)
	}
	if !strings.Contains(got, "CREATE TABLE [users]") {
		t.Errorf("缺少 CREATE TABLE [users], got:\n%s", got)
	}
	if !strings.Contains(got, "[id] INT NOT NULL") {
		t.Errorf("缺少 [id] INT NOT NULL, got:\n%s", got)
	}
	if !strings.Contains(got, "[name] NVARCHAR(100)") {
		t.Errorf("缺少 [name] NVARCHAR(100), got:\n%s", got)
	}
	if !strings.Contains(got, "PRIMARY KEY ([id])") {
		t.Errorf("缺少 PRIMARY KEY, got:\n%s", got)
	}
}

func TestSQLServerGenerateCreate_WithSchema(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "INT", Nullable: false},
	}
	got, err := sqlserverGenerateCreate(nil, "dbo.users", cols, []string{"id"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(got, "CREATE TABLE [dbo].[users]") {
		t.Errorf("缺少 CREATE TABLE [dbo].[users], got:\n%s", got)
	}
}

func TestSQLServerGenerateCreate_EmptyCols(t *testing.T) {
	_, err := sqlserverGenerateCreate(nil, "t", nil, nil)
	if err == nil {
		t.Error("空列应返回错误")
	}
}

// ---- MERGE SQL construction ----

func TestBuildMergeSQL_WithPK(t *testing.T) {
	cols := []string{"id", "name", "email"}
	pk := []string{"id"}
	got := buildMergeSQL("users", cols, pk, 2)
	if !strings.Contains(got, "MERGE INTO [users] AS t") {
		t.Errorf("缺少 MERGE INTO, got:\n%s", got)
	}
	if !strings.Contains(got, "USING (VALUES (@p1,@p2,@p3), (@p4,@p5,@p6))") {
		t.Errorf("VALUES 子句不正确, got:\n%s", got)
	}
	if !strings.Contains(got, "ON (t.[id] = s.[id])") {
		t.Errorf("ON 子句不正确, got:\n%s", got)
	}
	if !strings.Contains(got, "WHEN MATCHED THEN UPDATE SET t.[name] = s.[name], t.[email] = s.[email]") {
		t.Errorf("UPDATE SET 子句不正确, got:\n%s", got)
	}
	if !strings.Contains(got, "WHEN NOT MATCHED THEN INSERT") {
		t.Errorf("缺少 INSERT 子句, got:\n%s", got)
	}
	if !strings.HasSuffix(got, ";") {
		t.Errorf("MERGE 应以分号结尾, got:\n%s", got)
	}
}

func TestBuildMergeSQL_OnlyPKColumns(t *testing.T) {
	cols := []string{"id"}
	pk := []string{"id"}
	got := buildMergeSQL("t", cols, pk, 1)
	// 只有主键列时，不应有 WHEN MATCHED THEN UPDATE
	if strings.Contains(got, "WHEN MATCHED THEN UPDATE") {
		t.Errorf("仅主键列时不应有 UPDATE 子句, got:\n%s", got)
	}
	if !strings.Contains(got, "WHEN NOT MATCHED THEN INSERT") {
		t.Errorf("仍应有 INSERT 子句, got:\n%s", got)
	}
}

func TestBuildInsertSQL_NoPK(t *testing.T) {
	cols := []string{"a", "b"}
	got := buildInsertSQL("log", cols, 2)
	if !strings.Contains(got, "INSERT INTO [log] ([a], [b]) VALUES (@p1,@p2), (@p3,@p4);") {
		t.Errorf("INSERT SQL 不正确, got:\n%s", got)
	}
}

// ---- rowValuePlaceholders ----

func TestRowValuePlaceholders(t *testing.T) {
	idx := 0
	if got := rowValuePlaceholders(1, &idx); got != "(@p1)" || idx != 1 {
		t.Errorf("rowValuePlaceholders(1) = %q (idx=%d), want (@p1) idx=1", got, idx)
	}
	idx = 0
	if got := rowValuePlaceholders(3, &idx); got != "(@p1,@p2,@p3)" || idx != 3 {
		t.Errorf("rowValuePlaceholders(3) = %q (idx=%d), want (@p1,@p2,@p3) idx=3", got, idx)
	}
	if got := rowValuePlaceholders(0, &idx); got != "()" {
		t.Errorf("rowValuePlaceholders(0) = %q, want ()", got)
	}
}

// ---- splitSchemaTable ----

func TestSplitSchemaTable(t *testing.T) {
	tests := []struct {
		input      string
		wantSchema string
		wantName   string
	}{
		{"dbo.users", "dbo", "users"},
		{"users", "dbo", "users"},
		{"sales.orders", "sales", "orders"},
	}
	for _, tt := range tests {
		s, n := splitSchemaTable(tt.input)
		if s != tt.wantSchema || n != tt.wantName {
			t.Errorf("splitSchemaTable(%q) = (%q, %q), want (%q, %q)",
				tt.input, s, n, tt.wantSchema, tt.wantName)
		}
	}
}

// ---- quoteID ----

func TestQuoteID(t *testing.T) {
	if got := quoteID("users"); got != "[users]" {
		t.Errorf("quoteID(users) = %q, want [users]", got)
	}
	if got := quoteID("dbo.users"); got != "[dbo].[users]" {
		t.Errorf("quoteID(dbo.users) = %q, want [dbo].[users]", got)
	}
}
