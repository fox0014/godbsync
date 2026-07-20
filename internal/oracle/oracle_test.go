package oracle

import (
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"godbsync/internal/dialect"
	"godbsync/internal/types"
)

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
		{"[]byte", []byte("hello")},
		{"bool", true},
		{"time", time.Now()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalize(tt.input)
			if !reflect.DeepEqual(got, tt.input) {
				t.Errorf("normalize(%v) = %v (%T), want equal", tt.input, got, got)
			}
		})
	}
}

func TestNormalize_BigIntToString(t *testing.T) {
	bigVal := big.NewInt(1234567890123456789)
	got := normalize(bigVal)
	want := "1234567890123456789"
	if got != want {
		t.Errorf("normalize(big.Int) = %v (%T), want %v", got, got, want)
	}
}

func TestNormalize_CustomTypeToString(t *testing.T) {
	type customType struct{}
	got := normalize(customType{})
	s, ok := got.(string)
	if !ok {
		t.Fatalf("未知类型应转为 string, got %T", got)
	}
	if s == "" {
		t.Error("字符串表示不应为空")
	}
}

func TestIsTimeType(t *testing.T) {
	for _, typ := range []string{"DATE", "TIMESTAMP", "TIMESTAMP(6)", "TIMESTAMP_WITH_TZ", "TIMESTAMP_WITH_LOCAL_TZ", "DATETIME", "OCIDATE", "TIMESTAMPDTY"} {
		if !isTimeType(typ) {
			t.Errorf("%s 应被识别为时间类型", typ)
		}
	}
	for _, typ := range []string{"VARCHAR2", "NUMBER", "CLOB"} {
		if isTimeType(typ) {
			t.Errorf("%s 不应被识别为时间类型", typ)
		}
	}
}

func TestIsNumericType(t *testing.T) {
	for _, typ := range []string{"NUMBER", "NUMERIC", "FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE", "DOUBLE", "DOUBLE PRECISION", "REAL", "BFLOAT", "BDOUBLE"} {
		if !isNumericType(typ) {
			t.Errorf("%s 应被识别为数值类型", typ)
		}
	}
	for _, typ := range []string{"VARCHAR2", "DATE", "CLOB"} {
		if isNumericType(typ) {
			t.Errorf("%s 不应被识别为数值类型", typ)
		}
	}
}

func TestReader_Dialect(t *testing.T) {
	var r dialect.Reader = &Reader{}
	if r.Dialect() != "oracle" {
		t.Errorf("Dialect 应为 oracle, got %q", r.Dialect())
	}
}

func TestReader_Placeholder(t *testing.T) {
	r := &Reader{}
	if got := r.Placeholder(1); got != ":1" {
		t.Errorf("Placeholder(1) = %q, want :1", got)
	}
	if got := r.Placeholder(2); got != ":2" {
		t.Errorf("Placeholder(2) = %q, want :2", got)
	}
}

func TestReader_QuoteIdentifier(t *testing.T) {
	r := &Reader{}
	if got := r.QuoteIdentifier("col"); got != `"col"` {
		t.Errorf("QuoteIdentifier = %q, want \"col\"", got)
	}
}

func TestReader_SupportsRowID(t *testing.T) {
	r := &Reader{}
	if !r.SupportsRowID() {
		t.Error("Oracle 应支持 ROWID")
	}
}

// ---- Oracle 作为同步目标端（Writer）的单元测试 ----

func TestOracleWriter_Dialect(t *testing.T) {
	var w dialect.Writer = &Writer{}
	if w.Dialect() != "oracle" {
		t.Errorf("Dialect 应为 oracle, got %q", w.Dialect())
	}
}

func TestOracleWriter_Registered(t *testing.T) {
	w, err := dialect.NewWriter("oracle")
	if err != nil {
		t.Fatalf("NewWriter(oracle) 应成功注册, got %v", err)
	}
	if w.Dialect() != "oracle" {
		t.Errorf("注册实例 Dialect 应为 oracle, got %q", w.Dialect())
	}
}

func TestOracleWriter_Placeholder(t *testing.T) {
	w := &Writer{}
	if got := w.Placeholder(1); got != ":1" {
		t.Errorf("Placeholder(1) = %q, want :1", got)
	}
	if got := w.Placeholder(3); got != ":3" {
		t.Errorf("Placeholder(3) = %q, want :3", got)
	}
}

func TestOracleWriter_QuoteIdentifier(t *testing.T) {
	w := &Writer{}
	if got := w.QuoteIdentifier("col"); got != `"col"` {
		t.Errorf("QuoteIdentifier = %q, want \"col\"", got)
	}
}

func TestOracleType_Mapping(t *testing.T) {
	tests := []struct {
		col  types.Column
		want string
	}{
		{types.Column{DBType: "VARCHAR", Length: 100}, "VARCHAR2(100)"},
		{types.Column{DBType: "VARCHAR", Length: 5000}, "CLOB"},
		{types.Column{DBType: "INT"}, "NUMBER(10,0)"},
		{types.Column{DBType: "BIGINT"}, "NUMBER(19,0)"},
		{types.Column{DBType: "TINYINT", Length: 1}, "NUMBER(1,0)"},
		{types.Column{DBType: "DECIMAL", Precision: 10, Scale: 2}, "NUMBER(10,2)"},
		{types.Column{DBType: "DOUBLE"}, "BINARY_DOUBLE"},
		{types.Column{DBType: "FLOAT"}, "BINARY_FLOAT"},
		{types.Column{DBType: "DATETIME"}, "TIMESTAMP"},
		{types.Column{DBType: "TEXT"}, "CLOB"},
		{types.Column{DBType: "BLOB"}, "BLOB"},
		{types.Column{DBType: "JSON"}, "CLOB"},
	}
	for _, tt := range tests {
		got, err := oracleType(tt.col, oracleVersion{major: 12})
		if err != nil {
			t.Fatalf("oracleType(%+v) error: %v", tt.col, err)
		}
		if got != tt.want {
			t.Errorf("oracleType(%s) = %q, want %q", tt.col.DBType, got, tt.want)
		}
	}
}

func TestOracleType_9iDegrade(t *testing.T) {
	// 9i 无 BINARY_FLOAT/DOUBLE，FLOAT/DOUBLE 源列应自动降级为 NUMBER。
	gotF, err := oracleType(types.Column{DBType: "FLOAT"}, oracleVersion{major: 9})
	if err != nil {
		t.Fatal(err)
	}
	if gotF != "NUMBER(38,10)" {
		t.Errorf("9i FLOAT 应降级为 NUMBER(38,10), got %q", gotF)
	}
	gotD, err := oracleType(types.Column{DBType: "DOUBLE"}, oracleVersion{major: 9})
	if err != nil {
		t.Fatal(err)
	}
	if gotD != "NUMBER(38,15)" {
		t.Errorf("9i DOUBLE 应降级为 NUMBER(38,15), got %q", gotD)
	}
	// 10g+ 仍用 BINARY_FLOAT/DOUBLE。
	gotModern, _ := oracleType(types.Column{DBType: "DOUBLE"}, oracleVersion{major: 11})
	if gotModern != "BINARY_DOUBLE" {
		t.Errorf("11g DOUBLE 应保留 BINARY_DOUBLE, got %q", gotModern)
	}
}

func TestOracleGenerateCreate_Basic(t *testing.T) {
	cols := []types.Column{
		{Name: "ID", DBType: "BIGINT", Nullable: false},
		{Name: "NAME", DBType: "VARCHAR", Length: 100, Nullable: true},
	}
	ddl, err := oracleGenerateCreate(nil, "users", cols, []string{"ID"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, `CREATE TABLE "users"`) {
		t.Errorf("应创建 users 表, got %s", ddl)
	}
	if !strings.Contains(ddl, `"ID" NUMBER(19,0)`) {
		t.Errorf("应包含 ID 列定义, got %s", ddl)
	}
	if !strings.Contains(ddl, "NOT NULL") {
		t.Error("ID 列应包含 NOT NULL")
	}
	if !strings.Contains(ddl, `PRIMARY KEY ("ID")`) {
		t.Errorf("应包含主键约束, got %s", ddl)
	}
	if strings.Contains(ddl, "IF NOT EXISTS") {
		t.Error("Oracle 不支持 IF NOT EXISTS")
	}
}

func TestOracleGenerateCreate_WithOwner(t *testing.T) {
	cols := []types.Column{{Name: "ID", DBType: "INT", Nullable: false}}
	ddl, err := oracleGenerateCreate(nil, "SCOTT.EMP", cols, []string{"ID"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, `CREATE TABLE "SCOTT"."EMP"`) {
		t.Errorf("应带 owner 限定, got %s", ddl)
	}
}

func TestBuildMergeSQL_WithPK(t *testing.T) {
	cols := []string{"ID", "NAME", "AGE"}
	pk := []string{"ID"}
	sql := buildMergeSQLArray("users", cols, pk)
	// 数组绑定：占位符数 = 列数，每个 :n 绑定一列数组。
	if !strings.Contains(sql, ":1") || !strings.Contains(sql, ":3") {
		t.Errorf("占位符应覆盖 1..3, got %s", sql)
	}
	if strings.Contains(sql, ":4") {
		t.Errorf("数组绑定不应有第 4 个占位符, got %s", sql)
	}
	if !strings.Contains(sql, "MERGE INTO \"users\" t") {
		t.Errorf("应包含 MERGE INTO, got %s", sql)
	}
	if !strings.Contains(sql, "USING (SELECT :1 c0, :2 c1, :3 c2 FROM dual) s") {
		t.Errorf("USING 应为单个 SELECT（数组绑定）, got %s", sql)
	}
	if !strings.Contains(sql, "ON (t.\"ID\" = s.c0)") {
		t.Errorf("ON 子句应匹配主键 ID, got %s", sql)
	}
	if !strings.Contains(sql, "WHEN MATCHED THEN UPDATE SET t.\"NAME\" = s.c1, t.\"AGE\" = s.c2") {
		t.Errorf("更新子句应排除主键列, got %s", sql)
	}
	if !strings.Contains(sql, "WHEN NOT MATCHED THEN INSERT (\"ID\", \"NAME\", \"AGE\") VALUES (s.c0, s.c1, s.c2)") {
		t.Errorf("插入子句应包含所有列, got %s", sql)
	}
}

func TestBuildMergeSQL_OnlyPKColumns(t *testing.T) {
	// 表仅有主键列时不应生成 UPDATE 子句（避免更新 ON 列）。
	cols := []string{"A", "B"}
	pk := []string{"A", "B"}
	sql := buildMergeSQLArray("t", cols, pk)
	if strings.Contains(sql, "WHEN MATCHED THEN UPDATE") {
		t.Errorf("全主键表不应有 UPDATE 子句, got %s", sql)
	}
	if !strings.Contains(sql, "WHEN NOT MATCHED THEN INSERT") {
		t.Errorf("应保留 INSERT 子句, got %s", sql)
	}
}

func TestBuildInsertSQL_NoPK(t *testing.T) {
	cols := []string{"ID", "NAME"}
	sql := buildInsertSQLArray("users", cols)
	if !strings.Contains(sql, "INSERT INTO \"users\"") {
		t.Errorf("应为 INSERT 语句, got %s", sql)
	}
	if !strings.Contains(sql, "INSERT INTO \"users\" (\"ID\", \"NAME\") VALUES (:1, :2)") {
		t.Errorf("应为数组绑定版 INSERT（占位符绑定列数组）, got %s", sql)
	}
}

func TestSplitOwnerTable(t *testing.T) {
	if o, n := splitOwnerTable("SCOTT.EMP"); o != "SCOTT" || n != "EMP" {
		t.Errorf("拆分失败: %q / %q", o, n)
	}
	if o, n := splitOwnerTable("EMP"); o != "" || n != "EMP" {
		t.Errorf("无 owner 时应为空, got %q / %q", o, n)
	}
}
