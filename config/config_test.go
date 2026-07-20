package config

import (
	"strings"
	"testing"
	"time"
)

func TestOracleDSN_DefaultPort(t *testing.T) {
	c := DBConfig{Dialect: "oracle", Host: "10.0.0.1", Username: "scott", Password: "tiger", Service: "ORCL"}
	dsn := c.DSN()
	if !strings.Contains(dsn, ":1521/") {
		t.Errorf("默认端口应为 1521, got %q", dsn)
	}
	if !strings.HasPrefix(dsn, "oracle://") {
		t.Errorf("DSN 应以 oracle:// 开头, got %q", dsn)
	}
}

func TestOracleDSN_CustomPort(t *testing.T) {
	c := DBConfig{Dialect: "oracle", Host: "10.0.0.1", Port: 1522, Username: "scott", Password: "tiger", Service: "ORCL"}
	dsn := c.DSN()
	if !strings.Contains(dsn, ":1522/") {
		t.Errorf("应使用指定的 1522 端口, got %q", dsn)
	}
}

func TestOracleDSN_SIDMode(t *testing.T) {
	c := DBConfig{Dialect: "oracle", Host: "10.0.0.1", Username: "scott", Password: "tiger", Service: "ORCL", ConnType: "sid"}
	dsn := c.DSN()
	if !strings.Contains(dsn, "?SID=ORCL") && !strings.Contains(dsn, "SID") {
		t.Errorf("sid 模式应包含 SID 参数, got %q", dsn)
	}
}

func TestOracleDSN_SIDUpperCase(t *testing.T) {
	c := DBConfig{Dialect: "oracle", Host: "10.0.0.1", Username: "scott", Password: "tiger", Service: "MYDB", ConnType: "SID"}
	dsn := c.DSN()
	if !strings.Contains(dsn, "?SID=MYDB") && !strings.Contains(dsn, "SID") {
		t.Errorf("大写的 SID 模式也应正确, got %q", dsn)
	}
}

func TestOracleDSN_EncodesSpecialChars(t *testing.T) {
	c := DBConfig{Dialect: "oracle", Host: "10.0.0.1", Username: "user@domain", Password: "p@ss!", Service: "ORCL"}
	dsn := c.DSN()
	if strings.Contains(dsn, "@domain@") {
		t.Errorf("用户名中的 @ 应被编码, got %q", dsn)
	}
}

func TestMySQLDSN_DefaultCharsetAndPort(t *testing.T) {
	c := DBConfig{Dialect: "mysql", Host: "10.0.0.1", Username: "root", Password: "pass", Database: "test"}
	dsn := c.DSN()
	if !strings.Contains(dsn, "charset=utf8mb4") {
		t.Errorf("默认字符集应为 utf8mb4, got %q", dsn)
	}
	if !strings.Contains(dsn, ":3306)") {
		t.Errorf("默认端口应为 3306, got %q", dsn)
	}
}

func TestMySQLDSN_CustomCharset(t *testing.T) {
	c := DBConfig{Dialect: "mysql", Host: "10.0.0.1", Username: "root", Password: "pass", Database: "test", Charset: "utf8"}
	dsn := c.DSN()
	if !strings.Contains(dsn, "charset=utf8") {
		t.Errorf("应使用指定的 utf8 字符集, got %q", dsn)
	}
}

func TestMySQLDSN_ContainsExpectedParams(t *testing.T) {
	c := DBConfig{Dialect: "mysql", Host: "10.0.0.1", Username: "root", Password: "pass", Database: "test"}
	dsn := c.DSN()
	for _, want := range []string{"parseTime=true", "loc=UTC", "multiStatements=false"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN 应包含 %s, got %q", want, dsn)
		}
	}
}

func TestMySQLDSN_DefaultTimezoneUTC(t *testing.T) {
	c := DBConfig{Dialect: "mysql", Host: "10.0.0.1", Username: "root", Password: "pass", Database: "test"}
	dsn := c.DSN()
	if !strings.Contains(dsn, "loc=UTC") {
		t.Errorf("未指定 timezone 时应默认 loc=UTC, got %q", dsn)
	}
}

func TestMySQLDSN_CustomTimezone(t *testing.T) {
	c := DBConfig{Dialect: "mysql", Host: "10.0.0.1", Username: "root", Password: "pass", Database: "test", Timezone: "Asia/Shanghai"}
	dsn := c.DSN()
	if !strings.Contains(dsn, "loc=Asia/Shanghai") {
		t.Errorf("应使用指定的 timezone, got %q", dsn)
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Host: "10.0.0.1", Username: "scott", Service: "ORCL"},
		Target: DBConfig{Dialect: "mysql", Host: "10.0.0.2", Username: "root", Database: "test"},
		Tables: []TableConfig{{Name: "USERS"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("有效配置不应报错, got %v", err)
	}
}

func TestValidate_MissingSourceHost(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Username: "x", Service: "x"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("应返回错误")
	}
}

func TestValidate_MissingTargetDatabase(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("应返回错误")
	}
}

func TestValidate_EmptyTables(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("未配置表时应返回错误")
	}
}

func TestValidate_EmptyTableName(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Tables: []TableConfig{{Name: ""}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("表名为空时应返回错误")
	}
}

func TestValidate_InvalidMode(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Tables: []TableConfig{{Name: "T", Mode: "badmode"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("无效 mode 应返回错误")
	}
}

func TestValidate_IncrementRequiresWatermark(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Tables: []TableConfig{{Name: "T", Mode: "incremental"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("增量模式缺少水位列应返回错误")
	}
}

func TestValidate_FullModeEmptyModeIsOK(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Tables: []TableConfig{{Name: "T", Mode: ""}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("mode 为空应默认 full, 不应报错, got %v", err)
	}
}

func TestValidate_DefaultDialects(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Host: "x", Username: "x", Service: "x"},
		Target: DBConfig{Host: "x", Username: "x", Database: "x"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("未指定 dialect 应默认 oracle/mysql, 不应报错, got %v", err)
	}
	if cfg.Source.Dialect != "oracle" {
		t.Errorf("source 默认 dialect 应为 oracle, got %q", cfg.Source.Dialect)
	}
	if cfg.Target.Dialect != "mysql" {
		t.Errorf("target 默认 dialect 应为 mysql, got %q", cfg.Target.Dialect)
	}
}

func TestTargetName_UsesTarget(t *testing.T) {
	tc := TableConfig{Name: "SRC", Target: "DST"}
	if got := tc.TargetName(); got != "DST" {
		t.Errorf("TargetName 应为 DST, got %q", got)
	}
}

func TestTargetName_FallsBackToName(t *testing.T) {
	tc := TableConfig{Name: "SRC"}
	if got := tc.TargetName(); got != "SRC" {
		t.Errorf("Target 为空时应回退到 Name, got %q", got)
	}
}

func TestSourceRef_WithTableSchema(t *testing.T) {
	tc := TableConfig{Name: "USERS", Schema: "HR"}
	if got := tc.SourceRef(""); got != "HR.USERS" {
		t.Errorf("应使用表级 schema, got %q", got)
	}
}

func TestSourceRef_WithGlobalSchema(t *testing.T) {
	tc := TableConfig{Name: "USERS"}
	if got := tc.SourceRef("APP"); got != "APP.USERS" {
		t.Errorf("应回退到全局 schema, got %q", got)
	}
}

func TestSourceRef_NoSchema(t *testing.T) {
	tc := TableConfig{Name: "USERS"}
	if got := tc.SourceRef(""); got != "USERS" {
		t.Errorf("无 schema 时应仅返回表名, got %q", got)
	}
}

func TestSourceRef_TableSchemaOverridesGlobal(t *testing.T) {
	tc := TableConfig{Name: "USERS", Schema: "HR"}
	if got := tc.SourceRef("APP"); got != "HR.USERS" {
		t.Errorf("表级 schema 应覆盖全局, got %q", got)
	}
}

func TestSourceSchema_PrefersTable(t *testing.T) {
	tc := TableConfig{Schema: "HR"}
	if got := tc.SourceSchema("APP"); got != "HR" {
		t.Errorf("应优先表级 schema, got %q", got)
	}
}

func TestSourceSchema_FallsBackToGlobal(t *testing.T) {
	tc := TableConfig{}
	if got := tc.SourceSchema("APP"); got != "APP" {
		t.Errorf("应回退到全局 schema, got %q", got)
	}
}

func TestSourceSchema_BothEmpty(t *testing.T) {
	tc := TableConfig{}
	if got := tc.SourceSchema(""); got != "" {
		t.Errorf("都为空时应返回空串, got %q", got)
	}
}

func TestResolveMode_DefaultFull(t *testing.T) {
	tc := TableConfig{}
	if got := tc.ResolveMode(); got != "full" {
		t.Errorf("空 mode 应返回 full, got %q", got)
	}
}

func TestResolveMode_PreservesIncremental(t *testing.T) {
	tc := TableConfig{Mode: "incremental"}
	if got := tc.ResolveMode(); got != "incremental" {
		t.Errorf("应保留 incremental, got %q", got)
	}
}

func TestResolveBatchSize_TablePriority(t *testing.T) {
	tc := TableConfig{BatchSize: 500}
	if got := tc.ResolveBatchSize(1000); got != 500 {
		t.Errorf("表级批大小优先, got %d", got)
	}
}

func TestResolveBatchSize_GlobalFallback(t *testing.T) {
	tc := TableConfig{}
	if got := tc.ResolveBatchSize(2000); got != 2000 {
		t.Errorf("应回退到全局默认, got %d", got)
	}
}

func TestResolveBatchSize_UltimateDefault(t *testing.T) {
	tc := TableConfig{}
	if got := tc.ResolveBatchSize(0); got != 1000 {
		t.Errorf("最终默认值应为 1000, got %d", got)
	}
}

func TestTargetColName_Mapped(t *testing.T) {
	tc := TableConfig{ColumnMapping: map[string]string{"SRC_ID": "id", "SRC_NAME": "name"}}
	if got := tc.TargetColName("SRC_ID"); got != "id" {
		t.Errorf("SRC_ID → id, got %q", got)
	}
}

func TestTargetColName_NoMapping(t *testing.T) {
	tc := TableConfig{ColumnMapping: map[string]string{"SRC_ID": "id"}}
	if got := tc.TargetColName("OTHER"); got != "OTHER" {
		t.Errorf("未映射列应返回原名, got %q", got)
	}
}

func TestMapColumnNames_AllMapped(t *testing.T) {
	tc := TableConfig{ColumnMapping: map[string]string{"A": "aa", "B": "bb"}}
	got := tc.MapColumnNames([]string{"A", "B"})
	if got[0] != "aa" || got[1] != "bb" {
		t.Errorf("批量映射失败: %v", got)
	}
}

func TestSourceColName_Reverse(t *testing.T) {
	tc := TableConfig{ColumnMapping: map[string]string{"SRC_C1": "my_c1", "SRC_C2": "my_c2"}}
	if got := tc.SourceColName("my_c1"); got != "SRC_C1" {
		t.Errorf("反向映射 my_c1 → SRC_C1, got %q", got)
	}
}

func TestSourceColName_NoMapping(t *testing.T) {
	tc := TableConfig{ColumnMapping: nil}
	if got := tc.SourceColName("x"); got != "x" {
		t.Errorf("无映射时反向查找应返回原名, got %q", got)
	}
}

func TestResolveAutoValidate_DefaultTrue(t *testing.T) {
	cfg := &Config{}
	if got := cfg.ResolveAutoValidate(); !got {
		t.Error("AutoValidate 未设置时应默认 true")
	}
}

func TestResolveAutoValidate_ExplicitTrue(t *testing.T) {
	v := true
	cfg := &Config{AutoValidate: &v}
	if got := cfg.ResolveAutoValidate(); !got {
		t.Error("AutoValidate=true 应返回 true")
	}
}

func TestResolveAutoValidate_ExplicitFalse(t *testing.T) {
	v := false
	cfg := &Config{AutoValidate: &v}
	if got := cfg.ResolveAutoValidate(); got {
		t.Error("AutoValidate=false 应返回 false")
	}
}

func TestResolveValidateSampleSize_Positive(t *testing.T) {
	cfg := &Config{ValidateSampleSize: 500}
	if got := cfg.ResolveValidateSampleSize(); got != 500 {
		t.Errorf("应返回配置值 500, got %d", got)
	}
}

func TestResolveValidateSampleSize_ZeroFallsBackToDefault(t *testing.T) {
	cfg := &Config{ValidateSampleSize: 0}
	if got := cfg.ResolveValidateSampleSize(); got != 100 {
		t.Errorf("<=0 时应回退默认 100, got %d", got)
	}
}

func TestResolveValidateSampleSize_NegativeFallsBackToDefault(t *testing.T) {
	cfg := &Config{ValidateSampleSize: -1}
	if got := cfg.ResolveValidateSampleSize(); got != 100 {
		t.Errorf("负数应回退默认 100, got %d", got)
	}
}

func TestResolveSampleOffset_TablePriority(t *testing.T) {
	tc := TableConfig{ValidateSampleOffset: 200}
	if got := tc.ResolveSampleOffset(50); got != 200 {
		t.Errorf("表级 offset 应优先, got %d", got)
	}
}

func TestResolveSampleOffset_GlobalFallback(t *testing.T) {
	tc := TableConfig{ValidateSampleOffset: 0}
	if got := tc.ResolveSampleOffset(50); got != 50 {
		t.Errorf("表级为 0 时应回退全局 %d, got %d", 50, got)
	}
}

func TestResolveSampleOffset_TableZeroGlobalZero(t *testing.T) {
	tc := TableConfig{ValidateSampleOffset: 0}
	if got := tc.ResolveSampleOffset(0); got != 0 {
		t.Errorf("都为 0 时应返回 0, got %d", got)
	}
}

func TestResolveSampleRandom_TableTrue(t *testing.T) {
	tc := TableConfig{ValidateSampleRandom: true}
	if got := tc.ResolveSampleRandom(false); !got {
		t.Error("表级 random=true 应返回 true")
	}
}

func TestResolveSampleRandom_TableFalseGlobalTrue(t *testing.T) {
	tc := TableConfig{ValidateSampleRandom: false}
	if got := tc.ResolveSampleRandom(true); !got {
		t.Error("表级未设, 全局 true 时应返回 true")
	}
}

func TestResolveSampleRandom_BothFalse(t *testing.T) {
	tc := TableConfig{ValidateSampleRandom: false}
	if got := tc.ResolveSampleRandom(false); got {
		t.Error("都为 false 时应返回 false")
	}
}

func TestResolveWatermarkCheckpointInterval_Default(t *testing.T) {
	cfg := &Config{WatermarkCheckpointInterval: 0}
	if got := cfg.ResolveWatermarkCheckpointInterval(); got != 10*time.Second {
		t.Errorf("<=0 时应默认 10s, got %v", got)
	}
}

func TestResolveWatermarkCheckpointInterval_Custom(t *testing.T) {
	cfg := &Config{WatermarkCheckpointInterval: 30}
	if got := cfg.ResolveWatermarkCheckpointInterval(); got != 30*time.Second {
		t.Errorf("应返回 30s, got %v", got)
	}
}

func TestResolveWatermarkCheckpointInterval_Negative(t *testing.T) {
	cfg := &Config{WatermarkCheckpointInterval: -5}
	if got := cfg.ResolveWatermarkCheckpointInterval(); got != 10*time.Second {
		t.Errorf("负数应回退默认 10s, got %v", got)
	}
}

func TestValidate_MySQLSourceRequiresDatabase(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "mysql", Host: "x", Username: "x"},
		Target: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("MySQL 源缺少 database 应返回错误")
	}
}

func TestValidate_OracleTargetRequiresService(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Target: DBConfig{Dialect: "oracle", Host: "x", Username: "x"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Oracle 目标缺少 service 应返回错误")
	}
}

func TestValidate_OracleTargetWithMySQLStateBackendError(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Target: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		State:  StateConfig{Backend: "mysql"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("目标库非 MySQL 时 state.backend=mysql 应返回错误")
	}
}

func TestValidate_MySQLToOracle_Valid(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Target: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		State:  StateConfig{Backend: "sqlite"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("合法的 MySQL→Oracle 配置不应报错, got %v", err)
	}
}

// 关键新能力：目标为 Oracle 时，只要配了独立进度 MySQL（state.mysql），
// state.backend=mysql 即可使用（进度不再依赖目标库是 MySQL）。
func TestValidate_StateMySQLBackendWithOracleTargetOK(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Target: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		State: StateConfig{
			Backend: "mysql",
			MySQL:   StateMySQLConfig{Host: "10.0.0.99", Username: "sync", Database: "godbsync_state"},
		},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("目标为 Oracle 且配了独立进度 MySQL 时不应报错, got %v", err)
	}
}

// state.backend=mysql 但 state.mysql 缺 username/database 应报错。
func TestValidate_StateMySQLBackendMissingFields(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Target: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		State: StateConfig{
			Backend: "mysql",
			MySQL:   StateMySQLConfig{Host: "10.0.0.99"}, // 缺 username/database
		},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("state.mysql 缺 username/database 应返回错误")
	}
}

// 向后兼容：未配 state.mysql 且目标本身是 MySQL 时，仍可回退复用目标连接。
func TestValidate_StateMySQLBackendFallbackToTargetMySQL(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "oracle", Host: "x", Username: "x", Service: "x"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		State:  StateConfig{Backend: "mysql"}, // 无 state.mysql，目标为 mysql -> 回退
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("目标为 MySQL 且未配 state.mysql 时应回退复用目标连接, got %v", err)
	}
}

func TestStateMySQLDSN_Defaults(t *testing.T) {
	c := StateMySQLConfig{Host: "10.0.0.99", Username: "sync", Password: "p", Database: "s"}
	dsn := c.DSN()
	for _, want := range []string{"charset=utf8mb4", ":3306)", "parseTime=true", "loc=UTC"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("独立进度 MySQL DSN 应包含 %s, got %q", want, dsn)
		}
	}
}

// ---- SQL Server ----

func TestSQLServerDSN_DefaultPort(t *testing.T) {
	c := DBConfig{Dialect: "sqlserver", Host: "10.0.0.1", Username: "sa", Password: "pass", Database: "testdb"}
	dsn := c.DSN()
	if !strings.Contains(dsn, "port=1433") {
		t.Errorf("默认端口应为 1433, got %q", dsn)
	}
	if !strings.Contains(dsn, "encrypt=disable") {
		t.Errorf("应包含 encrypt=disable, got %q", dsn)
	}
	if !strings.Contains(dsn, "database=testdb") {
		t.Errorf("应包含 database=testdb, got %q", dsn)
	}
}

func TestSQLServerDSN_Encrypt(t *testing.T) {
	// 默认 encrypt=disable（兼容自签名/内网）。
	c := DBConfig{Dialect: "sqlserver", Host: "h", Port: 1433, Username: "u", Password: "p", Database: "db"}
	if dsn := c.DSN(); !strings.Contains(dsn, "encrypt=disable") {
		t.Errorf("默认应 encrypt=disable, got %q", dsn)
	}
	// encrypt=true -> 启用加密并信任服务器证书（适配 Azure/强制加密实例）。
	c2 := DBConfig{Dialect: "sqlserver", Host: "h", Username: "u", Password: "p", Database: "db", Encrypt: "true"}
	if dsn := c2.DSN(); !strings.Contains(dsn, "encrypt=true;trustservercertificate=true") {
		t.Errorf("encrypt=true 应生成 encrypt=true;trustservercertificate=true, got %q", dsn)
	}
	// 其它取值原样透传（如 strict）。
	c3 := DBConfig{Dialect: "sqlserver", Host: "h", Username: "u", Password: "p", Database: "db", Encrypt: "strict"}
	if dsn := c3.DSN(); !strings.Contains(dsn, "encrypt=strict") {
		t.Errorf("encrypt=strict 应原样透传, got %q", dsn)
	}
}

func TestValidate_SQLServerSourceRequiresDatabase(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "sqlserver", Host: "x", Username: "sa"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		State:  StateConfig{Backend: "sqlite"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("source.dialect=sqlserver 缺 database 应报错")
	}
}

func TestValidate_SQLServerTargetRequiresDatabase(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "x"},
		Target: DBConfig{Dialect: "sqlserver", Host: "x", Username: "sa"},
		State:  StateConfig{Backend: "sqlite"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("target.dialect=sqlserver 缺 database 应报错")
	}
}

func TestValidate_SQLServerToMySQL_Valid(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "sqlserver", Host: "x", Username: "sa", Database: "src"},
		Target: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "tgt"},
		State:  StateConfig{Backend: "sqlite"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("合法的 SQL Server->MySQL 配置不应报错, got %v", err)
	}
}

func TestValidate_MySQLToSQLServer_Valid(t *testing.T) {
	cfg := &Config{
		Source: DBConfig{Dialect: "mysql", Host: "x", Username: "x", Database: "src"},
		Target: DBConfig{Dialect: "sqlserver", Host: "x", Username: "sa", Database: "tgt"},
		State:  StateConfig{Backend: "sqlite"},
		Tables: []TableConfig{{Name: "T"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("合法的 MySQL->SQL Server 配置不应报错, got %v", err)
	}
}
