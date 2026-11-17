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

// ---- MySQL→MySQL 建表保真（回归 Error 1170）----

// TestGenerateCreate_MySQLSourceFidelity 复现现场表 t_work_type_coefficient_copy1：
// 源库 varchar 列在旧实现里因 ColumnType.Length() 恒不可用而全部降级为 TEXT，
// 导致 `id` TEXT 作主键触发 Error 1170。修复后应沿用 COLUMN_TYPE 原文。
func TestGenerateCreate_MySQLSourceFidelity(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "VARCHAR", RawType: "varchar(32)", Nullable: false,
			CharSet: "utf8mb4", Collation: "utf8mb4_general_ci"},
		{Name: "create_by", DBType: "VARCHAR", RawType: "varchar(10)", Nullable: true,
			CharSet: "utf8mb4", Collation: "utf8mb4_general_ci"},
		{Name: "create_by_name", DBType: "VARCHAR", RawType: "varchar(100)", Nullable: true,
			CharSet: "utf8mb4", Collation: "utf8mb4_general_ci"},
		{Name: "center", DBType: "VARCHAR", RawType: "varchar(100)", Nullable: true,
			CharSet: "utf8mb4", Collation: "utf8mb4_general_ci"},
		{Name: "pk_org", DBType: "VARCHAR", RawType: "varchar(32)", Nullable: true,
			CharSet: "utf8mb4", Collation: "utf8mb4_general_ci"},
		{Name: "create_time", DBType: "DATETIME", RawType: "datetime", Nullable: true,
			HasDefault: true, Default: "CURRENT_TIMESTAMP"},
		{Name: "post_coef", DBType: "DECIMAL", RawType: "decimal(14,2)", Precision: 14, Scale: 2, Nullable: true},
	}
	ddl, err := GenerateCreate(nil, "t_work_type_coefficient_copy1", cols, []string{"id"})
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	mustContain := []string{
		"`id` varchar(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL",
		"`create_by` varchar(10)",
		"`create_by_name` varchar(100)",
		"`center` varchar(100)",
		"`pk_org` varchar(32)",
		"`create_time` datetime DEFAULT CURRENT_TIMESTAMP",
		"`post_coef` decimal(14,2)",
		"PRIMARY KEY (`id`)",
	}
	for _, want := range mustContain {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL 缺少片段 %q\n实际 DDL:\n%s", want, ddl)
		}
	}
	for _, bad := range []string{"`id` TEXT", "DATETIME(6)", "`create_by` TEXT"} {
		if strings.Contains(ddl, bad) {
			t.Errorf("DDL 不应包含 %q\n实际 DDL:\n%s", bad, ddl)
		}
	}
}

// TestGenerateCreate_MySQLSourceAttributes 验证 char/text/unsigned/enum、DEFAULT 与 COMMENT 保真。
func TestGenerateCreate_MySQLSourceAttributes(t *testing.T) {
	cols := []types.Column{
		{Name: "code", DBType: "CHAR", RawType: "char(2)", Nullable: false,
			HasDefault: true, Default: "x", Comment: "代码"},
		{Name: "flag", DBType: "INT", RawType: "int unsigned", Nullable: false,
			HasDefault: true, Default: "0"},
		{Name: "kind", DBType: "ENUM", RawType: "enum('a','b')", Nullable: true,
			HasDefault: true, Default: "a"},
		{Name: "note", DBType: "MEDIUMTEXT", RawType: "mediumtext", Nullable: true},
		{Name: "blob_col", DBType: "BLOB", RawType: "longblob", Nullable: true},
		{Name: "uid", DBType: "VARCHAR", RawType: "varchar(36)", Nullable: true,
			HasDefault: true, Default: "uuid()", DefaultIsExpr: true},
		{Name: "percent", DBType: "DECIMAL", RawType: "decimal(5,2)", Nullable: true,
			HasDefault: true, Default: "-1.50"},
	}
	ddl, err := GenerateCreate(nil, "t_attrs", cols, nil)
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	mustContain := []string{
		"`code` char(2) NOT NULL DEFAULT 'x' COMMENT '代码'",
		"`flag` int unsigned NOT NULL DEFAULT 0",
		"`kind` enum('a','b') DEFAULT 'a'",
		"`note` mediumtext",
		"`blob_col` longblob",
		"`uid` varchar(36) DEFAULT (uuid())",
		"`percent` decimal(5,2) DEFAULT -1.50",
	}
	for _, want := range mustContain {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL 缺少片段 %q\n实际 DDL:\n%s", want, ddl)
		}
	}
}

// TestMySQLRawTypeForVersion 验证 COLUMN_TYPE 原文按目标版本降级（不改动现代版本路径）。
func TestMySQLRawTypeForVersion(t *testing.T) {
	modern := mysqlVersion{8, 0, 0}
	old56 := mysqlVersion{5, 6, 51}
	old55 := mysqlVersion{5, 5, 30}

	got, err := mysqlRawTypeForVersion(types.Column{Name: "d", RawType: "json"}, modern)
	if err != nil || got != "json" {
		t.Errorf("8.0 json → got %q, err %v", got, err)
	}
	got, err = mysqlRawTypeForVersion(types.Column{Name: "d", RawType: "json"}, old56)
	if err != nil || got != "LONGTEXT" {
		t.Errorf("5.6 json → got %q, err %v", got, err)
	}
	got, err = mysqlRawTypeForVersion(types.Column{Name: "d", RawType: "datetime(6)"}, old55)
	if err != nil || got != "datetime" {
		t.Errorf("5.5 datetime(6) → got %q, err %v", got, err)
	}
	got, err = mysqlRawTypeForVersion(types.Column{Name: "d", RawType: "datetime(6)"}, old56)
	if err != nil || got != "datetime(6)" {
		t.Errorf("5.6.51 datetime(6) → got %q, err %v", got, err)
	}
	if _, err := mysqlRawTypeForVersion(types.Column{Name: "d", RawType: "varchar(10); DROP TABLE x"}, modern); err == nil {
		t.Error("含分号的异常类型定义应报错")
	}
}

// TestValidRawType 验证 enum 字面量中的分号/引号不会误判，语句注入形态会被拒绝。
func TestValidRawType(t *testing.T) {
	valid := []string{"varchar(32)", "int unsigned", "decimal(14,2)", "enum('a;b','c\\'d')", "set('x','y')"}
	for _, s := range valid {
		if !validRawType(s) {
			t.Errorf("应视为合法: %q", s)
		}
	}
	invalid := []string{"varchar(10)`", "int; drop", "varchar(10)\n)", "enum('a"}
	for _, s := range invalid {
		if validRawType(s) {
			t.Errorf("应视为非法: %q", s)
		}
	}
}

// TestMySQLTypesEqual 验证 GenerateAlter 的类型比对不受大小写/整数显示宽度影响。
func TestMySQLTypesEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"int(11) unsigned", "int unsigned", true},
		{"INT", "int(11)", true},
		{"VARCHAR(32)", "varchar(32)", true},
		{"varchar(32)", "varchar(64)", false},
		{"decimal(14,2)", "decimal(14,2)", true},
		{"datetime(6)", "datetime", false},
	}
	for _, c := range cases {
		if got := mysqlTypesEqual(c.a, c.b); got != c.want {
			t.Errorf("mysqlTypesEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestGenerateCreate_MySQLSourceAutoIncOnUpdateGenerated 覆盖 AUTO_INCREMENT、
// ON UPDATE CURRENT_TIMESTAMP、生成列（VIRTUAL/STORED）三项列定义保真。
func TestGenerateCreate_MySQLSourceAutoIncOnUpdateGenerated(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false, AutoIncrement: true,
			AutoIncrementStart: 1000},
		{Name: "name", DBType: "VARCHAR", RawType: "varchar(20)", Nullable: true,
			CharSet: "utf8mb4", Collation: "utf8mb4_general_ci"},
		{Name: "created_at", DBType: "TIMESTAMP", RawType: "timestamp", Nullable: false,
			HasDefault: true, Default: "CURRENT_TIMESTAMP", OnUpdateCurrentTimestamp: true},
		{Name: "updated_at", DBType: "DATETIME", RawType: "datetime(6)", Nullable: true,
			HasDefault: true, Default: "CURRENT_TIMESTAMP(6)", OnUpdateCurrentTimestamp: true},
		{Name: "full_name", DBType: "VARCHAR", RawType: "varchar(60)", Nullable: true,
			CharSet: "utf8mb4", Collation: "utf8mb4_general_ci",
			GenerationExpression: "concat(`name`,_utf8mb4'-x')", GenerationType: "VIRTUAL"},
		{Name: "total", DBType: "DECIMAL", RawType: "decimal(10,2)", Nullable: true,
			GenerationExpression: "(`a` * `b`)", GenerationType: "STORED"},
	}
	ddl, err := GenerateCreate(nil, "t_gen", cols, []string{"id"})
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	mustContain := []string{
		"`id` bigint NOT NULL AUTO_INCREMENT",
		"`created_at` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP",
		"`updated_at` datetime(6) DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)",
		"`full_name` varchar(60) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci GENERATED ALWAYS AS (concat(`name`,_utf8mb4'-x')) VIRTUAL",
		"`total` decimal(10,2) GENERATED ALWAYS AS (`a` * `b`) STORED",
		"PRIMARY KEY (`id`)",
		"DEFAULT CHARSET=utf8mb4 AUTO_INCREMENT=1000",
	}
	for _, want := range mustContain {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL 缺少片段 %q\n实际 DDL:\n%s", want, ddl)
		}
	}
	if strings.Contains(ddl, "GENERATED ALWAYS AS (((") {
		t.Errorf("生成列表达式不应出现异常多重括号\n实际 DDL:\n%s", ddl)
	}
}

// TestGenerateCreate_AutoIncrementStart 验证自增计数器仅在「存在自增列且源端有值」时输出。
func TestGenerateCreate_AutoIncrementStart(t *testing.T) {
	// 源表从未插入过数据：TABLES.AUTO_INCREMENT 为 NULL/0，不输出起始值。
	cols := []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false, AutoIncrement: true},
	}
	ddl, err := GenerateCreate(nil, "t_empty", cols, []string{"id"})
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	if strings.Contains(ddl, "AUTO_INCREMENT=") {
		t.Errorf("无源计数器时不应输出 AUTO_INCREMENT=，实际 DDL:\n%s", ddl)
	}

	// 不存在自增列时，即使其它列误带起始值也不输出。
	cols = []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false, AutoIncrementStart: 1000},
	}
	ddl, err = GenerateCreate(nil, "t_noai", cols, []string{"id"})
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	if strings.Contains(ddl, "AUTO_INCREMENT=") {
		t.Errorf("无自增列时不应输出 AUTO_INCREMENT=，实际 DDL:\n%s", ddl)
	}

	// 源表删过尾部数据：计数器 1000 > max(id)+1，应原样写入建表语句。
	cols = []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false, AutoIncrement: true,
			AutoIncrementStart: 1000},
	}
	ddl, err = GenerateCreate(nil, "t_start", cols, []string{"id"})
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	if !strings.HasSuffix(ddl, "AUTO_INCREMENT=1000") {
		t.Errorf("应在表选项末尾输出 AUTO_INCREMENT=1000，实际 DDL:\n%s", ddl)
	}
}

// TestGenerateCreate_AutoIncrementMustBeKey 验证自增列不是主键首列时明确报错
// （本工具只生成主键约束，不迁移二级/唯一索引，无法保证自增列有合法索引）。
func TestGenerateCreate_AutoIncrementMustBeKey(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false, AutoIncrement: true},
		{Name: "code", DBType: "VARCHAR", RawType: "varchar(32)", Nullable: false},
	}
	if _, err := GenerateCreate(nil, "t_ai", cols, []string{"code"}); err == nil {
		t.Fatal("自增列不在主键首列时应报错")
	} else if !strings.Contains(err.Error(), "AUTO_INCREMENT") {
		t.Errorf("错误信息应说明 AUTO_INCREMENT 约束，实际: %v", err)
	}
	if _, err := GenerateCreate(nil, "t_ai", cols, nil); err == nil {
		t.Fatal("无主键且存在自增列时应报错")
	}
	// 复合主键中自增列为首列时合法。
	if _, err := GenerateCreate(nil, "t_ai", cols, []string{"id", "code"}); err != nil {
		t.Errorf("自增列作为复合主键首列应合法: %v", err)
	}
}

// TestGenerateCreate_GeneratedColumnConflicts 验证生成列与 AUTO_INCREMENT 的互斥约束。
func TestGenerateCreate_GeneratedColumnConflicts(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false, AutoIncrement: true,
			GenerationExpression: "(`a` + 1)", GenerationType: "VIRTUAL"},
	}
	if _, err := GenerateCreate(nil, "t_bad", cols, []string{"id"}); err == nil {
		t.Fatal("生成列同时声明 AUTO_INCREMENT 应报错")
	}
}

// TestValidGenerationExpression 验证生成列表达式校验：合法表达式放行，语句注入形态拒绝。
func TestValidGenerationExpression(t *testing.T) {
	valid := []string{
		"(`a` + `b`)",
		"concat(`name`, '-', `x`)",
		"json_extract(`doc`, '$.a;b')",
	}
	for _, s := range valid {
		if !validGenerationExpression(s) {
			t.Errorf("应视为合法生成列表达式: %q", s)
		}
	}
	invalid := []string{"`a`; DROP TABLE x", "`a`\n+ 1", "concat(`a`, 'x"}
	for _, s := range invalid {
		if validGenerationExpression(s) {
			t.Errorf("应视为非法生成列表达式: %q", s)
		}
	}
}

// TestGenerateCreate_TableOptions 验证表尾按源表原样输出 ENGINE / DEFAULT CHARSET /
// COLLATE / COMMENT，且表注释中的单引号与反斜杠被安全转义。
func TestGenerateCreate_TableOptions(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false},
	}
	meta := types.TableMeta{
		Engine:    "MyISAM",
		Charset:   "latin1",
		Collation: "latin1_swedish_ci",
		Comment:   "工种系数表 O'Brien \\tmp",
	}
	ddl, err := generateCreateWithMeta(nil, "t_meta", cols, []string{"id"}, meta)
	if err != nil {
		t.Fatalf("generateCreateWithMeta 失败: %v", err)
	}
	want := ") ENGINE=MyISAM DEFAULT CHARSET=latin1 COLLATE=latin1_swedish_ci COMMENT='工种系数表 O''Brien \\\\tmp'"
	if !strings.Contains(ddl, want) {
		t.Errorf("表尾缺少片段 %q\n实际 DDL:\n%s", want, ddl)
	}
	// 表尾必须位于 PRIMARY KEY 之后（列定义部分不受影响）。
	if i, j := strings.Index(ddl, "PRIMARY KEY"), strings.Index(ddl, "ENGINE=MyISAM"); i < 0 || j < i {
		t.Errorf("表尾选项应位于列定义之后\n实际 DDL:\n%s", ddl)
	}
}

// TestGenerateCreate_TableOptionsDefaults 验证源表级属性缺失时表尾保持历史输出，
// 不出现空的 COMMENT 字面量或 COLLATE。
func TestGenerateCreate_TableOptionsDefaults(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false},
	}
	ddl, err := GenerateCreate(nil, "t_default", cols, []string{"id"})
	if err != nil {
		t.Fatalf("GenerateCreate 失败: %v", err)
	}
	if !strings.HasSuffix(ddl, ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4") {
		t.Errorf("缺省表尾应为 ENGINE=InnoDB DEFAULT CHARSET=utf8mb4，实际 DDL:\n%s", ddl)
	}
	for _, bad := range []string{"COLLATE=", "COMMENT="} {
		if strings.Contains(ddl, bad) {
			t.Errorf("无源表级属性时不应输出 %q，实际 DDL:\n%s", bad, ddl)
		}
	}
}

// TestGenerateCreate_TableOptionsSanitize 验证非法表级取值被降级而不是拼进 DDL；
// 表注释中的引号/分号只作为内容存在，无法闭合字面量形成注入。
func TestGenerateCreate_TableOptionsSanitize(t *testing.T) {
	cols := []types.Column{
		{Name: "id", DBType: "BIGINT", RawType: "bigint", Nullable: false},
	}
	meta := types.TableMeta{
		Engine:    "InnoDB;DROP TABLE x",
		Charset:   "utf8mb4;--",
		Collation: "utf8mb4_general_ci;--",
		Comment:   "'; DROP TABLE x; --",
	}
	ddl, err := generateCreateWithMeta(nil, "t_safe", cols, []string{"id"}, meta)
	if err != nil {
		t.Fatalf("generateCreateWithMeta 失败: %v", err)
	}
	if strings.Contains(ddl, "DROP TABLE") && !strings.Contains(ddl, "COMMENT='''; DROP TABLE x; --'") {
		t.Errorf("非法表级取值未安全处理，实际 DDL:\n%s", ddl)
	}
	if !strings.Contains(ddl, "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4") {
		t.Errorf("非法 engine/charset 应降级为默认值，实际 DDL:\n%s", ddl)
	}
	if strings.Contains(ddl, "COLLATE=") {
		t.Errorf("非法 collation 应省略，实际 DDL:\n%s", ddl)
	}
	if !strings.Contains(ddl, "COMMENT='''; DROP TABLE x; --'") {
		t.Errorf("表注释的单引号应双写转义，实际 DDL:\n%s", ddl)
	}
}

// TestTableOptionsVersionDegrade 验证目标版本低于源表特性时字符集/排序规则按版本降级。
func TestTableOptionsVersionDegrade(t *testing.T) {
	cases := []struct {
		name string
		meta types.TableMeta
		ver  mysqlVersion
		want string
	}{
		{
			name: "5.5.2 无 utf8mb4",
			meta: types.TableMeta{Charset: "utf8mb4", Collation: "utf8mb4_general_ci"},
			ver:  mysqlVersion{major: 5, minor: 5, patch: 2},
			want: "ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_general_ci",
		},
		{
			name: "5.7 降级 0900 排序规则",
			meta: types.TableMeta{Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci"},
			ver:  mysqlVersion{major: 5, minor: 7},
			want: "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		},
		{
			name: "5.7 降级 utf8mb3 名称",
			meta: types.TableMeta{Charset: "utf8mb3", Collation: "utf8mb3_general_ci"},
			ver:  mysqlVersion{major: 5, minor: 7},
			want: "ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_general_ci",
		},
		{
			name: "排序规则与字符集不匹配时省略 COLLATE",
			meta: types.TableMeta{Charset: "latin1", Collation: "utf8mb4_general_ci"},
			ver:  mysqlVersion{major: 8, minor: 0},
			want: "ENGINE=InnoDB DEFAULT CHARSET=latin1",
		},
	}
	for _, c := range cases {
		got := mysqlTableOptions(c.meta, c.ver)
		if got != c.want {
			t.Errorf("%s: mysqlTableOptions = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCharsetFromCollation 验证排序规则名到字符集的推断（兼容服务无映射表时的兜底）。
func TestCharsetFromCollation(t *testing.T) {
	cases := map[string]string{
		"utf8mb4_general_ci": "utf8mb4",
		"utf8mb3_general_ci": "utf8mb3",
		"latin1_swedish_ci":  "latin1",
		"binary":             "binary",
		"":                   "",
	}
	for coll, want := range cases {
		if got := charsetFromCollation(coll); got != want {
			t.Errorf("charsetFromCollation(%q) = %q, want %q", coll, got, want)
		}
	}
}

// TestSplitTableRef 验证从 "db.table" / 反引号限定名中解析库名与表名。
func TestSplitTableRef(t *testing.T) {
	cases := []struct{ ref, schema, table string }{
		{"t", "", "t"},
		{"mfghrdb.t_work_type_coefficient_copy1", "mfghrdb", "t_work_type_coefficient_copy1"},
		{"`mfghrdb`.`t`", "mfghrdb", "t"},
	}
	for _, c := range cases {
		s, tb := splitTableRef(c.ref)
		if s != c.schema || tb != c.table {
			t.Errorf("splitTableRef(%q) = (%q,%q), want (%q,%q)", c.ref, s, tb, c.schema, c.table)
		}
	}
}
