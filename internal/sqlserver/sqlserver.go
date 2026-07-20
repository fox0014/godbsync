package sqlserver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"godbsync/internal/dialect"
	"godbsync/internal/types"
)

// SQL Server 占位符上限（SQL Server 单条语句最多 2100 个参数，留余量）。
const maxPlaceholders = 2000

// pkCache 缓存各（连接, 目标表）的主键，避免每个批次重复查询 INFORMATION_SCHEMA。
var pkCache sync.Map // key: fmt.Sprintf("%p|%s", db, target) -> []string

// ================================================================
// Reader 实现（SQL Server 作为同步源端）
// ================================================================

func init() {
	dialect.RegisterReader("sqlserver", func() dialect.Reader { return &Reader{} })
}

// Reader 实现 dialect.Reader 接口，封装 SQL Server 源数据库的读取操作。
type Reader struct{}

func (r *Reader) Dialect() string { return "sqlserver" }

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

func (r *Reader) QuoteIdentifier(name string) string { return "[" + name + "]" }

// SupportsRowID SQL Server 不支持 ROWID 伪列。
func (r *Reader) SupportsRowID() bool { return false }

// Normalize go-mssqldb 返回的文本列可能是 []byte，绑定到 MySQL 时需转回 string；
// uniqueidentifier 返回 [16]byte，须转成规范 GUID 字符串（否则写入 VARCHAR(36) 会损坏）。
func (r *Reader) Normalize(v interface{}, dbType string) interface{} {
	return normalizeSQLServer(v, dbType)
}

func (r *Reader) IsTimeType(dbType string) bool { return isSQLServerTimeType(dbType) }

func (r *Reader) IsNumericType(dbType string) bool { return isSQLServerNumericType(dbType) }

// Open 打开一个 SQL Server 连接并配置连接池参数。
// go-mssqldb 是纯 Go 实现，无需安装 SQL Server 客户端。
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQL Server 连接失败: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接 SQL Server 失败(Ping): %w", err)
	}
	return db, nil
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

// readerPrimaryKey 查询 SQL Server 源表主键（按 ORDINAL_POSITION 排序）。
// schema 为空时默认使用 dbo。
func readerPrimaryKey(db *sql.DB, schema, table string) []string {
	if schema == "" {
		if i := strings.LastIndex(table, "."); i >= 0 {
			schema = table[:i]
			table = table[i+1:]
		} else {
			schema = "dbo"
		}
	}
	q := `SELECT cc.COLUMN_NAME
		FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS c
		JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE cc
		  ON c.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		  AND c.TABLE_SCHEMA = cc.TABLE_SCHEMA
		WHERE c.CONSTRAINT_TYPE = 'PRIMARY KEY'
		  AND c.TABLE_SCHEMA = ? AND UPPER(c.TABLE_NAME) = UPPER(?)
		ORDER BY cc.ORDINAL_POSITION`
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

// normalizeSQLServer 将 go-mssqldb 返回的值规整为可跨库绑定的基础类型。
// 文本列返回 []byte → string；uniqueidentifier 返回 [16]byte → 规范 GUID 字符串；
// 其余原生类型原样保留。
// 注意：不能无脑把全部 []byte 转 string/hex，否则 VARBINARY 等二进制列会被破坏，
// 故仅当 dbType 为 UNIQUEIDENTIFIER 时才做 GUID 转换。
func normalizeSQLServer(v interface{}, dbType string) interface{} {
	switch x := v.(type) {
	case nil, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64, string, bool, time.Time:
		return v
	case []byte:
		if strings.EqualFold(dbType, "UNIQUEIDENTIFIER") && len(x) == 16 {
			return guidToString(x)
		}
		return string(x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// guidToString 将 go-mssqldb 返回的 16 字节 uniqueidentifier 转为规范 GUID 字符串
// （xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx），与 SQL Server 显示及 MySQL VARCHAR(36) 兼容。
// 字节布局遵循 RFC 4122 / SQL Server：前 3 组小端、Data4 大端。
func guidToString(b []byte) string {
	const hexd = "0123456789ABCDEF"
	buf := make([]byte, 36)
	pos := 0
	put := func(n int) {
		buf[pos] = hexd[b[n]>>4]
		buf[pos+1] = hexd[b[n]&0x0F]
		pos += 2
	}
	// 8-4-4-4-12
	put(0)
	put(1)
	put(2)
	put(3)
	buf[pos] = '-'
	pos++
	put(4)
	put(5)
	buf[pos] = '-'
	pos++
	put(6)
	put(7)
	buf[pos] = '-'
	pos++
	put(8)
	put(9)
	buf[pos] = '-'
	pos++
	put(10)
	put(11)
	put(12)
	put(13)
	put(14)
	put(15)
	return string(buf)
}

func isSQLServerTimeType(dbType string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "DATE", "DATETIME", "DATETIME2", "SMALLDATETIME", "DATETIMEOFFSET", "TIME":
		return true
	}
	return false
}

func isSQLServerNumericType(dbType string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "INT", "BIGINT", "SMALLINT", "TINYINT", "DECIMAL", "NUMERIC",
		"MONEY", "SMALLMONEY", "FLOAT", "REAL", "BIT":
		return true
	}
	return false
}

// ================================================================
// Writer 实现（SQL Server 作为同步目标端）
// ================================================================

func init() {
	dialect.RegisterWriter("sqlserver", func() dialect.Writer { return &Writer{} })
}

// Writer 实现 dialect.Writer 接口，封装 SQL Server 目标数据库的写入操作。
type Writer struct{}

func (w *Writer) Dialect() string { return "sqlserver" }

func (w *Writer) Open(dsn string) (*sql.DB, error) { return Open(dsn) }

func (w *Writer) BulkUpsert(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	return sqlserverBulkUpsert(ctx, db, target, cols, rows, upsert)
}

func (w *Writer) Truncate(db *sql.DB, table string) error { return sqlserverTruncate(db, table) }

func (w *Writer) TableExists(db *sql.DB, database, table string) (bool, error) {
	return sqlserverTableExists(db, database, table)
}

func (w *Writer) ExecDDL(db *sql.DB, ddl string) error { return sqlserverExecDDL(db, ddl) }

func (w *Writer) GenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	return sqlserverGenerateCreate(db, table, cols, pk)
}

func (w *Writer) RowCount(db *sql.DB, table string) (int64, error) {
	return sqlserverRowCount(db, table)
}

func (w *Writer) PrimaryKey(db *sql.DB, database, table string) []string {
	return readerPrimaryKey(db, database, table)
}

func (w *Writer) QuoteIdentifier(name string) string { return "[" + name + "]" }

func (w *Writer) Placeholder(_ int) string { return "?" }

// sqlserverTruncate 清空目标表（支持 schema.table 形式）。
func sqlserverTruncate(db *sql.DB, table string) error {
	q := "TRUNCATE TABLE " + quoteID(table)
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("清空表 %s 失败: %w", table, err)
	}
	return nil
}

// sqlserverTableExists 判断目标表是否存在。
func sqlserverTableExists(db *sql.DB, database, table string) (bool, error) {
	schema, name := splitSchemaTable(table)
	if schema == "" {
		schema = "dbo"
	}
	q := `SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`
	var n int
	if err := db.QueryRow(q, schema, name).Scan(&n); err != nil {
		return false, fmt.Errorf("查询表 %s 是否存在失败: %w", table, err)
	}
	return n > 0, nil
}

// sqlserverExecDDL 在目标库执行一条 DDL。
func sqlserverExecDDL(db *sql.DB, ddl string) error {
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("执行 DDL 失败: %w\nDDL: %s", err, ddl)
	}
	return nil
}

// sqlserverRowCount 返回目标表当前行数。
func sqlserverRowCount(db *sql.DB, table string) (int64, error) {
	var n int64
	q := "SELECT COUNT(*) FROM " + quoteID(table)
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计目标表 %s 行数失败: %w", table, err)
	}
	return n, nil
}

// ----------------------------------------------------------------
// 类型映射（源类型 → SQL Server 列类型）
// ----------------------------------------------------------------

// sqlserverType 将源数据库列类型（典型为 MySQL 类型名）映射为 SQL Server 列类型定义。
func sqlserverType(c types.Column) (string, error) {
	t := strings.ToUpper(strings.TrimSpace(c.DBType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "VARCHAR", "NVARCHAR", "VARCHAR2", "NVARCHAR2", "BPCHAR":
		n := c.Length
		if n <= 0 {
			return "NVARCHAR(4000)", nil
		}
		if n > 4000 {
			return "NVARCHAR(MAX)", nil
		}
		return fmt.Sprintf("NVARCHAR(%d)", n), nil
	case "CHAR", "NCHAR":
		n := c.Length
		if n <= 0 {
			n = 1
		}
		if n > 4000 {
			return "NVARCHAR(MAX)", nil
		}
		return fmt.Sprintf("NCHAR(%d)", n), nil
	case "INT", "INTEGER", "MEDIUMINT", "INT4", "SERIAL":
		return "INT", nil
	case "BIGINT", "INT8", "BIGSERIAL":
		return "BIGINT", nil
	case "SMALLINT", "INT2", "SMALLSERIAL":
		return "SMALLINT", nil
	case "TINYINT":
		// TINYINT(1) 常用于布尔语义
		if c.Length == 1 {
			return "BIT", nil
		}
		// MySQL TINYINT 为有符号(-128..127)，TINYINT UNSIGNED 为 0..255；
		// go-sql-driver 的 DatabaseTypeName 统一返回 "TINYINT" 无法区分带符号，
		// 故非布尔 TINYINT 一律映射到 SMALLINT，避免无符号值溢出。
		return "SMALLINT", nil
	case "DECIMAL", "NUMERIC", "NUMBER":
		return sqlserverNumberType(c.Precision, c.Scale), nil
	case "MONEY":
		return "MONEY", nil
	case "SMALLMONEY":
		return "SMALLMONEY", nil
	case "FLOAT", "BINARY_FLOAT", "FLOAT4":
		return "REAL", nil
	case "BINARY_DOUBLE", "DOUBLE", "DOUBLE PRECISION", "REAL", "FLOAT8":
		return "FLOAT(53)", nil
	case "DATE", "DATETIME", "DATETIME2", "SMALLDATETIME", "DATETIMEOFFSET",
		"TIMESTAMP", "TIME",
		"OCIDATE", "TIMESTAMPDTY", "TIMESTAMPTZ_DTY", "TIMESTAMPELTZ",
		"TIMESTAMPLTZ_DTY", "TIMESTAMPTZ", "TIMETZ",
		"TIMESTAMP_WITH_TZ", "TIMESTAMP_WITH_LOCAL_TZ":
		return "DATETIME2", nil
	case "YEAR":
		return "SMALLINT", nil
	case "TEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT",
		"CLOB", "NCLOB", "LONG", "NTEXT", "XML", "CITEXT", "NAME":
		return "NVARCHAR(MAX)", nil
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB",
		"RAW", "LONG RAW", "BINARY", "VARBINARY", "IMAGE", "BYTEA":
		return "VARBINARY(MAX)", nil
	case "BOOLEAN", "BOOL", "BIT":
		return "BIT", nil
	case "JSON", "JSONB":
		return "NVARCHAR(MAX)", nil
	case "ENUM", "SET":
		return "NVARCHAR(255)", nil
	case "UNIQUEIDENTIFIER", "UUID":
		return "UNIQUEIDENTIFIER", nil
	default:
		return "", fmt.Errorf("未知源类型: %s（列 %s）", c.DBType, c.Name)
	}
}

func sqlserverNumberType(precision, scale int) string {
	if scale > 0 {
		p := precision
		if p <= 0 {
			p = 38
		}
		if p > 38 {
			p = 38
		}
		s := scale
		if s > p {
			s = p
		}
		return fmt.Sprintf("DECIMAL(%d,%d)", p, s)
	}
	switch {
	case precision <= 0:
		return "DECIMAL(38,0)"
	case precision <= 2:
		return "TINYINT"
	case precision <= 4:
		return "SMALLINT"
	case precision <= 9:
		return "INT"
	case precision <= 18:
		return "BIGINT"
	default:
		return fmt.Sprintf("DECIMAL(%d,0)", precision)
	}
}

// ----------------------------------------------------------------
// DDL 生成
// ----------------------------------------------------------------

// sqlserverGenerateCreate 根据源列元数据生成 SQL Server 的 CREATE TABLE 语句。
// 注意：SQL Server 原生不支持 CREATE TABLE IF NOT EXISTS 语法（与 MySQL/Oracle 不同），
// 故此处只生成普通 CREATE TABLE；重复建表的防护由调用方 ensureTable 先查 TableExists 负责，
// 与 Oracle 方言保持一致。
func sqlserverGenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	if len(cols) == 0 {
		return "", fmt.Errorf("表 %s 没有任何列，无法生成建表语句", table)
	}
	defs := make([]string, 0, len(cols)+1)
	pkSet := make(map[string]bool, len(pk))
	for _, k := range pk {
		pkSet[strings.ToUpper(k)] = true
	}

	for _, c := range cols {
		mt, err := sqlserverType(c)
		if err != nil {
			return "", err
		}
		line := fmt.Sprintf("  [%s] %s", c.Name, mt)
		if !c.Nullable {
			line += " NOT NULL"
		}
		defs = append(defs, line)
	}

	if len(pk) > 0 {
		quoted := make([]string, 0, len(pk))
		for _, k := range pk {
			quoted = append(quoted, fmt.Sprintf("[%s]", k))
		}
		defs = append(defs, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(quoted, ", ")))
	}

	body := strings.Join(defs, ",\n")
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n)", quoteID(table), body), nil
}

// ----------------------------------------------------------------
// 批量写入（MERGE 实现幂等 upsert）
// ----------------------------------------------------------------

// sqlserverBulkUpsert 批量写入 SQL Server。
// upsert=false 时强制普通 INSERT（全量首跑，所有行均为新行，且避免 MERGE 的匹配开销）；
// upsert=true 且有主键时按主键构建 MERGE（命中更新非主键列、未命中插入）；
// upsert=true 且无主键时退化为普通 INSERT（非幂等，需配合 truncate 使用）。
// ctx 为 nil 时不限制超时。
func sqlserverBulkUpsert(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	n := len(rows)
	if n == 0 {
		return 0, nil
	}
	perRow := len(cols)
	if perRow == 0 {
		return 0, fmt.Errorf("批量写入 %s 失败: 列数为空", target)
	}
	// SQL Server 表值构造器 VALUES（含 MERGE ... USING (VALUES ...)）单条语句最多 1000 行，
	// 超出报错误 10738。分块须同时受「占位符上限」与「1000 行」双重约束，
	// 否则单列表（perRow=1）会按 2000 行分块而直接报错。
	const maxRowsPerStmt = 1000
	maxRows := maxPlaceholders / perRow
	if maxRows > maxRowsPerStmt {
		maxRows = maxRowsPerStmt
	}
	if maxRows < 1 {
		maxRows = 1
	}

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

	pk := sqlserverPrimaryKey(db, target)
	useMerge := upsert && len(pk) > 0
	var total int64
	for start := 0; start < n; start += maxRows {
		end := start + maxRows
		if end > n {
			end = n
		}
		chunk := rows[start:end]

		var sqlStr string
		if useMerge {
			sqlStr = buildMergeSQL(target, cols, pk, len(chunk))
		} else {
			sqlStr = buildInsertSQL(target, cols, len(chunk))
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

// rowValuePlaceholders 生成含 n 个占位符的行模板，如 n=3 -> "(?,?,?)"。
func rowValuePlaceholders(n int) string {
	if n <= 0 {
		return "()"
	}
	return "(" + strings.Repeat("?,", n-1) + "?)"
}

// buildMergeSQL 生成 SQL Server MERGE 语句：
// 把 chunk 行展开为 USING (VALUES ...) 子查询，以主键列做 ON 匹配，
// 命中更新非主键列、未命中插入整行。
func buildMergeSQL(target string, cols []string, pk []string, rowCount int) string {
	pkSet := make(map[string]bool, len(pk))
	for _, k := range pk {
		pkSet[strings.ToUpper(k)] = true
	}

	// VALUES 子句：每行 (?,?,?,...)
	rowPlaceholder := rowValuePlaceholders(len(cols))
	var valuesRows []string
	for i := 0; i < rowCount; i++ {
		valuesRows = append(valuesRows, rowPlaceholder)
	}
	valuesClause := strings.Join(valuesRows, ", ")

	// USING 列别名
	aliases := make([]string, len(cols))
	for i, c := range cols {
		aliases[i] = fmt.Sprintf("[%s]", c)
	}
	aliasClause := strings.Join(aliases, ", ")

	// ON 子句
	var onParts []string
	for _, c := range cols {
		if pkSet[strings.ToUpper(c)] {
			onParts = append(onParts, fmt.Sprintf("t.[%s] = s.[%s]", c, c))
		}
	}
	onClause := strings.Join(onParts, " AND ")

	// WHEN MATCHED THEN UPDATE
	var updates []string
	for _, c := range cols {
		if pkSet[strings.ToUpper(c)] {
			continue
		}
		updates = append(updates, fmt.Sprintf("t.[%s] = s.[%s]", c, c))
	}
	updateClause := ""
	if len(updates) > 0 {
		updateClause = " WHEN MATCHED THEN UPDATE SET " + strings.Join(updates, ", ")
	}

	// WHEN NOT MATCHED THEN INSERT
	insertCols := make([]string, len(cols))
	insertVals := make([]string, len(cols))
	for i, c := range cols {
		insertCols[i] = fmt.Sprintf("[%s]", c)
		insertVals[i] = fmt.Sprintf("s.[%s]", c)
	}
	insertClause := fmt.Sprintf(" WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s)",
		strings.Join(insertCols, ", "), strings.Join(insertVals, ", "))

	return fmt.Sprintf("MERGE INTO %s AS t USING (VALUES %s) AS s (%s) ON (%s)%s%s;",
		quoteID(target), valuesClause, aliasClause, onClause, updateClause, insertClause)
}

// buildInsertSQL 生成无主键时的普通多行 INSERT（非幂等）。
func buildInsertSQL(target string, cols []string, rowCount int) string {
	rowPlaceholder := rowValuePlaceholders(len(cols))
	var valuesRows []string
	for i := 0; i < rowCount; i++ {
		valuesRows = append(valuesRows, rowPlaceholder)
	}
	insertCols := make([]string, len(cols))
	for i, c := range cols {
		insertCols[i] = fmt.Sprintf("[%s]", c)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES %s;",
		quoteID(target), strings.Join(insertCols, ", "), strings.Join(valuesRows, ", "))
}

// sqlserverPrimaryKey 查询目标表主键列，结果按（连接, 表）缓存以避免每批次重复查询。
func sqlserverPrimaryKey(db *sql.DB, target string) []string {
	key := fmt.Sprintf("%p|%s", db, target)
	if v, ok := pkCache.Load(key); ok {
		return v.([]string)
	}
	schema, name := splitSchemaTable(target)
	if schema == "" {
		schema = "dbo"
	}
	pk := readerPrimaryKey(db, schema, name)
	pkCache.Store(key, pk)
	return pk
}

// splitSchemaTable 将 "schema.table" 拆分为 (schema, table)；无点时返回 ("dbo", table)。
func splitSchemaTable(table string) (schema, name string) {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[:i], table[i+1:]
	}
	return "dbo", table
}

// quoteID 用 SQL Server 方括号包裹标识符，支持 schema.table 形式。
func quoteID(table string) string {
	parts := strings.SplitN(table, ".", 2)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = "[" + p + "]"
	}
	return strings.Join(quoted, ".")
}
