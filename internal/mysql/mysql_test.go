package mysql

import (
	"strings"
	"testing"
	"time"

	"godbsync/internal/dialect"
	"godbsync/internal/types"
)

func TestMapType_Varchar2(t *testing.T) {
	got, err := MapType(types.Column{DBType: "VARCHAR2", Length: 256})
	if err != nil {
		t.Fatal(err)
	}
	if got != "VARCHAR(256)" {
		t.Errorf("VARCHAR2(256) → got %q", got)
	}
}

func TestMapType_Varchar2Text(t *testing.T) {
	got, err := MapType(types.Column{DBType: "VARCHAR2", Length: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got != "TEXT" {
		t.Errorf("VARCHAR2(0) → TEXT, got %q", got)
	}
}

func TestMapType_Varchar2Longtext(t *testing.T) {
	got, err := MapType(types.Column{DBType: "VARCHAR2", Length: 20000})
	if err != nil {
		t.Fatal(err)
	}
	if got != "LONGTEXT" {
		t.Errorf("VARCHAR2(20000) → LONGTEXT, got %q", got)
	}
}

func TestMapType_NVarchar2(t *testing.T) {
	got, err := MapType(types.Column{DBType: "NVARCHAR2", Length: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got != "VARCHAR(100)" {
		t.Errorf("NVARCHAR2 → got %q", got)
	}
}

func TestMapType_Char(t *testing.T) {
	tests := []struct {
		length int64
		want   string
	}{
		{0, "CHAR(1)"},
		{10, "CHAR(10)"},
		{300, "VARCHAR(255)"},
	}
	for _, tt := range tests {
		got, err := MapType(types.Column{DBType: "CHAR", Length: tt.length})
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("CHAR(%d) → got %q, want %q", tt.length, got, tt.want)
		}
	}
}

func TestMapType_Number(t *testing.T) {
	tests := []struct {
		precision, scale int
		want             string
	}{
		{0, 0, "DECIMAL(38,0)"},
		{1, 0, "TINYINT"},
		{3, 0, "SMALLINT"},
		{6, 0, "INT"},
		{12, 0, "BIGINT"},
		{20, 0, "DECIMAL(20,0)"},
		{10, 2, "DECIMAL(10,2)"},
		{0, 3, "DECIMAL(38,3)"},
		{70, 2, "DECIMAL(65,2)"},
	}
	for _, tt := range tests {
		got, err := MapType(types.Column{DBType: "NUMBER", Precision: tt.precision, Scale: tt.scale})
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("NUMBER(%d,%d) → got %q, want %q", tt.precision, tt.scale, got, tt.want)
		}
	}
}

func TestMapType_FloatVariants(t *testing.T) {
	for _, typ := range []string{"FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE", "DOUBLE", "DOUBLE PRECISION", "REAL",
		"BFLOAT", "BDOUBLE", "IBFLOAT", "IBDOUBLE"} {
		got, err := MapType(types.Column{DBType: typ})
		if err != nil {
			t.Fatal(err)
		}
		if got != "DOUBLE" {
			t.Errorf("%s → got %q, want DOUBLE", typ, got)
		}
	}
}

func TestMapType_DateAndTimestamp(t *testing.T) {
	for _, typ := range []string{
		"DATE", "OCIDATE", "TIMESTAMP", "TIMESTAMPDTY",
		"TIMESTAMPTZ_DTY", "TIMESTAMPELTZ", "TIMESTAMPLTZ_DTY",
		"TIMESTAMPTZ", "TIMETZ", "TIMESTAMP_WITH_TZ",
		"TIMESTAMP_WITH_LOCAL_TZ", "DATETIME",
	} {
		got, err := MapType(types.Column{DBType: typ})
		if err != nil {
			t.Fatal(err)
		}
		if got != "DATETIME(6)" {
			t.Errorf("%s → got %q, want DATETIME(6)", typ, got)
		}
	}
}

func TestMapType_Lobs(t *testing.T) {
	tests := []struct {
		srcType string
		want    string
	}{
		{"CLOB", "LONGTEXT"},
		{"NCLOB", "LONGTEXT"},
		{"BLOB", "LONGBLOB"},
		{"RAW", "LONGBLOB"},
		{"LONG RAW", "LONGBLOB"},
		{"LONG", "LONGTEXT"},
	}
	for _, tt := range tests {
		got, err := MapType(types.Column{DBType: tt.srcType})
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("%s → got %q, want %q", tt.srcType, got, tt.want)
		}
	}
}

func TestMapType_UnknownType(t *testing.T) {
	_, err := MapType(types.Column{DBType: "UNKNOWN_TYPE", Name: "col_x"})
	if err == nil {
		t.Error("未知类型应返回错误")
	}
	if !strings.Contains(err.Error(), "未知源类型") {
		t.Errorf("错误信息应包含提示, got %v", err)
	}
	if !strings.Contains(err.Error(), "col_x") {
		t.Errorf("错误信息应包含列名, got %v", err)
	}
}

func TestGenerateCreate_BasicTable(t *testing.T) {
	cols := []types.Column{
		{Name: "ID", DBType: "NUMBER", Precision: 10, Nullable: false},
		{Name: "NAME", DBType: "VARCHAR2", Length: 100, Nullable: true},
	}
	ddl, err := GenerateCreate(nil, "users", cols, []string{"ID"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "CREATE TABLE IF NOT EXISTS") {
		t.Error("应包含 CREATE TABLE IF NOT EXISTS")
	}
	if !strings.Contains(ddl, "`users`") {
		t.Error("应包含反引号包裹的表名")
	}
	if !strings.Contains(ddl, "`ID`") {
		t.Error("应包含 ID 列")
	}
	if !strings.Contains(ddl, "NOT NULL") {
		t.Error("ID 列应包含 NOT NULL")
	}
	if !strings.Contains(ddl, "PRIMARY KEY") {
		t.Error("应包含 PRIMARY KEY 约束")
	}
	if !strings.Contains(ddl, "ENGINE=InnoDB") {
		t.Error("应指定 InnoDB 引擎")
	}
}

func TestGenerateCreate_NoPrimaryKey(t *testing.T) {
	cols := []types.Column{
		{Name: "DATA", DBType: "CLOB", Nullable: true},
	}
	ddl, err := GenerateCreate(nil, "t", cols, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ddl, "PRIMARY KEY") {
		t.Error("无主键时不应包含 PRIMARY KEY")
	}
}

func TestGenerateCreate_CompositePK(t *testing.T) {
	cols := []types.Column{
		{Name: "A", DBType: "NUMBER", Precision: 5},
		{Name: "B", DBType: "NUMBER", Precision: 5},
	}
	ddl, err := GenerateCreate(nil, "t", cols, []string{"A", "B"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "PRIMARY KEY (`A`, `B`)") {
		t.Errorf("应包含复合主键, got %s", ddl)
	}
}

func TestGenerateCreate_EmptyCols(t *testing.T) {
	_, err := GenerateCreate(nil, "t", nil, nil)
	if err == nil {
		t.Error("列为空应返回错误")
	}
}

func TestGenerateCreate_UnknownType(t *testing.T) {
	cols := []types.Column{
		{Name: "BAD", DBType: "FOOBAR"},
	}
	_, err := GenerateCreate(nil, "t", cols, nil)
	if err == nil {
		t.Error("未知类型应报错")
	}
}

// ---- 版本自动降级（按目标服务端版本生成兼容 DDL，无需配置）----

func TestMySQLCharset_ByVersion(t *testing.T) {
	tests := []struct {
		ver  mysqlVersion
		want string
	}{
		{mysqlVersion{5, 5, 2}, "utf8"},     // 5.5.2 无 utf8mb4
		{mysqlVersion{5, 5, 3}, "utf8mb4"},  // 5.5.3 起支持
		{mysqlVersion{5, 6, 51}, "utf8mb4"}, // 5.6.x
		{mysqlVersion{8, 0, 36}, "utf8mb4"}, // 8.0
	}
	for _, tt := range tests {
		if got := mysqlCharset(tt.ver); got != tt.want {
			t.Errorf("mysqlCharset(%d.%d.%d) = %q, want %q", tt.ver.major, tt.ver.minor, tt.ver.patch, got, tt.want)
		}
	}
}

func TestMapType_TimeByVersion(t *testing.T) {
	modern := mysqlVersion{8, 0, 0}
	old := mysqlVersion{5, 5, 30} // 无 DATETIME(6) 微秒精度
	gotModern, _ := mysqlTypeForVersion(types.Column{DBType: "DATETIME"}, modern)
	if gotModern != "DATETIME(6)" {
		t.Errorf("现代版本 DATETIME 应映射 DATETIME(6), got %q", gotModern)
	}
	gotOld, _ := mysqlTypeForVersion(types.Column{DBType: "DATETIME"}, old)
	if gotOld != "DATETIME" {
		t.Errorf("5.5.x DATETIME 应退化为 DATETIME(无 fsp), got %q", gotOld)
	}
}

func TestMapType_JSONByVersion(t *testing.T) {
	modern := mysqlVersion{8, 0, 0}
	old := mysqlVersion{5, 6, 51} // 5.6 无 JSON 类型
	gotModern, _ := mysqlTypeForVersion(types.Column{DBType: "JSON"}, modern)
	if gotModern != "JSON" {
		t.Errorf("现代版本 JSON 应映射 JSON, got %q", gotModern)
	}
	gotOld, _ := mysqlTypeForVersion(types.Column{DBType: "JSON"}, old)
	if gotOld != "LONGTEXT" {
		t.Errorf("5.6 JSON 应退化为 LONGTEXT, got %q", gotOld)
	}
}

// ---- MySQL 作为同步源端（Reader）的单元测试 ----

func TestMySQLReader_Dialect(t *testing.T) {
	var r dialect.Reader = &Reader{}
	if r.Dialect() != "mysql" {
		t.Errorf("Dialect 应为 mysql, got %q", r.Dialect())
	}
}

func TestMySQLReader_Registered(t *testing.T) {
	r, err := dialect.NewReader("mysql")
	if err != nil {
		t.Fatalf("NewReader(mysql) 应成功注册, got %v", err)
	}
	if r.Dialect() != "mysql" {
		t.Errorf("注册实例 Dialect 应为 mysql, got %q", r.Dialect())
	}
}

func TestMySQLReader_Placeholder(t *testing.T) {
	r := &Reader{}
	if got := r.Placeholder(1); got != "?" {
		t.Errorf("Placeholder(1) = %q, want ?", got)
	}
	if got := r.Placeholder(5); got != "?" {
		t.Errorf("Placeholder(5) = %q, want ?", got)
	}
}

func TestMySQLReader_QuoteIdentifier(t *testing.T) {
	r := &Reader{}
	if got := r.QuoteIdentifier("col"); got != "`col`" {
		t.Errorf("QuoteIdentifier = %q, want `col`", got)
	}
}

func TestMySQLReader_SupportsRowID(t *testing.T) {
	r := &Reader{}
	if r.SupportsRowID() {
		t.Error("MySQL 不应支持 ROWID")
	}
}

func TestMySQLReader_IsTimeType(t *testing.T) {
	r := &Reader{}
	for _, typ := range []string{"DATE", "DATETIME", "TIMESTAMP", "TIME", "datetime", "timestamp(6)"} {
		if !r.IsTimeType(typ) {
			t.Errorf("%s 应被识别为时间类型", typ)
		}
	}
	for _, typ := range []string{"VARCHAR", "BIGINT", "DECIMAL", "JSON", "YEAR"} {
		if r.IsTimeType(typ) {
			t.Errorf("%s 不应被识别为时间类型", typ)
		}
	}
}

func TestMySQLReader_IsNumericType(t *testing.T) {
	r := &Reader{}
	for _, typ := range []string{"INT", "INTEGER", "BIGINT", "TINYINT", "SMALLINT", "MEDIUMINT", "DECIMAL", "FLOAT", "DOUBLE", "BIT", "bigint"} {
		if !r.IsNumericType(typ) {
			t.Errorf("%s 应被识别为数值类型", typ)
		}
	}
	for _, typ := range []string{"VARCHAR", "TEXT", "JSON", "DATE"} {
		if r.IsNumericType(typ) {
			t.Errorf("%s 不应被识别为数值类型", typ)
		}
	}
}

func TestMySQLReader_NormalizeByteToString(t *testing.T) {
	r := &Reader{}
	// MySQL go-sql-driver 对文本列返回 []byte，绑给 Oracle 前需转 string。
	got := r.Normalize([]byte("hello"), "")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("[]byte 应转为 string, got %T", got)
	}
	if s != "hello" {
		t.Errorf("转换结果错误: %q", s)
	}
	// 基础类型应原样保留。
	if r.Normalize(int64(7), "") != int64(7) {
		t.Error("int64 应保留")
	}
	if _, ok := r.Normalize(time.Now(), "").(time.Time); !ok {
		t.Error("time.Time 应保留")
	}
}
