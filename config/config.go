package config

import (
	"fmt"
	"io/ioutil"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultBatchTimeout = 5 * time.Minute

// DBConfig 是统一的数据库连接配置，通过 Dialect 字段区分源/目标数据库类型。
// 不同方言使用各自需要的字段，其余字段忽略。
type DBConfig struct {
	Dialect  string `yaml:"dialect"`   // "oracle"、"mysql"、"sqlserver" 或 "postgresql"
	Host     string `yaml:"host"`      // 主机地址
	Port     int    `yaml:"port"`      // 端口
	Username string `yaml:"username"`  // 用户名
	Password string `yaml:"password"`  // 密码
	Service  string `yaml:"service"`   // Oracle: 服务名或 SID
	ConnType string `yaml:"conn_type"` // Oracle: "service"（默认）或 "sid"
	Schema   string `yaml:"schema"`    // Oracle/SQL Server: 源表缺省属主（schema）
	Database string `yaml:"database"`  // MySQL/SQL Server: 源/目标数据库名
	Charset  string `yaml:"charset"`   // MySQL: 字符集（默认 utf8mb4）
	Timezone string `yaml:"timezone"`  // MySQL: 时间列存储时区（默认 UTC）
	Encrypt  string `yaml:"encrypt"`   // SQL Server: 连接加密模式，留空=disable（兼容自签名）；true=启用加密并信任服务器证书
}

// DSN 根据 Dialect 生成对应驱动的连接串。
func (c DBConfig) DSN() string {
	switch strings.ToLower(c.Dialect) {
	case "oracle":
		return c.oracleDSN()
	case "mysql":
		return c.mysqlDSN()
	case "sqlserver":
		return c.sqlserverDSN()
	case "postgresql":
		return c.postgresqlDSN()
	default:
		return ""
	}
}

// defaultOraclePrefetch 推断的 Oracle 数组抓取行数（基于默认 batch_size）。
// go-ora 默认仅 25 行/次抓取，反向同步（Oracle 作源）会因频繁网络往返而极慢；
// 这里按默认批次大小推断，让 Oracle 源读取一次抓取一整批。
const defaultOraclePrefetch = 1000

func (c DBConfig) oracleDSN() string {
	return c.oracleDSNWithPrefetch(defaultOraclePrefetch)
}

// oracleDSNWithPrefetch 生成带数组抓取大小（PREFETCH_ROWS）的 Oracle 连接串。
// prefetch<=0 时不附加该参数（回退驱动默认 25）。service 与 sid 两种模式均支持。
func (c DBConfig) oracleDSNWithPrefetch(prefetch int) string {
	user := url.QueryEscape(c.Username)
	pass := url.QueryEscape(c.Password)
	port := c.Port
	if port == 0 {
		port = 1521
	}
	var extra string
	if prefetch > 0 {
		extra = fmt.Sprintf("PREFETCH_ROWS=%d", prefetch)
	}
	if strings.EqualFold(c.ConnType, "sid") {
		base := fmt.Sprintf("oracle://%s:%s@%s:%d/?SID=%s", user, pass, c.Host, port, url.QueryEscape(c.Service))
		if extra != "" {
			return base + "&" + extra
		}
		return base
	}
	base := fmt.Sprintf("oracle://%s:%s@%s:%d/%s", user, pass, c.Host, port, c.Service)
	if extra != "" {
		return base + "?" + extra
	}
	return base
}

// DSNWithPrefetch 在 Oracle 源连接串上按 batch_size 推断数组抓取大小；
// 非 Oracle 方言原样返回 DSN（prefetch 对其无语义）。用于避免新增配置项即可提速反向同步。
func (c DBConfig) DSNWithPrefetch(prefetch int) string {
	if strings.EqualFold(c.Dialect, "oracle") {
		return c.oracleDSNWithPrefetch(prefetch)
	}
	return c.DSN()
}

// buildMySQLDSN 拼接 go-sql-driver 的 MySQL 连接串（参数风格与 DBConfig.mysqlDSN 一致）。
// 抽成公共函数，供 DBConfig 与独立的进度库配置 StateMySQLConfig 共用，避免重复。
func buildMySQLDSN(host string, port int, user, pass, db, charset, tz string) string {
	if charset == "" {
		charset = "utf8mb4"
	}
	if port == 0 {
		port = 3306
	}
	if tz == "" {
		tz = "UTC"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=true&loc=%s&multiStatements=false"+
		"&writeTimeout=120s&readTimeout=120s&timeout=30s",
		user, pass, host, port, db, charset, tz)
}

func (c DBConfig) mysqlDSN() string {
	return buildMySQLDSN(c.Host, c.Port, c.Username, c.Password, c.Database, c.Charset, c.Timezone)
}

// sqlserverDSN 生成 go-mssqldb 的连接串（ADO 风格，避免 URL 编码问题）。
// encrypt 留空时默认 encrypt=disable（兼容自签名证书/内网环境）；
// 配置 encrypt: true 时启用加密并信任服务器证书（适配 Azure SQL 或开启 Force Encryption 的实例）。
func (c DBConfig) sqlserverDSN() string {
	port := c.Port
	if port == 0 {
		port = 1433
	}
	enc := "encrypt=disable"
	if c.Encrypt != "" {
		if strings.EqualFold(c.Encrypt, "true") || strings.EqualFold(c.Encrypt, "enable") || strings.EqualFold(c.Encrypt, "yes") {
			enc = "encrypt=true;trustservercertificate=true"
		} else {
			enc = "encrypt=" + c.Encrypt
		}
	}
	return fmt.Sprintf("server=%s;port=%d;user id=%s;password=%s;database=%s;%s",
		c.Host, port, c.Username, c.Password, c.Database, enc)
}

// postgresqlDSN 生成 lib/pq 的连接串（URL 形式，用户名/密码做 URL 编码）。
// sslmode 默认 disable（兼容自签名/内网，与 SQL Server 默认行为一致）；
// 配置 Encrypt 字段可覆盖为 require/verify-ca/verify-full（复用该字段，语义即 PG 的 sslmode）。
func (c DBConfig) postgresqlDSN() string {
	port := c.Port
	if port == 0 {
		port = 5432
	}
	sslmode := "disable"
	if c.Encrypt != "" {
		sslmode = c.Encrypt
	}
	user := url.UserPassword(c.Username, c.Password)
	u := url.URL{
		Scheme: "postgres",
		User:   user,
		Host:   fmt.Sprintf("%s:%d", c.Host, port),
		Path:   "/" + c.Database,
	}
	q := u.Query()
	q.Set("sslmode", sslmode)
	q.Set("connect_timeout", "30")
	u.RawQuery = q.Encode()
	return u.String()
}

// TableConfig 单张表的同步配置。
type TableConfig struct {
	Name                 string            `yaml:"name"`
	Schema               string            `yaml:"schema"`
	Target               string            `yaml:"target"`
	Mode                 string            `yaml:"mode"`
	Watermark            string            `yaml:"watermark"`
	BatchSize            int               `yaml:"batch_size"`
	Truncate             bool              `yaml:"truncate"`
	IgnoreErrors         bool              `yaml:"ignore_errors"`
	ValidateSampleOffset int               `yaml:"validate_sample_offset"`
	ValidateSampleRandom bool              `yaml:"validate_sample_random"`
	ColumnMapping        map[string]string `yaml:"column_mapping"`
}

// StateMySQLConfig 是「独立进度 MySQL」的连接配置。
// 与 source/target 解耦：无论目标库是什么（含 Oracle），只要在这里配好连接，
// 进度（水位表/断点表）就能存进这台独立 MySQL，不再要求目标库本身是 MySQL。
type StateMySQLConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
	Charset  string `yaml:"charset"`
	Timezone string `yaml:"timezone"`
}

// DSN 生成独立进度 MySQL 的连接串。
func (c StateMySQLConfig) DSN() string {
	return buildMySQLDSN(c.Host, c.Port, c.Username, c.Password, c.Database, c.Charset, c.Timezone)
}

type StateConfig struct {
	Backend string           `yaml:"backend"`
	Path    string           `yaml:"path"`
	MySQL   StateMySQLConfig `yaml:"mysql"`
}

// Config 是同步任务的顶层配置，聚合 source/target 两个数据库连接、进度后端与待同步表列表。
// 由 YAML 反序列化得到，运行前须经 Validate 校验。
type Config struct {
	Source                      DBConfig      `yaml:"source"`
	Target                      DBConfig      `yaml:"target"`
	State                       StateConfig   `yaml:"state"`
	Tables                      []TableConfig `yaml:"tables"`
	Workers                     int           `yaml:"workers"`
	LogLevel                    string        `yaml:"log_level"`
	AutoValidate                *bool         `yaml:"auto_validate"`
	ValidateSampleSize          int           `yaml:"validate_sample_size"`
	ValidateSampleOffset        int           `yaml:"validate_sample_offset"`
	ValidateSampleRandom        bool          `yaml:"validate_sample_random"`
	WatermarkCheckpointInterval int           `yaml:"watermark_checkpoint_interval"`
	BatchTimeout                string        `yaml:"batch_timeout"`
	BatchSize                   int           `yaml:"batch_size"`
	// OracleArrayBinding 控制 Oracle 目标写入是否使用数组绑定（默认 true）。
	// 设为 false 可回退到传统 UNION ALL 行展开写法（性能略低但兼容性更好，已充分验证）。
	OracleArrayBinding *bool `yaml:"oracle_array_binding"`
}

func Load(path string) (*Config, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validIdentifier 校验单个 SQL 标识符（库名/表名/列名/schema/owner）。
// 仅允许 ASCII 字母、数字与下划线；长度限制 1..63（主流数据库标识符上限）。
// 排除引号、空格、分号等所有特殊字符，从根源杜绝标识符注入与 `..` 路径穿越。
func validIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_') {
			return false
		}
	}
	return true
}

// validQualifiedName 校验可带单级点号的限定名，如 `schema.table` / `owner.table` / `db.table`。
// 各段必须是合法标识符，且不允许空段或连续点号（禁止 `..` 路径穿越）。
func validQualifiedName(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !validIdentifier(p) {
			return false
		}
	}
	return true
}

// validHost 校验数据库连接主机（IP/域名/socket 路径）。
// 禁止空白、引号、分号、注释符等可用于 DSN/连接串注入的字符；
// 允许字母、数字、点、冒号、连字符、下划线与 Unix socket 的斜杠。
func validHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			b == '.', b == ':', b == '-', b == '_', b == '/', b == '[', b == ']':
			// 允许（方括号仅用于 IPv6 字面量定界，不进 SQL 标识符，无注入风险）
		default:
			return false
		}
	}
	return true
}

// validServiceName 校验 Oracle service name 或 SID。
// 允许字母、数字、下划线、点（service name 可含域）、美元符与井号（SID 允许）；
// 禁止空白、引号、分号等注入字符。
func validServiceName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			b == '.', b == '_', b == '$', b == '#', b == '-':
			// 允许
		default:
			return false
		}
	}
	return true
}

// validateDBIdentifiers 校验单个 DBConfig 中会被拼进 SQL/DSN 的标识符类字段。
// 空字段跳过（未指定即合法）；非空字段按其在对应方言中的语义校验格式。
func validateDBIdentifiers(role string, c DBConfig) error {
	// Database 仅对 mysql/sqlserver/postgresql 有效（Oracle 用 Service）。
	switch strings.ToLower(c.Dialect) {
	case "mysql", "sqlserver", "postgresql":
		if c.Database != "" && !validIdentifier(c.Database) {
			return fmt.Errorf("%s.database 含非法字符（仅允许字母、数字、下划线，长度 1..63）", role)
		}
	}
	if c.Schema != "" && !validIdentifier(c.Schema) {
		return fmt.Errorf("%s.schema 含非法字符（仅允许字母、数字、下划线，长度 1..63）", role)
	}
	if c.Service != "" && !validServiceName(c.Service) {
		return fmt.Errorf("%s.service 含非法字符", role)
	}
	return nil
}

func (c *Config) Validate() error {
	if c.Source.Dialect == "" {
		c.Source.Dialect = "oracle"
	}
	if c.Target.Dialect == "" {
		c.Target.Dialect = "mysql"
	}
	if c.Source.Host == "" || c.Source.Username == "" {
		return fmt.Errorf("source 配置不完整（host/username 必填）")
	}
	if c.Target.Host == "" || c.Target.Username == "" {
		return fmt.Errorf("target 配置不完整（host/username 必填）")
	}
	// 标识符白名单校验：防御不可信配置导致的 SQL/DSN 注入。
	if !validHost(c.Source.Host) {
		return fmt.Errorf("source.host 含非法字符（仅允许字母、数字、. : - _ /，不含空白与引号）")
	}
	if !validHost(c.Target.Host) {
		return fmt.Errorf("target.host 含非法字符（仅允许字母、数字、. : - _ /，不含空白与引号）")
	}
	if err := validateDBIdentifiers("source", c.Source); err != nil {
		return err
	}
	if err := validateDBIdentifiers("target", c.Target); err != nil {
		return err
	}
	if c.State.Backend == "mysql" && c.State.MySQL.Host != "" && !validHost(c.State.MySQL.Host) {
		return fmt.Errorf("state.mysql.host 含非法字符（仅允许字母、数字、. : - _ /，不含空白与引号）")
	}
	if c.State.Backend == "mysql" && c.State.MySQL.Database != "" && !validIdentifier(c.State.MySQL.Database) {
		return fmt.Errorf("state.mysql.database 含非法字符（仅允许字母、数字、下划线，长度 1..63）")
	}
	if c.Source.Dialect == "oracle" && c.Source.Service == "" {
		return fmt.Errorf("source.dialect=oracle 时 service 必填")
	}
	if c.Source.Dialect == "mysql" && c.Source.Database == "" {
		return fmt.Errorf("source.dialect=mysql 时 database 必填")
	}
	if c.Source.Dialect == "sqlserver" && c.Source.Database == "" {
		return fmt.Errorf("source.dialect=sqlserver 时 database 必填")
	}
	if c.Source.Dialect == "postgresql" && c.Source.Database == "" {
		return fmt.Errorf("source.dialect=postgresql 时 database 必填")
	}
	if c.Target.Dialect == "mysql" && c.Target.Database == "" {
		return fmt.Errorf("target.dialect=mysql 时 database 必填")
	}
	if c.Target.Dialect == "oracle" && c.Target.Service == "" {
		return fmt.Errorf("target.dialect=oracle 时 service 必填")
	}
	if c.Target.Dialect == "sqlserver" && c.Target.Database == "" {
		return fmt.Errorf("target.dialect=sqlserver 时 database 必填")
	}
	if c.Target.Dialect == "postgresql" && c.Target.Database == "" {
		return fmt.Errorf("target.dialect=postgresql 时 database 必填")
	}
	if c.State.Backend == "mysql" {
		// 进度 MySQL 现在由独立的 state.mysql 段配置，与目标库解耦：
		// 只要配了独立进度库，任意目标方言（含 Oracle）都能用 mysql 后端。
		// 未配置 state.mysql 时，仅允许回退复用「目标为 MySQL」的连接（向后兼容旧用法）。
		if c.State.MySQL.Host == "" {
			if c.Target.Dialect != "mysql" {
				return fmt.Errorf("state.backend=mysql 但未配置 state.mysql；目标库非 MySQL 时必须在 state.mysql 段填写独立进度库连接（host/username/database）")
			}
		} else if c.State.MySQL.Username == "" || c.State.MySQL.Database == "" {
			return fmt.Errorf("state.mysql 段不完整（username/database 必填）")
		}
	}
	if len(c.Tables) == 0 {
		return fmt.Errorf("未配置任何待同步表（tables 为空）")
	}
	if c.State.Backend != "" && c.State.Backend != "sqlite" && c.State.Backend != "mysql" {
		return fmt.Errorf("state.backend 必须为 sqlite 或 mysql，当前为 %q", c.State.Backend)
	}
	for i, t := range c.Tables {
		if t.Name == "" {
			return fmt.Errorf("tables[%d].name 不能为空", i)
		}
		if !validIdentifier(t.Name) {
			return fmt.Errorf("tables[%d].name 含非法字符（仅允许字母、数字、下划线，长度 1..63）", i)
		}
		if t.Target != "" && !validQualifiedName(t.Target) {
			return fmt.Errorf("tables[%d].target 含非法字符（限定名仅允许 schema.table 形式，每段仅字母数字下划线）", i)
		}
		if t.Schema != "" && !validIdentifier(t.Schema) {
			return fmt.Errorf("tables[%d].schema 含非法字符（仅允许字母、数字、下划线，长度 1..63）", i)
		}
		if t.Watermark != "" && !validIdentifier(t.Watermark) {
			return fmt.Errorf("tables[%d].watermark 含非法字符（仅允许字母、数字、下划线）", i)
		}
		for src, tgt := range t.ColumnMapping {
			if !validIdentifier(src) {
				return fmt.Errorf("tables[%d].column_mapping 的源列名 %q 含非法字符（仅允许字母、数字、下划线）", i, src)
			}
			if !validIdentifier(tgt) {
				return fmt.Errorf("tables[%d].column_mapping 的目标列名 %q 含非法字符（仅允许字母、数字、下划线）", i, tgt)
			}
		}
		mode := t.Mode
		if mode == "" {
			mode = "full"
		}
		if mode != "full" && mode != "incremental" {
			return fmt.Errorf("tables[%d].mode 必须为 full 或 incremental，当前为 %q", i, t.Mode)
		}
		if mode == "incremental" && t.Watermark == "" {
			return fmt.Errorf("tables[%d] 为增量模式，必须指定 watermark 列", i)
		}
	}
	return nil
}

func (t TableConfig) TargetName() string {
	if t.Target != "" {
		return t.Target
	}
	return t.Name
}

func (t TableConfig) SourceRef(globalSchema string) string {
	sch := t.Schema
	if sch == "" {
		sch = globalSchema
	}
	if sch == "" {
		return t.Name
	}
	return sch + "." + t.Name
}

func (t TableConfig) SourceSchema(globalSchema string) string {
	if t.Schema != "" {
		return t.Schema
	}
	return globalSchema
}

func (t TableConfig) ResolveMode() string {
	if t.Mode == "" {
		return "full"
	}
	return t.Mode
}

func (t TableConfig) ResolveBatchSize(globalDefault int) int {
	if t.BatchSize > 0 {
		return t.BatchSize
	}
	if globalDefault > 0 {
		return globalDefault
	}
	return 1000
}

// ResolveBatchSize 返回全局批量大小：未配置时回退默认 1000。表级 batch_size 优先于它。
func (c *Config) ResolveBatchSize() int {
	if c.BatchSize > 0 {
		return c.BatchSize
	}
	return 1000
}

// ResolveOracleArrayBinding 返回是否启用 Oracle 数组绑定，未配置默认 true。
func (c *Config) ResolveOracleArrayBinding() bool {
	if c.OracleArrayBinding != nil {
		return *c.OracleArrayBinding
	}
	return true
}

func (c *Config) ResolveAutoValidate() bool {
	if c.AutoValidate != nil {
		return *c.AutoValidate
	}
	return true
}

func (c *Config) ResolveValidateSampleSize() int {
	if c.ValidateSampleSize > 0 {
		return c.ValidateSampleSize
	}
	return 100
}

func (t TableConfig) ResolveSampleOffset(global int) int {
	if t.ValidateSampleOffset > 0 {
		return t.ValidateSampleOffset
	}
	return global
}

func (t TableConfig) TargetColName(sourceName string) string {
	if mapped, ok := t.ColumnMapping[sourceName]; ok {
		return mapped
	}
	return sourceName
}

func (t TableConfig) MapColumnNames(sourceNames []string) []string {
	result := make([]string, len(sourceNames))
	for i, n := range sourceNames {
		result[i] = t.TargetColName(n)
	}
	return result
}

func (t TableConfig) SourceColName(targetName string) string {
	for src, tgt := range t.ColumnMapping {
		if strings.EqualFold(tgt, targetName) {
			return src
		}
	}
	return targetName
}

func (t TableConfig) ResolveSampleRandom(global bool) bool {
	if t.ValidateSampleRandom {
		return true
	}
	return global
}

func (c *Config) ResolveWatermarkCheckpointInterval() time.Duration {
	if c.WatermarkCheckpointInterval > 0 {
		return time.Duration(c.WatermarkCheckpointInterval) * time.Second
	}
	return 10 * time.Second
}

func (c *Config) ResolveStateSpec(override string) string {
	if override != "" {
		return override
	}
	switch c.State.Backend {
	case "mysql":
		return "mysql"
	case "sqlite":
		p := c.State.Path
		if p == "" {
			p = "godbsync.db"
		}
		return "file=" + p
	default:
		return "file=godbsync.db"
	}
}

func (c *Config) ResolveBatchTimeout() time.Duration {
	if c.BatchTimeout == "" {
		return DefaultBatchTimeout
	}
	if d, err := time.ParseDuration(c.BatchTimeout); err == nil && d > 0 {
		return d
	}
	return DefaultBatchTimeout
}
