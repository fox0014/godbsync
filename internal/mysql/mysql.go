package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"godbsync/internal/dialect"
	"godbsync/internal/types"
)

func init() {
	dialect.RegisterWriter("mysql", func() dialect.Writer { return &Writer{} })
}

// Writer 实现 dialect.Writer 接口，封装 MySQL 目标数据库的写入操作。
type Writer struct{}

func (w *Writer) Dialect() string { return "mysql" }

func (w *Writer) Open(dsn string) (*sql.DB, error) { return Open(dsn) }

func (w *Writer) BulkUpsert(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	return bulkUpsertTx(ctx, db, target, cols, rows, upsert)
}

func (w *Writer) Truncate(db *sql.DB, table string) error { return Truncate(db, table) }

func (w *Writer) TableExists(db *sql.DB, database, table string) (bool, error) {
	return TableExists(db, database, table)
}

func (w *Writer) ExecDDL(db *sql.DB, ddl string) error { return ExecDDL(db, ddl) }

func (w *Writer) GenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	return GenerateCreate(db, table, cols, pk)
}

func (w *Writer) RowCount(db *sql.DB, table string) (int64, error) { return RowCount(db, table) }

func (w *Writer) PrimaryKey(db *sql.DB, database, table string) []string {
	return PrimaryKey(db, database, table)
}

func (w *Writer) QuoteIdentifier(name string) string { return "`" + name + "`" }

func (w *Writer) Placeholder(_ int) string { return "?" }

// maxPlaceholders 是 MySQL 单条预处理语句允许的最大占位符数量（硬上限 65535）。
const maxPlaceholders = 65535

// Open 打开一个 MySQL 连接并配置连接池参数。
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 连接失败: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接 MySQL 失败(Ping): %w", err)
	}
	if _, err := db.Exec("SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return nil, fmt.Errorf("设置 MySQL 隔离级别失败: %w", err)
	}
	return db, nil
}

// ExecDDL 在目标库执行一条 DDL（CREATE/ALTER 等）。
func ExecDDL(db *sql.DB, ddl string) error {
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("执行 DDL 失败: %w\nDDL: %s", err, ddl)
	}
	return nil
}

// RowCount 返回目标表当前行数。
func RowCount(db *sql.DB, table string) (int64, error) {
	var n int64
	q := fmt.Sprintf("SELECT COUNT(*) FROM `%s`", table)
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计目标表 %s 行数失败: %w", table, err)
	}
	return n, nil
}

// BulkUpsert 将一批行批量写入目标表（无超时控制，仅用于串行全量路径）。
// upsert=false 时使用普通 INSERT，适用于全量首跑。
func BulkUpsert(db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	return bulkUpsertTx(nil, db, target, cols, rows, upsert)
}

// BulkUpsertContext 带 context 超时的批量写入。
func BulkUpsertContext(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	return bulkUpsertTx(ctx, db, target, cols, rows, upsert)
}

// bulkUpsertTx 执行批量写入，ctx 为 nil 时不限制超时。
// upsert=true 生成 INSERT ... ON DUPLICATE KEY UPDATE（增量幂等）；
// upsert=false 生成普通多行 INSERT（全量首跑，所有行均为新行）。
func bulkUpsertTx(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	n := len(rows)
	if n == 0 {
		return 0, nil
	}
	perRow := len(cols)
	if perRow == 0 {
		return 0, fmt.Errorf("批量写入 %s 失败: 列数为空", target)
	}

	maxRows := maxPlaceholders / perRow
	if maxRows < 1 {
		maxRows = 1
	}

	quoted := make([]string, perRow)
	for i, c := range cols {
		quoted[i] = "`" + c + "`"
	}
	colList := strings.Join(quoted, ", ")
	placeholders := "(" + strings.Repeat("?,", perRow-1) + "?)"

	updates := make([]string, perRow)
	for i, c := range quoted {
		updates[i] = c + "=VALUES(" + c + ")"
	}
	updateClause := strings.Join(updates, ", ")

	var tx *sql.Tx
	var err error
	if ctx != nil {
		tx, err = db.BeginTx(ctx, nil)
	} else {
		tx, err = db.Begin()
	}
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var total int64
	for start := 0; start < n; start += maxRows {
		end := start + maxRows
		if end > n {
			end = n
		}
		chunk := rows[start:end]

		parts := make([]string, len(chunk))
		for i := range parts {
			parts[i] = placeholders
		}
		valueClause := strings.Join(parts, ", ")

		sqlStr := fmt.Sprintf("INSERT INTO `%s` (%s) VALUES %s", target, colList, valueClause)
		if upsert {
			sqlStr += " ON DUPLICATE KEY UPDATE " + updateClause
		}

		args := make([]interface{}, 0, len(chunk)*perRow)
		for _, r := range chunk {
			for _, v := range r {
				args = append(args, v)
			}
		}
		if ctx != nil {
			_, err = tx.ExecContext(ctx, sqlStr, args...)
		} else {
			_, err = tx.Exec(sqlStr, args...)
		}
		if err != nil {
			return total, fmt.Errorf("批量写入 %s 失败: %w", target, err)
		}
		total += int64(len(chunk))
	}

	if err = tx.Commit(); err != nil {
		return total, fmt.Errorf("提交事务失败: %w", err)
	}
	return total, nil
}

// PrimaryKey 查询 MySQL 目标表的主键列（按序位排列）。
func PrimaryKey(db *sql.DB, database, table string) []string {
	q := `SELECT COLUMN_NAME
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_KEY = 'PRI'
		ORDER BY ORDINAL_POSITION`
	rows, err := db.Query(q, database, table)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil
		}
		pk = append(pk, col)
	}
	return pk
}

// TableExists 判断 MySQL 目标库中指定表是否存在。
func TableExists(db *sql.DB, database, table string) (bool, error) {
	q := `SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`
	var n int
	if err := db.QueryRow(q, database, table).Scan(&n); err != nil {
		return false, fmt.Errorf("查询表 %s 是否存在失败: %w", table, err)
	}
	return n > 0, nil
}

// Truncate 清空目标表（全量同步前可选）。
func Truncate(db *sql.DB, table string) error {
	q := fmt.Sprintf("TRUNCATE TABLE `%s`", table)
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("清空表 %s 失败: %w", table, err)
	}
	return nil
}

// ----------------------------------------------------------------
// 类型映射（原 mapping 包逻辑）
// ----------------------------------------------------------------

// MapType 将源列类型映射为对应的 MySQL 列类型定义（含长度/精度）。
// 等效于按现代版本（MySQL 5.7+）映射，保留历史行为。
func MapType(c types.Column) (string, error) {
	return mysqlTypeForVersion(c, mysqlVersion{major: 8, minor: 0, patch: 0})
}

// mysqlVersion 记录目标 MySQL/MariaDB 的服务端版本，用于按版本自动降级 DDL。
type mysqlVersion struct{ major, minor, patch int }

// atLeast 判断版本是否不低于 major.minor.patch。
func (v mysqlVersion) atLeast(major, minor, patch int) bool {
	if v.major != major {
		return v.major > major
	}
	if v.minor != minor {
		return v.minor > minor
	}
	return v.patch >= patch
}

// mysqlVerCache 按 *sql.DB 缓存探测到的版本，避免每次建表都查 VERSION()。
var mysqlVerCache sync.Map

// detectMySQLVersion 探测目标 MySQL/MariaDB 版本（缓存于 db 指针）。
// db 为 nil（如测试）或探测失败时使用现代版本默认值，保证已测的 5.7+/8.0 路径不变。
func detectMySQLVersion(db *sql.DB) mysqlVersion {
	def := mysqlVersion{major: 8, minor: 0, patch: 0}
	if db == nil {
		return def
	}
	if v, ok := mysqlVerCache.Load(db); ok {
		return v.(mysqlVersion)
	}
	v := def
	var raw string
	if err := db.QueryRow("SELECT VERSION()").Scan(&raw); err == nil {
		if parsed, ok := parseMySQLVersion(raw); ok {
			v = parsed
		}
	}
	mysqlVerCache.Store(db, v)
	return v
}

// parseMySQLVersion 解析 MySQL/MariaDB 的 VERSION() 输出。
// MySQL 形如 "8.0.36" 或 "8.0.36-0ubuntu0.20.04.1"；MariaDB 形如
// "5.5.5-10.11.6-MariaDB-..."（5.5.5 为兼容前缀）或 "10.11.6-MariaDB-..."。
func parseMySQLVersion(raw string) (mysqlVersion, bool) {
	s := raw
	low := strings.ToLower(raw)
	if strings.Contains(low, "mariadb") {
		parts := strings.Split(raw, "-")
		if len(parts) >= 2 && parts[0] == "5.5.5" {
			s = parts[1]
		} else {
			s = parts[0]
		}
	} else if i := strings.Index(s, "-"); i >= 0 {
		s = s[:i]
	}
	nums := strings.Split(s, ".")
	var v mysqlVersion
	if len(nums) >= 1 {
		fmt.Sscanf(nums[0], "%d", &v.major)
	}
	if len(nums) >= 2 {
		fmt.Sscanf(nums[1], "%d", &v.minor)
	}
	if len(nums) >= 3 {
		fmt.Sscanf(nums[2], "%d", &v.patch)
	}
	if v.major == 0 && v.minor == 0 && v.patch == 0 {
		return v, false
	}
	return v, true
}

// mysqlCharset 按版本选择默认字符集：5.5.3+ 用 utf8mb4，更早用 utf8（3 字节）。
func mysqlCharset(ver mysqlVersion) string {
	if ver.atLeast(5, 5, 3) {
		return "utf8mb4"
	}
	return "utf8"
}

func mysqlTypeForVersion(c types.Column, ver mysqlVersion) (string, error) {
	// MySQL→MySQL 保真：直接沿用源列 COLUMN_TYPE 原文（varchar(32)/int unsigned/enum(...) 等），
	// 不再按基础类型名重新映射，避免丢失长度、unsigned、enum 取值等信息。
	if c.RawType != "" {
		return mysqlRawTypeForVersion(c, ver)
	}

	t := strings.ToUpper(c.DBType)
	base := t
	if idx := strings.Index(t, "("); idx >= 0 {
		base = t[:idx]
	}

	// go-sql-driver 对无符号列返回 "UNSIGNED TINYINT"/"UNSIGNED BIGINT" 等类型名，
	// 需拆出基础类型后追加 UNSIGNED 后缀（MySQL 语法为 "INT UNSIGNED"）。
	if inner, ok := strings.CutPrefix(base, "UNSIGNED "); ok {
		cc := c
		cc.RawType = ""
		cc.DBType = inner
		mapped, err := mysqlTypeForVersion(cc, ver)
		if err != nil {
			return "", err
		}
		return mapped + " UNSIGNED", nil
	}

	switch base {
	case "VARCHAR2", "NVARCHAR2", "VARCHAR", "NVARCHAR", "BPCHAR":
		return varcharType(c.Length), nil
	case "CHAR", "NCHAR":
		n := c.Length
		if n <= 0 || n > 255 {
			if n > 255 {
				return "VARCHAR(255)", nil
			}
			return "CHAR(1)", nil
		}
		return fmt.Sprintf("CHAR(%d)", n), nil
	case "NUMBER", "NUMERIC", "DECIMAL", "MONEY", "SMALLMONEY":
		return numberType(c.Precision, c.Scale), nil
	case "INT", "INTEGER", "SMALLINT", "TINYINT", "BIGINT", "MEDIUMINT":
		return base, nil
	case "YEAR":
		return "YEAR", nil
	// PostgreSQL 整数类型：INT2/INT4/INT8 与 serial 序列需映射为 MySQL 对应整数类型
	//（不能用 base 原样返回，INT4 等不是合法 MySQL 类型名）。
	case "INT2", "SMALLSERIAL":
		return "SMALLINT", nil
	case "INT4", "SERIAL":
		return "INT", nil
	case "INT8", "BIGSERIAL":
		return "BIGINT", nil
	case "FLOAT4":
		return "FLOAT", nil
	case "FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE", "DOUBLE", "DOUBLE PRECISION", "REAL", "FLOAT8",
		"BFLOAT", "BDOUBLE", "IBFLOAT", "IBDOUBLE":
		return "DOUBLE", nil
	case "DATE", "OCIDATE", "TIMESTAMP", "TIMESTAMPDTY", "TIMESTAMPTZ_DTY", "TIMESTAMPELTZ",
		"TIMESTAMPLTZ_DTY", "TIMESTAMPTZ", "TIMETZ",
		"TIMESTAMP_WITH_TZ", "TIMESTAMP_WITH_LOCAL_TZ", "DATETIME",
		"DATETIME2", "SMALLDATETIME", "DATETIMEOFFSET",
		"TIME":
		// 微秒精度 DATETIME(6) 需 5.6.4+；更早版本退化为 DATETIME（秒级）。
		if ver.atLeast(5, 6, 4) {
			return "DATETIME(6)", nil
		}
		return "DATETIME", nil
	case "CLOB", "NCLOB", "LONG", "TEXT", "MEDIUMTEXT", "TINYTEXT", "NTEXT", "XML", "CITEXT", "NAME":
		return "LONGTEXT", nil
	case "BLOB", "RAW", "LONG RAW", "TINYBLOB", "MEDIUMBLOB", "BINARY", "VARBINARY", "IMAGE", "BYTEA":
		return "LONGBLOB", nil
	case "INTERVAL YEAR TO MONTH", "INTERVAL DAY TO SECOND", "INTERVAL":
		return "VARCHAR(50)", nil
	case "BOOLEAN", "BOOL", "BIT":
		return "TINYINT(1)", nil
	case "ENUM", "SET":
		return "VARCHAR(255)", nil
	case "JSON", "JSONB":
		// JSON 类型需 5.7+；更早版本退化为 LONGTEXT（MariaDB 10.2+ 支持 JSON 别名，同样命中）。
		if ver.atLeast(5, 7, 0) {
			return "JSON", nil
		}
		return "LONGTEXT", nil
	case "UNIQUEIDENTIFIER", "UUID":
		return "VARCHAR(36)", nil
	default:
		return "", fmt.Errorf("未知源类型: %s（列 %s）", c.DBType, c.Name)
	}
}

// mysqlRawTypeForVersion 校验并返回源列 COLUMN_TYPE 原文，仅按目标版本降级其不支持的语法。
func mysqlRawTypeForVersion(c types.Column, ver mysqlVersion) (string, error) {
	raw := strings.TrimSpace(c.RawType)
	if !validRawType(raw) {
		return "", fmt.Errorf("源列 %s 的类型定义含不安全字符: %q", c.Name, c.RawType)
	}
	base := baseMySQLType(raw)
	// JSON 需 5.7+，更早目标退化为 LONGTEXT（与原映射行为一致）。
	if !ver.atLeast(5, 7, 0) && base == "json" {
		return "LONGTEXT", nil
	}
	// 微秒精度 DATETIME(n)/TIMESTAMP(n)/TIME(n) 需 5.6.4+，更早目标去掉精度参数。
	if !ver.atLeast(5, 6, 4) {
		switch base {
		case "datetime", "timestamp", "time":
			raw = stripTypeArgs(raw)
		}
	}
	return raw, nil
}

// baseMySQLType 取类型基础名（小写，去掉括号参数与 unsigned 等后缀）。
func baseMySQLType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "( "); i >= 0 {
		s = s[:i]
	}
	return s
}

// stripTypeArgs 去掉类型名后的第一个括号参数，如 datetime(6) → datetime、int(11) unsigned → int unsigned。
func stripTypeArgs(s string) string {
	i := strings.Index(s, "(")
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], ")")
	if j < 0 {
		return s
	}
	return s[:i] + s[i+j+1:]
}

// validRawType 校验 COLUMN_TYPE 原文：允许括号、逗号、引号（enum/set 取值）等，
// 但拒绝单引号之外的反引号/分号/换行，避免把异常元数据拼进 DDL 造成语句注入。
func validRawType(s string) bool {
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && inQuote:
			i++ // 跳过被转义的下一个字符
		case ch == '\'':
			inQuote = !inQuote
		case !inQuote && (ch == '`' || ch == ';' || ch == '\n' || ch == '\r'):
			return false
		}
	}
	return !inQuote
}

// mysqlTypesEqual 以大小写/空白/整数显示宽度无关的方式比较两个 MySQL 类型定义。
// 用于 GenerateAlter 比对源列映射结果与目标列 COLUMN_TYPE，避免 int(11) vs INT 之类误报。
func mysqlTypesEqual(a, b string) bool {
	return canonicalMySQLType(a) == canonicalMySQLType(b)
}

func canonicalMySQLType(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	switch baseMySQLType(s) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "year":
		s = stripTypeArgs(s)
	}
	return s
}

func varcharType(length int64) string {
	if length <= 0 {
		return "TEXT"
	}
	if length > 16383 {
		return "LONGTEXT"
	}
	return fmt.Sprintf("VARCHAR(%d)", length)
}

func numberType(precision, scale int) string {
	p := precision
	if p > 65 {
		p = 65
	}
	if scale == 255 {
		scale = 0
	}
	if scale > 30 {
		scale = 30
	}
	if scale > 0 {
		if p <= 0 {
			p = 38
		}
		if scale > p {
			p = scale
		}
		return fmt.Sprintf("DECIMAL(%d,%d)", p, scale)
	}
	switch {
	case p <= 0:
		return "DECIMAL(38,0)"
	case p <= 2:
		return "TINYINT"
	case p <= 4:
		return "SMALLINT"
	case p <= 9:
		return "INT"
	case p <= 18:
		return "BIGINT"
	default:
		return fmt.Sprintf("DECIMAL(%d,0)", p)
	}
}

// ----------------------------------------------------------------
// DDL 生成（原 schema 包逻辑）
// ----------------------------------------------------------------

// GenerateCreateWithMeta 按源表级属性生成带表尾选项（ENGINE/CHARSET/COLLATE/COMMENT）的建表语句。
func (w *Writer) GenerateCreateWithMeta(db *sql.DB, table string, cols []types.Column, pk []string, meta types.TableMeta) (string, error) {
	return generateCreateWithMeta(db, table, cols, pk, meta)
}

// GenerateCreate 根据源列元数据生成 MySQL 的 CREATE TABLE 语句。
// db 为目标库连接，用于探测服务端版本并按版本自动降级字符集/类型（5.5/5.6 开箱即用）。
func GenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	return generateCreateWithMeta(db, table, cols, pk, types.TableMeta{})
}

// generateCreateWithMeta 是建表的统一实现；meta 为零值时输出与此前完全一致
// （ENGINE=InnoDB DEFAULT CHARSET=<目标默认>），仅在表尾追加源表属性。
func generateCreateWithMeta(db *sql.DB, table string, cols []types.Column, pk []string, meta types.TableMeta) (string, error) {
	if len(cols) == 0 {
		return "", fmt.Errorf("表 %s 没有任何列，无法生成建表语句", table)
	}
	ver := detectMySQLVersion(db)
	defs := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		mt, err := mysqlTypeForVersion(c, ver)
		if err != nil {
			return "", err
		}
		def, err := mysqlColumnDef(c, mt, ver, pk)
		if err != nil {
			return "", err
		}
		defs = append(defs, def)
	}

	if len(pk) > 0 {
		quoted := make([]string, 0, len(pk))
		for _, k := range pk {
			quoted = append(quoted, fmt.Sprintf("`%s`", k))
		}
		defs = append(defs, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(quoted, ", ")))
	}

	body := strings.Join(defs, ",\n")
	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` (\n%s\n) %s", table, body, mysqlTableOptions(meta, ver))
	// 搬迁源表自增计数器：仅当存在 AUTO_INCREMENT 列且源端 TABLES.AUTO_INCREMENT 有值时输出。
	// 源表删过尾部数据时该值可能远大于 max(id)+1，显式指定可避免两边 id 段重叠。
	if start, ok := autoIncrementStartOf(cols); ok {
		ddl += fmt.Sprintf(" AUTO_INCREMENT=%d", start)
	}
	return ddl, nil
}

// mysqlTableOptions 生成表尾选项：ENGINE / DEFAULT CHARSET / COLLATE / COMMENT。
// meta 为零值（非 MySQL 源或读取失败）时保持历史输出：ENGINE=InnoDB + 目标默认字符集，
// 不输出 COLLATE/COMMENT。取值缺失或非法时降级而不是硬塞：
//   - ENGINE 非法标识符 → InnoDB；
//   - 字符集非法 → 目标默认字符集；按目标版本降级（<5.5.3 无 utf8mb4、<8.0.29 无 utf8mb3）；
//   - 排序规则非法或与最终字符集不匹配 → 不输出 COLLATE；
//   - 表注释为空 → 不输出 COMMENT 子句。
func mysqlTableOptions(meta types.TableMeta, ver mysqlVersion) string {
	engine := "InnoDB"
	if validIdentifierName(meta.Engine) {
		engine = meta.Engine
	}
	charset := mysqlCharset(ver)
	if cs := mysqlCharsetForVersion(meta.Charset, ver); cs != "" {
		charset = cs
	}
	opts := fmt.Sprintf("ENGINE=%s DEFAULT CHARSET=%s", engine, charset)
	if coll := mysqlCollationForVersion(meta.Collation, ver); validIdentifierName(coll) && collationMatchesCharset(coll, charset) {
		opts += " COLLATE=" + coll
	}
	if meta.Comment != "" {
		opts += " COMMENT=" + mysqlQuotedString(meta.Comment)
	}
	return opts
}

// mysqlCharsetForVersion 按目标版本降级源表默认字符集：
// 低于 5.5.3 无 utf8mb4，降为 utf8；低于 8.0.29 旧版只认 utf8，故 utf8mb3 降为 utf8。
// 非法字符集名返回空，由调用方回退目标默认字符集。
func mysqlCharsetForVersion(cs string, ver mysqlVersion) string {
	if !validIdentifierName(cs) {
		return ""
	}
	if !ver.atLeast(5, 5, 3) && strings.EqualFold(cs, "utf8mb4") {
		return "utf8"
	}
	if !ver.atLeast(8, 0, 29) && strings.EqualFold(cs, "utf8mb3") {
		return "utf8"
	}
	return cs
}

// collationMatchesCharset 判断排序规则是否属于该字符集：MySQL 排序规则名以「字符集_」为前缀，
// 特例 binary 字符集与其排序规则同名。不匹配时输出 COLLATE 会建表失败，故直接省略。
func collationMatchesCharset(coll, charset string) bool {
	lowC, lowS := strings.ToLower(coll), strings.ToLower(charset)
	if lowC == lowS {
		return true
	}
	return strings.HasPrefix(lowC, lowS+"_")
}

// autoIncrementStartOf 取源表自增计数器（仅 AUTO_INCREMENT 列携带该值；0 表示未知或从未插入数据）。
func autoIncrementStartOf(cols []types.Column) (int64, bool) {
	for _, c := range cols {
		if c.AutoIncrement && c.AutoIncrementStart > 0 {
			return c.AutoIncrementStart, true
		}
	}
	return 0, false
}

// mysqlColumnDef 生成单列的 DDL 片段：类型 + 字符集/排序规则 + 可空性 + 默认值 +
// AUTO_INCREMENT + ON UPDATE + 生成列表达式 + 注释。
// 上述附加属性只在 MySQL 源端提供时（types.Column 已填充）才输出，
// 其他方言的源端保持原有 DDL 形态。
//
// pk 为主键列（按源表主键顺序），用于校验 AUTO_INCREMENT 列必须是主键首列：
// MySQL 要求自增列必须是某个索引的首列，而本工具建表只生成主键约束，不迁移二级/唯一索引。
func mysqlColumnDef(c types.Column, mt string, ver mysqlVersion, pk []string) (string, error) {
	line := fmt.Sprintf("  `%s` %s", c.Name, mt)
	// MySQL 语法要求 CHARACTER SET / COLLATE 紧跟类型名。
	if c.CharSet != "" && validIdentifierName(c.CharSet) &&
		(ver.atLeast(5, 5, 3) || !strings.EqualFold(c.CharSet, "utf8mb4")) {
		line += " CHARACTER SET " + c.CharSet
		coll := mysqlCollationForVersion(c.Collation, ver)
		if validIdentifierName(coll) && (ver.atLeast(5, 5, 3) || !strings.Contains(strings.ToLower(coll), "utf8mb4")) {
			line += " COLLATE " + coll
		}
	}

	// 生成列：data_type [GENERATED ALWAYS] AS (expr) [VIRTUAL|STORED] [NOT NULL] [COMMENT]，
	// 生成列不能同时有 DEFAULT / AUTO_INCREMENT / ON UPDATE。
	if c.GenerationExpression != "" {
		if c.AutoIncrement {
			return "", fmt.Errorf("列 %s 既是生成列又是 AUTO_INCREMENT，MySQL 不支持", c.Name)
		}
		gt := c.GenerationType
		if gt == "" {
			gt = "VIRTUAL"
		}
		line += " GENERATED ALWAYS AS " + generationExprForDDL(c.GenerationExpression) + " " + gt
		if !c.Nullable {
			line += " NOT NULL"
		}
		if c.Comment != "" {
			line += " COMMENT " + mysqlQuotedString(c.Comment)
		}
		return line, nil
	}

	if !c.Nullable {
		line += " NOT NULL"
	}
	if c.HasDefault && !c.AutoIncrement {
		line += " DEFAULT " + mysqlColumnDefault(c, mt)
	}
	if c.AutoIncrement {
		if len(pk) == 0 || pk[0] != c.Name {
			return "", fmt.Errorf("列 %s 为 AUTO_INCREMENT，但不是主键首列（本工具建表只生成主键约束，未迁移二级/唯一索引），无法保真建表", c.Name)
		}
		line += " AUTO_INCREMENT"
	}
	if c.OnUpdateCurrentTimestamp {
		base := baseMySQLType(mt)
		if base == "datetime" || base == "timestamp" {
			line += onUpdateCurrentTimestampClause(mt)
		}
	}
	if c.Comment != "" {
		line += " COMMENT " + mysqlQuotedString(c.Comment)
	}
	return line, nil
}

// generationExprForDDL 给生成列表达式加上 MySQL 必需的括号；
// INFORMATION_SCHEMA.GENERATION_EXPRESSION 通常已带一层外括号，此时原样使用避免出现双括号。
func generationExprForDDL(expr string) string {
	e := strings.TrimSpace(expr)
	if fullyParenthesized(e) {
		return e
	}
	return "(" + e + ")"
}

// fullyParenthesized 判断整个字符串是否被一对最外层括号完整包裹（忽略引号/反引号内的括号）。
func fullyParenthesized(s string) bool {
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return false
	}
	depth := 0
	inQuote, inBacktick := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && (inQuote || inBacktick):
			i++
		case ch == '\'' && !inBacktick:
			inQuote = !inQuote
		case ch == '`' && !inQuote:
			inBacktick = !inBacktick
		case !inQuote && !inBacktick && ch == '(':
			depth++
		case !inQuote && !inBacktick && ch == ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return false
			}
		}
	}
	return depth == 0
}

// onUpdateCurrentTimestampClause 输出 ON UPDATE CURRENT_TIMESTAMP，精度与列定义一致
// （timestamp(6)/datetime(6) 需要 CURRENT_TIMESTAMP(6)，否则 MySQL 报精度不匹配）。
func onUpdateCurrentTimestampClause(mt string) string {
	if fsp := typeFractionalSeconds(mt); fsp != "" {
		return " ON UPDATE CURRENT_TIMESTAMP(" + fsp + ")"
	}
	return " ON UPDATE CURRENT_TIMESTAMP"
}

// typeFractionalSeconds 取类型括号中的整数参数（如 datetime(6) → "6"），无参数返回空。
func typeFractionalSeconds(t string) string {
	i := strings.Index(t, "(")
	if i < 0 {
		return ""
	}
	j := strings.Index(t[i:], ")")
	if j <= 1 {
		return ""
	}
	v := strings.TrimSpace(t[i+1 : i+j])
	if _, err := strconv.Atoi(v); err != nil {
		return ""
	}
	return v
}

// mysqlCollationForVersion 按目标版本降级源端排序规则，否则目标库不认识该 collation 会直接建表失败：
// 低于 8.0 的 *_0900_* 降为 utf8mb4_general_ci/utf8mb4_bin；低于 8.0.29 的 utf8mb3_* 降为 utf8_*；
// 低于 5.5.3 的 utf8mb4_* 降为 utf8_*（该版本无 utf8mb4）。
func mysqlCollationForVersion(coll string, ver mysqlVersion) string {
	if coll == "" {
		return coll
	}
	low := strings.ToLower(coll)
	if !ver.atLeast(8, 0, 0) && strings.Contains(low, "0900") {
		if strings.HasSuffix(low, "_bin") {
			coll = "utf8mb4_bin"
		} else {
			coll = "utf8mb4_general_ci"
		}
		low = strings.ToLower(coll)
	}
	if !ver.atLeast(8, 0, 29) && strings.HasPrefix(low, "utf8mb3_") {
		coll = "utf8" + coll[len("utf8mb3"):]
		low = strings.ToLower(coll)
	}
	if !ver.atLeast(5, 5, 3) && strings.HasPrefix(low, "utf8mb4_") {
		coll = "utf8" + coll[len("utf8mb4"):]
	}
	return coll
}

// mysqlColumnDefault 按列类型渲染默认值：数值/BIT 原样输出，字符串/日期等加单引号，
// MySQL 8.0.13+ 的表达式默认值需包一层括号（CURRENT_TIMESTAMP 除外）。
func mysqlColumnDefault(c types.Column, mt string) string {
	d := strings.TrimSpace(c.Default)
	base := baseMySQLType(mt)
	if c.DefaultIsExpr {
		low := strings.ToLower(d)
		if strings.HasPrefix(low, "current_timestamp") {
			return d
		}
		if strings.HasPrefix(d, "(") && strings.HasSuffix(d, ")") {
			return d
		}
		return "(" + d + ")"
	}
	switch base {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint",
		"decimal", "numeric", "float", "double", "real", "bit", "year":
		return d
	}
	low := strings.ToLower(d)
	if (base == "datetime" || base == "timestamp") &&
		(low == "current_timestamp" || strings.HasPrefix(low, "current_timestamp(")) {
		return d
	}
	return mysqlQuotedString(d)
}

// mysqlQuotedString 把字符串渲染为 MySQL 单引号字面量（转义反斜杠与单引号）。
func mysqlQuotedString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// validIdentifierName 校验字符集/排序规则名（仅字母、数字、下划线）。
func validIdentifierName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// GenerateAlter 比对目标表（已存在）现有列与映射后的源列，生成使其一致的 ALTER TABLE 语句。
// 仅处理「目标缺列（ADD COLUMN）」与「列类型不一致（MODIFY）」两类差异；
// 目标多出而源没有的列不参与（避免误删业务列）。只读目标库、不执行 DDL。
// 返回空字符串表示结构一致。供整库同步前结构预检：发现不一致由调用方打印并退出。
func (w *Writer) GenerateAlter(db *sql.DB, table string, srcCols []types.Column) (string, error) {
	tgtCols, err := readerTableColumns(db, table)
	if err != nil {
		return "", fmt.Errorf("读取目标表 %s 现有列失败: %w", table, err)
	}
	ver := detectMySQLVersion(db)
	tgtMap := make(map[string]types.Column, len(tgtCols))
	for _, c := range tgtCols {
		tgtMap[strings.ToUpper(c.Name)] = c
	}
	qT := w.QuoteIdentifier(table)
	var stmts []string
	for _, sc := range srcCols {
		want, e := mysqlTypeForVersion(sc, ver)
		if e != nil {
			return "", e
		}
		qC := w.QuoteIdentifier(sc.Name)
		tc, ok := tgtMap[strings.ToUpper(sc.Name)]
		if !ok {
			line := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", qT, qC, want)
			if !sc.Nullable {
				line += " NOT NULL"
			}
			stmts = append(stmts, line+";")
			continue
		}
		have, e := mysqlTypeForVersion(tc, ver)
		if e != nil {
			have = "" // 目标列类型无法识别：保守生成 MODIFY 以便人工核对
		}
		if !mysqlTypesEqual(have, want) {
			line := fmt.Sprintf("ALTER TABLE %s MODIFY %s %s", qT, qC, want)
			if !sc.Nullable {
				line += " NOT NULL"
			}
			stmts = append(stmts, line+";")
		}
	}
	return strings.Join(stmts, "\n"), nil
}

// ----------------------------------------------------------------
// Reader 实现（MySQL 作为同步源端）
// ----------------------------------------------------------------

func init() {
	// mysql 包同时提供 Writer（作为目标端）与 Reader（作为源端）。
	dialect.RegisterReader("mysql", func() dialect.Reader { return &Reader{} })
}

// Reader 实现 dialect.Reader 接口，封装 MySQL 源数据库的读取操作。
type Reader struct{}

func (r *Reader) Dialect() string { return "mysql" }

func (r *Reader) Open(dsn string) (*sql.DB, error) { return Open(dsn) }

func (r *Reader) TableColumns(db *sql.DB, tableRef string) ([]types.Column, error) {
	return readerTableColumns(db, tableRef)
}

func (r *Reader) PrimaryKey(db *sql.DB, schema, table string) []string {
	return readerPrimaryKey(db, schema, table)
}

func (r *Reader) RowCount(db *sql.DB, tableRef, where string, args []interface{}) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s", tableRef)
	if where != "" {
		q += " WHERE " + where
	}
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计源表 %s 行数失败: %w", tableRef, err)
	}
	return n, nil
}

func (r *Reader) ScanRow(rows *sql.Rows, n int) ([]interface{}, error) {
	return scanRow(rows, n)
}

func (r *Reader) Placeholder(_ int) string { return "?" }

func (r *Reader) QuoteIdentifier(name string) string { return "`" + name + "`" }

// SamplePKQuery 按主键有序抽样：MySQL 使用 LIMIT/OFFSET，5.7 与 8.0 通用。
// 不使用 ROW_NUMBER() OVER —— 该窗口函数仅 MySQL 8.0+ 支持，5.7 会直接报语法错误。
func (r *Reader) SamplePKQuery(tableRef string, orderCols []string, offset, limit int64) string {
	cols := make([]string, len(orderCols))
	for i, c := range orderCols {
		cols[i] = r.QuoteIdentifier(c)
	}
	sel := strings.Join(cols, ", ")
	return fmt.Sprintf("SELECT %s FROM %s ORDER BY %s LIMIT %d OFFSET %d",
		sel, tableRef, sel, limit, offset)
}

// SupportsRowID MySQL 不支持 ROWID 伪列。
func (r *Reader) SupportsRowID() bool { return false }

// Normalize MySQL 经 go-sql-driver 返回的文本列是 []byte，
// 直接绑定到 Oracle 会被当 RAW/BLOB，故在此转回 string；
// 时间列（time.Time）、数值（int64/float64）等基础类型原样保留。
func (r *Reader) Normalize(v interface{}, dbType string) interface{} { return normalizeMySQL(v) }

func (r *Reader) IsTimeType(dbType string) bool { return isMySQLTimeType(dbType) }

func (r *Reader) IsNumericType(dbType string) bool { return isMySQLNumericType(dbType) }

// ListTables 枚举 MySQL 源库下所有基表（排除视图与系统库），用于通配符 * 整库同步展开。
func (r *Reader) ListTables(db *sql.DB, schema string) ([]string, error) {
	q := `SELECT TABLE_NAME
		FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`
	rows, err := db.Query(q, schema)
	if err != nil {
		return nil, fmt.Errorf("枚举 MySQL 源库 %s 表失败: %w", schema, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

// TableMeta 读取源表级属性（存储引擎/默认字符集/排序规则/表注释），供 MySQL→MySQL 建表还原表尾选项。
// tableRef 支持 "db.table" 或仅表名（未限定库名时取连接默认库，与 TableColumns 一致）。
func (r *Reader) TableMeta(db *sql.DB, tableRef string) (types.TableMeta, error) {
	schema, table := splitTableRef(tableRef)
	if schema == "" {
		var cur string
		if e := db.QueryRow("SELECT DATABASE()").Scan(&cur); e == nil {
			schema = cur
		}
	}
	if schema == "" || table == "" {
		return types.TableMeta{}, fmt.Errorf("无法解析源表 %s 的库名", tableRef)
	}
	return readerTableMeta(db, schema, table)
}

// readerTableMeta 查询 INFORMATION_SCHEMA.TABLES，取 ENGINE / TABLE_COLLATION / TABLE_COMMENT。
// 字符集优先由 COLLATION_CHARACTER_SET_APPLICABILITY 反查（权威）；该映射表在部分
// MySQL 兼容服务（如 go-mysql-server）中不存在，此时退化为按排序规则名前缀推断
// （utf8mb4_general_ci → utf8mb4）。视图等没有 ENGINE/COLLATION 的对象返回零值字段。
func readerTableMeta(db *sql.DB, schema, table string) (types.TableMeta, error) {
	const base = `SELECT t.ENGINE, t.TABLE_COLLATION, t.TABLE_COMMENT
		FROM INFORMATION_SCHEMA.TABLES t
		WHERE t.TABLE_SCHEMA = ? AND t.TABLE_NAME = ?`
	var engine, collation, comment sql.NullString
	if err := db.QueryRow(base, schema, table).Scan(&engine, &collation, &comment); err != nil {
		return types.TableMeta{}, fmt.Errorf("读取源表 %s.%s 表级属性失败: %w", schema, table, err)
	}

	meta := types.TableMeta{
		Engine:    engine.String,
		Collation: collation.String,
		Comment:   comment.String,
	}
	meta.Charset = charsetFromCollation(meta.Collation)

	const withCharset = `SELECT cs.CHARACTER_SET_NAME
		FROM INFORMATION_SCHEMA.COLLATION_CHARACTER_SET_APPLICABILITY cs
		WHERE cs.COLLATION_NAME = ?`
	var cs sql.NullString
	if err := db.QueryRow(withCharset, meta.Collation).Scan(&cs); err == nil && cs.String != "" {
		meta.Charset = cs.String
	}
	return meta, nil
}

// charsetFromCollation 由排序规则名推断字符集：MySQL 排序规则名以「字符集_」为前缀
// （utf8mb4_general_ci → utf8mb4），binary 字符集的排序规则与其同名。
func charsetFromCollation(coll string) string {
	low := strings.ToLower(coll)
	if i := strings.Index(low, "_"); i > 0 {
		return low[:i]
	}
	return low
}

// readerTableColumns 读取 MySQL 表列元数据。
//
// 基础元数据（列名/基础类型/精度/可空性）沿用 "SELECT * FROM <ref> WHERE 1=0"，
// 再用 INFORMATION_SCHEMA.COLUMNS 补充 go-sql-driver 拿不到的列定义：
//
//   - COLUMN_TYPE：varchar(32)、int unsigned、decimal(14,2)、enum('a','b') 等完整类型定义
//   - CHARACTER_SET_NAME / COLLATION_NAME：字符集与排序规则
//   - COLUMN_DEFAULT / EXTRA：默认值（含 8.0.13+ 的表达式默认值）
//   - COLUMN_COMMENT：列注释
//
// 根因说明：go-sql-driver 未实现 driver.RowsColumnTypeLength，ColumnType.Length() 恒返回
// ok=false，旧路径下 varchar(n)/char(n) 的 Length 永远为 0，MySQL 目标端据此把它们
// 统一降级成 TEXT/CHAR(1)；TEXT 列再作主键即触发 Error 1170。
// MySQL→MySQL 建表改为直接沿用 COLUMN_TYPE 原文（types.Column.RawType），不再按基础类型重映射。
//
// Length/Precision/Scale 保持旧路径的取值（Length 仍为 0，Precision/Scale 来自驱动），
// 以免改变 MySQL→Oracle/SQL Server/PostgreSQL 等其它目标方向的既有建表结果。
// INFORMATION_SCHEMA 不可读（权限不足等）时只返回基础元数据，行为回退到旧版。
func readerTableColumns(db *sql.DB, tableRef string) ([]types.Column, error) {
	cols, err := readerTableColumnsFromResult(db, tableRef)
	if err != nil {
		return nil, err
	}
	schema, table := splitTableRef(tableRef)
	if schema == "" {
		// 与 readerPrimaryKey 一致：未限定库名时取连接默认库（DSN 中的 database）。
		var cur string
		if e := db.QueryRow("SELECT DATABASE()").Scan(&cur); e == nil {
			schema = cur
		}
	}
	if schema != "" && table != "" {
		enrichColumnsFromInfoSchema(db, schema, table, cols)
	}
	return cols, nil
}

// readerTableColumnsFromResult 是列元数据的基础读取路径（驱动层 ColumnTypes）。
func readerTableColumnsFromResult(db *sql.DB, tableRef string) ([]types.Column, error) {
	rows, err := db.Query(fmt.Sprintf("SELECT * FROM %s WHERE 1=0", tableRef))
	if err != nil {
		return nil, fmt.Errorf("读取表 %s 列元数据失败: %w", tableRef, err)
	}
	defer rows.Close()

	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("获取表 %s 列类型失败: %w", tableRef, err)
	}
	cols := make([]types.Column, 0, len(cts))
	for _, ct := range cts {
		c := types.Column{Name: ct.Name(), DBType: ct.DatabaseTypeName()}
		if length, ok := ct.Length(); ok {
			c.Length = length
		}
		if prec, scale, ok := ct.DecimalSize(); ok {
			c.Precision = int(prec)
			c.Scale = int(scale)
		}
		if nullable, ok := ct.Nullable(); ok {
			c.Nullable = nullable
		}
		cols = append(cols, c)
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("表 %s 未返回任何列，请确认表名是否正确", tableRef)
	}
	return cols, nil
}

// enrichColumnsFromInfoSchema 按列名把 INFORMATION_SCHEMA.COLUMNS 中的完整列定义回填到 cols：
// COLUMN_TYPE（类型原文）、字符集/排序规则、默认值、注释、自增、ON UPDATE、生成列表达式。
// 该函数是尽力而为：查询失败或行列不匹配时直接返回，保留基础元数据。
//
// GENERATION_EXPRESSION 是 MySQL 5.7 才有的列，5.6 及更早版本查询会报未知列，
// 故先带该列查询，失败后退回不带生成列的查询（单次额外往返，仅旧版本发生）。
func enrichColumnsFromInfoSchema(db *sql.DB, schema, table string, cols []types.Column) {
	// 同一查询里 LEFT JOIN TABLES 取表级自增计数器（TABLES.AUTO_INCREMENT），
	// 避免为起始值再单独往返一次；表从未插入过数据时该值为 NULL。
	const baseCols = `SELECT c.COLUMN_NAME, c.COLUMN_TYPE, c.CHARACTER_SET_NAME, c.COLLATION_NAME,
		c.COLUMN_DEFAULT, c.COLUMN_COMMENT, c.EXTRA`
	const where = `, t.AUTO_INCREMENT
		FROM INFORMATION_SCHEMA.COLUMNS c
		LEFT JOIN INFORMATION_SCHEMA.TABLES t
			ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
		WHERE c.TABLE_SCHEMA = ? AND c.TABLE_NAME = ?
		ORDER BY c.ORDINAL_POSITION`
	queries := []string{
		baseCols + ", c.GENERATION_EXPRESSION" + where,
		baseCols + where,
	}

	idx := make(map[string]int, len(cols))
	for i, c := range cols {
		idx[c.Name] = i
	}

	for _, q := range queries {
		hasGen := strings.Contains(q, "GENERATION_EXPRESSION")
		rows, err := db.Query(q, schema, table)
		if err != nil {
			continue // 旧版本无 GENERATION_EXPRESSION，尝试下一条查询
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var (
					name, columnType string
					charSet, coll    sql.NullString
					def              sql.NullString
					comment, extra   sql.NullString
					genExpr          sql.NullString
					autoIncStart     sql.NullInt64
				)
				dest := []interface{}{&name, &columnType, &charSet, &coll, &def, &comment, &extra}
				if hasGen {
					dest = append(dest, &genExpr)
				}
				dest = append(dest, &autoIncStart)
				if err := rows.Scan(dest...); err != nil {
					return
				}
				i, ok := idx[name]
				if !ok {
					continue
				}
				c := &cols[i]
				if validRawType(columnType) {
					c.RawType = columnType
				}
				c.CharSet = charSet.String
				c.Collation = coll.String
				c.Comment = comment.String

				extraUp := strings.ToUpper(extra.String)
				c.AutoIncrement = strings.Contains(extraUp, "AUTO_INCREMENT")
				if c.AutoIncrement && autoIncStart.Valid {
					c.AutoIncrementStart = autoIncStart.Int64
				}
				c.OnUpdateCurrentTimestamp = strings.Contains(extraUp, "ON UPDATE CURRENT_TIMESTAMP")
				switch {
				case strings.Contains(extraUp, "STORED GENERATED"):
					c.GenerationType = "STORED"
				case strings.Contains(extraUp, "VIRTUAL GENERATED"):
					c.GenerationType = "VIRTUAL"
				}
				if genExpr.Valid && strings.TrimSpace(genExpr.String) != "" &&
					validGenerationExpression(genExpr.String) {
					c.GenerationExpression = genExpr.String
					if c.GenerationType == "" {
						c.GenerationType = "VIRTUAL" // EXTRA 未给类型时按虚拟列兜底
					}
				}

				if def.Valid {
					c.HasDefault = true
					c.Default = def.String
					c.DefaultIsExpr = strings.Contains(extraUp, "DEFAULT_GENERATED")
				}
			}
		}()
		return
	}
}

// validGenerationExpression 校验生成列表达式：分号/换行只允许出现在引号或反引号内，
// 避免异常元数据把 DDL 拆成多条语句。表达式本身由 MySQL 服务器生成，反引号是合法语法。
func validGenerationExpression(s string) bool {
	inQuote, inBacktick := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && (inQuote || inBacktick):
			i++ // 跳过被转义的下一个字符
		case ch == '\'' && !inBacktick:
			inQuote = !inQuote
		case ch == '`' && !inQuote:
			inBacktick = !inBacktick
		case !inQuote && !inBacktick && (ch == ';' || ch == '\n' || ch == '\r'):
			return false
		}
	}
	return !inQuote && !inBacktick
}

// splitTableRef 把 "db.table" / "`db`.`table`" 拆为库名与表名，未限定库时 schema 为空。
func splitTableRef(ref string) (schema, table string) {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "."); i >= 0 {
		return unquoteMySQLIdentifier(ref[:i]), unquoteMySQLIdentifier(ref[i+1:])
	}
	return "", unquoteMySQLIdentifier(ref)
}

// unquoteMySQLIdentifier 去掉标识符两端的反引号或双引号（支持双写反引号转义）。
func unquoteMySQLIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '`' || s[0] == '"') && s[len(s)-1] == s[0] {
		q := string(s[0])
		return strings.ReplaceAll(s[1:len(s)-1], q+q, q)
	}
	return s
}

// readerPrimaryKey 查询 MySQL 源表主键（按 ORDINAL_POSITION 排序）。
// schema 即数据库名；若为空则从 table 的 "db.table" 形式中解析，
// 若仍为空则回退到连接当前默认库（DSN 中的 database），与 readerTableColumns 行为一致。
// 表名大小写不敏感（INFORMATION_SCHEMA 存储可能为小写）。
func readerPrimaryKey(db *sql.DB, schema, table string) []string {
	if schema == "" {
		if i := strings.Index(table, "."); i >= 0 {
			schema = table[:i]
			table = table[i+1:]
		}
	}
	if schema == "" {
		// 未提供 schema：使用连接当前默认库（对应 DSN 中的 database）。
		var cur string
		if err := db.QueryRow("SELECT DATABASE()").Scan(&cur); err == nil {
			schema = cur
		}
	}
	if schema == "" {
		return nil
	}
	q := `SELECT COLUMN_NAME
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = ? AND UPPER(TABLE_NAME) = UPPER(?) AND COLUMN_KEY = 'PRI'
		ORDER BY ORDINAL_POSITION`
	rows, err := db.Query(q, schema, table)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil
		}
		pk = append(pk, col)
	}
	return pk
}

// scanRow 将当前行扫描为 []interface{}，保留驱动原生值。
func scanRow(rows *sql.Rows, n int) ([]interface{}, error) {
	vals := make([]interface{}, n)
	ptrs := make([]interface{}, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	return vals, nil
}

func normalizeMySQL(v interface{}) interface{} {
	switch x := v.(type) {
	case nil, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64, string, bool, time.Time:
		return v
	case []byte:
		return string(x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func isMySQLTimeType(dbType string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "DATE", "DATETIME", "TIMESTAMP", "TIME":
		return true
	}
	return false
}

func isMySQLNumericType(dbType string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "INT", "INTEGER", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT",
		"DECIMAL", "NUMERIC", "FLOAT", "DOUBLE", "DOUBLE PRECISION", "REAL", "BIT":
		return true
	}
	return false
}
