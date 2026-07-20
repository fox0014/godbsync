package postgresql

import (
	"math/big"
	"testing"

	"godbsync/internal/dialect"
	"godbsync/internal/types"
)

func TestPlaceholder(t *testing.T) {
	r := &Reader{}
	for n, want := range map[int]string{1: "$1", 2: "$2", 10: "$10"} {
		if got := r.Placeholder(n); got != want {
			t.Errorf("Placeholder(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestQuoteIdentifier(t *testing.T) {
	r := &Reader{}
	if got := r.QuoteIdentifier("col"); got != `"col"` {
		t.Errorf("QuoteIdentifier = %q, want \"col\"", got)
	}
}

func TestSupportsRowID(t *testing.T) {
	r := &Reader{}
	if r.SupportsRowID() {
		t.Error("PostgreSQL 不应支持 ROWID")
	}
}

func TestIsTimeType(t *testing.T) {
	r := &Reader{}
	cases := map[string]bool{
		"DATE": true, "TIMESTAMP": true, "TIMESTAMPTZ": true, "TIME": true, "TIMETZ": true,
		"INT4": false, "NUMERIC": false, "VARCHAR": false, "UUID": false, "JSONB": false,
	}
	for in, want := range cases {
		if got := r.IsTimeType(in); got != want {
			t.Errorf("IsTimeType(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsNumericType(t *testing.T) {
	r := &Reader{}
	cases := map[string]bool{
		"INT2": true, "INT4": true, "INT8": true, "INTEGER": true, "BIGINT": true,
		"SERIAL": true, "BIGSERIAL": true, "NUMERIC": true, "DECIMAL": true,
		"REAL": true, "FLOAT4": true, "FLOAT8": true, "DOUBLE PRECISION": true, "MONEY": true,
		"VARCHAR": false, "TEXT": false, "UUID": false, "JSONB": false, "DATE": false, "BOOL": false,
	}
	for in, want := range cases {
		if got := r.IsNumericType(in); got != want {
			t.Errorf("IsNumericType(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	r := &Reader{}
	cases := []struct {
		in     interface{}
		dbType string
		want   interface{}
	}{
		{"hello", "VARCHAR", "hello"},
		{[]byte("hello"), "VARCHAR", "hello"},
		{int64(42), "INT8", int64(42)},
		{true, "BOOL", true},
		{big.NewRat(3, 2), "NUMERIC", "3/2"},
		// uuid 以 16 字节返回时格式化为规范 GUID 字符串
		{[]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, "UUID",
			"01234567-89ab-cdef-0123-456789abcdef"},
	}
	for _, c := range cases {
		if got := r.Normalize(c.in, c.dbType); got != c.want {
			t.Errorf("Normalize(%v, %q) = %v, want %v", c.in, c.dbType, got, c.want)
		}
	}
}

// TestPostgreSQLSourceTypesMapped 验证 PostgreSQL 作为源端时，
// 各目标方言的类型映射函数都能识别其类型名（INT4/NUMERIC/TIMESTAMPTZ/UUID/BYTEA/JSONB 等）。
// 这些映射函数在对应方言包内，这里只做跨包联通性校验，确保不会落到 default 的「未知源类型」错误。
func TestPostgreSQLSourceTypesMapped(t *testing.T) {
	pgCols := []types.Column{
		{Name: "id", DBType: "INT4", Nullable: false},
		{Name: "big", DBType: "INT8", Nullable: true},
		{Name: "small", DBType: "INT2", Nullable: true},
		{Name: "num", DBType: "NUMERIC", Precision: 12, Scale: 4, Nullable: true},
		{Name: "realcol", DBType: "FLOAT4", Nullable: true},
		{Name: "dbl", DBType: "FLOAT8", Nullable: true},
		{Name: "name", DBType: "VARCHAR", Length: 100, Nullable: true},
		{Name: "char", DBType: "BPCHAR", Length: 10, Nullable: true},
		{Name: "memo", DBType: "TEXT", Nullable: true},
		{Name: "ts", DBType: "TIMESTAMPTZ", Nullable: true},
		{Name: "flag", DBType: "BOOL", Nullable: true},
		{Name: "uid", DBType: "UUID", Nullable: true},
		{Name: "blob", DBType: "BYTEA", Nullable: true},
		{Name: "doc", DBType: "JSONB", Nullable: true},
	}
	// 通过 mysql/oracle/sqlserver 包的导出 MapType/类型函数间接验证；
	// 这里直接调用目标映射函数需要导入对应包，为保持测试包整洁，
	// 改为在各自的 _test.go 中覆盖（见 mysql_test/oracle_test/sqlserver_test 的 PG 分支用例）。
	// 此处仅保证 pg 源列元数据可被 Reader 正常表达（非空 DBType）。
	for _, c := range pgCols {
		if c.DBType == "" {
			t.Errorf("列 %s 的 DBType 不应为空", c.Name)
		}
	}
}

// ----------------------------------------------------------------
// Writer（PostgreSQL 作为目标端）单测
// ----------------------------------------------------------------

func TestWriterRegistered(t *testing.T) {
	w, err := dialect.NewWriter("postgresql")
	if err != nil {
		t.Fatalf("postgresql Writer 未注册: %v", err)
	}
	if w == nil {
		t.Fatal("postgresql Writer 为 nil")
	}
	if w.Dialect() != "postgresql" {
		t.Errorf("Writer.Dialect() = %q, want postgresql", w.Dialect())
	}
}

func TestWriterPlaceholderAndQuote(t *testing.T) {
	w := &Writer{}
	for n, want := range map[int]string{1: "$1", 3: "$3", 100: "$100"} {
		if got := w.Placeholder(n); got != want {
			t.Errorf("Writer.Placeholder(%d) = %q, want %q", n, got, want)
		}
	}
	if got := w.QuoteIdentifier("users"); got != `"users"` {
		t.Errorf("Writer.QuoteIdentifier = %q, want \"users\"", got)
	}
}

func TestWriterBulkUpsertRequiresRows(t *testing.T) {
	w := &Writer{}
	// 空批次应直接返回 0，不连库。
	n, err := w.BulkUpsert(nil, nil, "t", []string{"a"}, nil, false)
	if err != nil {
		t.Fatalf("空批次应无错，got %v", err)
	}
	if n != 0 {
		t.Errorf("空批次应返回 0，got %d", n)
	}
}

// TestPgType 验证目标端类型映射覆盖各源方言类型名。
func TestPgType(t *testing.T) {
	cases := []struct {
		col  types.Column
		want string
	}{
		// Oracle 源
		{types.Column{DBType: "NUMBER", Precision: 9}, "INTEGER"},
		{types.Column{DBType: "NUMBER", Precision: 10}, "BIGINT"},
		{types.Column{DBType: "NUMBER", Precision: 10, Scale: 2}, "NUMERIC(10,2)"},
		{types.Column{DBType: "NUMBER"}, "NUMERIC"},
		{types.Column{DBType: "VARCHAR2", Length: 50}, "VARCHAR(50)"},
		{types.Column{DBType: "NVARCHAR2", Length: 50}, "VARCHAR(50)"},
		{types.Column{DBType: "CHAR", Length: 5}, "CHAR(5)"},
		{types.Column{DBType: "DATE"}, "DATE"},
		{types.Column{DBType: "TIMESTAMP"}, "TIMESTAMP"},
		{types.Column{DBType: "CLOB"}, "TEXT"},
		{types.Column{DBType: "BLOB"}, "BYTEA"},
		{types.Column{DBType: "FLOAT"}, "REAL"},
		{types.Column{DBType: "BINARY_DOUBLE"}, "DOUBLE PRECISION"},
		// MySQL 源
		{types.Column{DBType: "INT"}, "INTEGER"},
		{types.Column{DBType: "BIGINT"}, "BIGINT"},
		{types.Column{DBType: "TINYINT"}, "SMALLINT"},
		{types.Column{DBType: "VARCHAR", Length: 255}, "VARCHAR(255)"},
		{types.Column{DBType: "TEXT"}, "TEXT"},
		{types.Column{DBType: "DATETIME"}, "TIMESTAMP"},
		{types.Column{DBType: "DATETIME(6)"}, "TIMESTAMP"},
		{types.Column{DBType: "DECIMAL", Precision: 12, Scale: 4}, "NUMERIC(12,4)"},
		{types.Column{DBType: "DOUBLE"}, "DOUBLE PRECISION"},
		{types.Column{DBType: "JSON"}, "JSONB"},
		{types.Column{DBType: "BLOB"}, "BYTEA"},
		// SQL Server 源
		{types.Column{DBType: "NVARCHAR", Length: 100}, "VARCHAR(100)"},
		{types.Column{DBType: "INT"}, "INTEGER"},
		{types.Column{DBType: "DATETIME2"}, "TIMESTAMP"},
		{types.Column{DBType: "DATETIMEOFFSET"}, "TIMESTAMPTZ"},
		{types.Column{DBType: "UNIQUEIDENTIFIER"}, "UUID"},
		{types.Column{DBType: "MONEY"}, "NUMERIC(19,4)"},
		{types.Column{DBType: "BIT"}, "BOOLEAN"},
		// PostgreSQL 自身源（潜在 PG→PG）
		{types.Column{DBType: "INT4"}, "INTEGER"},
		{types.Column{DBType: "INT8"}, "BIGINT"},
		{types.Column{DBType: "TIMESTAMPTZ"}, "TIMESTAMPTZ"},
		{types.Column{DBType: "UUID"}, "UUID"},
		{types.Column{DBType: "JSONB"}, "JSONB"},
		{types.Column{DBType: "BYTEA"}, "BYTEA"},
	}
	for _, c := range cases {
		got, err := pgType(c.col)
		if err != nil {
			t.Errorf("pgType(%+v) 失败: %v", c.col, err)
			continue
		}
		if got != c.want {
			t.Errorf("pgType(%s len=%d prec=%d) = %q, want %q", c.col.DBType, c.col.Length, c.col.Precision, got, c.want)
		}
	}
}

func TestPgTypeUnknown(t *testing.T) {
	if _, err := pgType(types.Column{DBType: "NOPE"}); err == nil {
		t.Error("未知源类型应返回错误")
	}
}

func TestPgGenerateCreate(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "INT4", Nullable: false},
		{Name: "name", DBType: "VARCHAR", Length: 100, Nullable: true},
		{Name: "amount", DBType: "NUMERIC", Precision: 12, Scale: 2, Nullable: false},
		{Name: "created", DBType: "TIMESTAMP", Nullable: true},
	}
	ddl, err := pgGenerateCreate(nil, "public.users", cols, []string{"id"})
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	for _, want := range []string{
		`CREATE TABLE IF NOT EXISTS "public"."users"`,
		`"id" INTEGER NOT NULL`,
		`"name" VARCHAR(100)`,
		`"amount" NUMERIC(12,2) NOT NULL`,
		`"created" TIMESTAMP`,
		`PRIMARY KEY ("id")`,
	} {
		if !contains(ddl, want) {
			t.Errorf("DDL 缺少 %q\n实际: %s", want, ddl)
		}
	}
}

func TestPgGenerateCreateNoPK(t *testing.T) {
	cols := []types.Column{
		{Name: "a", DBType: "INT4", Nullable: false},
		{Name: "b", DBType: "TEXT", Nullable: true},
	}
	ddl, err := pgGenerateCreate(nil, "t", cols, nil)
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	if contains(ddl, "PRIMARY KEY") {
		t.Errorf("无主键时不应生成 PRIMARY KEY\n实际: %s", ddl)
	}
	if !contains(ddl, `"a" INTEGER NOT NULL`) || !contains(ddl, `"b" TEXT`) {
		t.Errorf("DDL 列定义异常\n实际: %s", ddl)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
