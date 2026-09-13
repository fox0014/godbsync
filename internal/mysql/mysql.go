package mysql

import (
	"context"
	"database/sql"
	"fmt"
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
	t := strings.ToUpper(c.DBType)
	base := t
	if idx := strings.Index(t, "("); idx >= 0 {
		base = t[:idx]
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

// GenerateCreate 根据源列元数据生成 MySQL 的 CREATE TABLE 语句。
// db 为目标库连接，用于探测服务端版本并按版本自动降级字符集/类型（5.5/5.6 开箱即用）。
func GenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	if len(cols) == 0 {
		return "", fmt.Errorf("表 %s 没有任何列，无法生成建表语句", table)
	}
	ver := detectMySQLVersion(db)
	charset := mysqlCharset(ver)
	defs := make([]string, 0, len(cols)+1)
	pkSet := make(map[string]bool, len(pk))
	for _, k := range pk {
		pkSet[strings.ToUpper(k)] = true
	}

	for _, c := range cols {
		mt, err := mysqlTypeForVersion(c, ver)
		if err != nil {
			return "", err
		}
		line := fmt.Sprintf("  `%s` %s", c.Name, mt)
		if !c.Nullable {
			line += " NOT NULL"
		}
		defs = append(defs, line)
	}

	if len(pk) > 0 {
		quoted := make([]string, 0, len(pk))
		for _, k := range pk {
			quoted = append(quoted, fmt.Sprintf("`%s`", k))
		}
		defs = append(defs, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(quoted, ", ")))
	}

	body := strings.Join(defs, ",\n")
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=%s", table, body, charset), nil
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
		if have != want {
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

// readerTableColumns 通过 "SELECT * FROM <ref> WHERE 1=0" 获取列元数据。
func readerTableColumns(db *sql.DB, tableRef string) ([]types.Column, error) {
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
