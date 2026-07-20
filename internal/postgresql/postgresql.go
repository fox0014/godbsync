// Package postgresql 实现 dialect.Reader（源端）与 dialect.Writer（目标端）接口，
// 使 PostgreSQL 既可作为同步源端也可作为目标端：
//   - 源端：PostgreSQL → MySQL / PostgreSQL → Oracle / PostgreSQL → SQL Server
//   - 目标端：MySQL → PostgreSQL / Oracle → PostgreSQL / SQL Server → PostgreSQL
//
// 幂等写入用 INSERT ... ON CONFLICT (pk) DO UPDATE（无主键时退化为普通 INSERT）；
// DDL 用 CREATE TABLE IF NOT EXISTS（PostgreSQL 9.1+ 支持）。
// 驱动使用 github.com/lib/pq（纯 Go，无 CGO，满足项目约束）。
package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"

	"godbsync/internal/dialect"
	"godbsync/internal/types"
)

func init() {
	// PostgreSQL 包同时提供 Reader（作为源端）与 Writer（作为目标端）。
	dialect.RegisterReader("postgresql", func() dialect.Reader { return &Reader{} })
	dialect.RegisterWriter("postgresql", func() dialect.Writer { return &Writer{} })
}

// Reader 实现 dialect.Reader 接口，封装 PostgreSQL 源数据库的读取操作。
type Reader struct{}

func (r *Reader) Dialect() string { return "postgresql" }

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

// Placeholder PostgreSQL 使用 $1/$2 位置占位符（与 Oracle 的 :n、MySQL 的 ? 不同，
// 但都被 dialect.Reader.Placeholder 接口屏蔽，syncer 不感知差异）。
func (r *Reader) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }

// QuoteIdentifier PostgreSQL 使用双引号包裹标识符（与 Oracle 一致）。
func (r *Reader) QuoteIdentifier(name string) string { return `"` + name + `"` }

// SupportsRowID PostgreSQL 无 ROWID 伪列（ctid 不稳定且需改写 SELECT，暂不启用）；
// 无单列主键的源表退化为无序全量，与 MySQL/SQL Server 源行为一致。
func (r *Reader) SupportsRowID() bool { return false }

// Normalize 将 lib/pq 原生值规整为可跨库绑定的基础类型：
// - []byte（text/varchar/uuid/json 等）转 string，否则绑给 Oracle 会被当 RAW；
// - numeric 可能以 *big.Rat 形式返回，转字符串保精度；
// - uuid 若以 16 字节返回，格式化为规范 GUID 字符串；
// - time.Time / 数值 / 字符串等基础类型原样保留。
func (r *Reader) Normalize(v interface{}, dbType string) interface{} {
	return normalizePostgreSQL(v, dbType)
}

func (r *Reader) IsTimeType(dbType string) bool { return isPostgreSQLTimeType(dbType) }

func (r *Reader) IsNumericType(dbType string) bool { return isPostgreSQLNumericType(dbType) }

// Open 打开一个 PostgreSQL 连接并配置连接池参数。
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 PostgreSQL 连接失败: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败(Ping): %w", err)
	}
	return db, nil
}

// readerTableColumns 通过 "SELECT * FROM <ref> WHERE 1=0" 获取列元数据。
// PostgreSQL 的 ColumnTypes().DatabaseTypeName() 返回内部类型名（如 INT4/NUMERIC/TIMESTAMPTZ/UUID），
// 这些名字正是目标端类型映射函数（mysqlType/oracleType/sqlserverType）需要识别的来源。
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

// readerPrimaryKey 查询 PostgreSQL 源表主键（按 ordinal_position 排序）。
// schema 即 PG 的 schema（缺省 public）；表名大小写不敏感匹配（PG 未加引号标识符会折叠为小写，
// 而 syncer 传入的是 UPPER(name)），故用 LOWER 比较。
func readerPrimaryKey(db *sql.DB, schema, table string) []string {
	if schema == "" {
		schema = "public"
	}
	q := `SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND LOWER(tc.table_schema) = LOWER($1)
		  AND LOWER(tc.table_name) = LOWER($2)
		ORDER BY kcu.ordinal_position`
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

func normalizePostgreSQL(v interface{}, dbType string) interface{} {
	switch x := v.(type) {
	case nil, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64, string, bool, time.Time:
		return v
	case []byte:
		// uuid 在个别驱动/配置下以 16 字节返回，需格式化为规范 GUID 字符串写入 VARCHAR(36)/CHAR(36)，
		// 否则直接转 string 会得到乱码字节。
		if strings.EqualFold(dbType, "UUID") && len(x) == 16 {
			return fmt.Sprintf("%x-%x-%x-%x-%x", x[0:4], x[4:6], x[6:8], x[8:10], x[10:16])
		}
		return string(x)
	case *big.Rat:
		// numeric/decimal 保精度转为字符串。
		return x.String()
	case *big.Int:
		return x.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func isPostgreSQLTimeType(dbType string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "DATE", "TIMESTAMP", "TIMESTAMPTZ", "TIME", "TIMETZ":
		return true
	}
	return false
}

func isPostgreSQLNumericType(dbType string) bool {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "INT2", "INT4", "INT8", "INT", "INTEGER", "SMALLINT", "BIGINT",
		"SERIAL", "SMALLSERIAL", "BIGSERIAL",
		"NUMERIC", "DECIMAL", "REAL", "FLOAT4", "FLOAT8", "DOUBLE PRECISION", "MONEY":
		return true
	}
	return false
}

// ================================================================
// Writer 实现（PostgreSQL 作为同步目标端）
// ================================================================

// Writer 实现 dialect.Writer 接口，封装 PostgreSQL 目标数据库的写入操作。
type Writer struct{}

func (w *Writer) Dialect() string { return "postgresql" }

func (w *Writer) Open(dsn string) (*sql.DB, error) { return Open(dsn) }

func (w *Writer) BulkUpsert(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	return pgBulkUpsert(ctx, db, target, cols, rows, upsert)
}

func (w *Writer) Truncate(db *sql.DB, table string) error { return pgTruncate(db, table) }

func (w *Writer) TableExists(db *sql.DB, database, table string) (bool, error) {
	return pgTableExists(db, database, table)
}

func (w *Writer) ExecDDL(db *sql.DB, ddl string) error { return pgExecDDL(db, ddl) }

func (w *Writer) GenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	return pgGenerateCreate(db, table, cols, pk)
}

func (w *Writer) RowCount(db *sql.DB, table string) (int64, error) { return pgRowCount(db, table) }

func (w *Writer) PrimaryKey(db *sql.DB, database, table string) []string {
	return pgTargetPrimaryKey(db, table)
}

func (w *Writer) QuoteIdentifier(name string) string { return `"` + name + `"` }

func (w *Writer) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }

// pgPkCache 缓存各（连接, 目标表）的主键，避免每个批次重复查询 information_schema。
var pgPkCache sync.Map // key: fmt.Sprintf("%p|%s", db, target) -> []string

// pgBulkUpsert 批量写入 PostgreSQL。
// upsert=false 时使用普通多行 INSERT（全量首跑，所有行均为新行）；
// upsert=true 且有主键时使用 INSERT ... ON CONFLICT (pk) DO UPDATE SET（增量幂等）；
// upsert=true 且无主键时退化为普通 INSERT（非幂等，需配合 truncate 使用）。
// 单条语句参数上限沿用 PostgreSQL 协议层 65535 参数硬上限推导分块。
func pgBulkUpsert(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	n := len(rows)
	if n == 0 {
		return 0, nil
	}
	perRow := len(cols)
	if perRow == 0 {
		return 0, fmt.Errorf("批量写入 %s 失败: 列数为空", target)
	}

	const maxParams = 65535
	const maxRowsSoft = 5000
	maxRows := maxParams / perRow
	if maxRows > maxRowsSoft {
		maxRows = maxRowsSoft
	}
	if maxRows < 1 {
		maxRows = 1
	}

	pk := pgPrimaryKey(db, target)
	useUpsert := upsert && len(pk) > 0

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

	quotedCols := make([]string, perRow)
	for i, c := range cols {
		quotedCols[i] = `"` + c + `"`
	}
	colList := strings.Join(quotedCols, ", ")

	var total int64
	for start := 0; start < n; start += maxRows {
		end := start + maxRows
		if end > n {
			end = n
		}
		chunk := rows[start:end]

		// PostgreSQL 占位符为位置参数 $n，多行需跨行连续编号。
		var valueRows []string
		ph := 1
		for range chunk {
			phs := make([]string, perRow)
			for i := 0; i < perRow; i++ {
				phs[i] = fmt.Sprintf("$%d", ph)
				ph++
			}
			valueRows = append(valueRows, "("+strings.Join(phs, ", ")+")")
		}
		valueClause := strings.Join(valueRows, ", ")

		sqlStr := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s", quotePg(target), colList, valueClause)
		if useUpsert {
			pkSet := make(map[string]bool, len(pk))
			for _, k := range pk {
				pkSet[strings.ToUpper(k)] = true
			}
			pkQuoted := make([]string, 0, len(pk))
			for _, k := range pk {
				pkQuoted = append(pkQuoted, `"`+k+`"`)
			}
			conflictClause := strings.Join(pkQuoted, ", ")
			var setParts []string
			for _, c := range cols {
				if pkSet[strings.ToUpper(c)] {
					continue
				}
				setParts = append(setParts, fmt.Sprintf(`"%s" = EXCLUDED."%s"`, c, c))
			}
			if len(setParts) > 0 {
				sqlStr += fmt.Sprintf(" ON CONFLICT (%s) DO UPDATE SET %s", conflictClause, strings.Join(setParts, ", "))
			} else {
				// 表仅由主键构成（无其它列），用 DO NOTHING 保证幂等。
				sqlStr += fmt.Sprintf(" ON CONFLICT (%s) DO NOTHING", conflictClause)
			}
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

// pgPrimaryKey 查询（并缓存）目标表主键，供 ON CONFLICT 冲突目标使用。
func pgPrimaryKey(db *sql.DB, target string) []string {
	key := fmt.Sprintf("%p|%s", db, target)
	if v, ok := pgPkCache.Load(key); ok {
		return v.([]string)
	}
	pk := pgTargetPrimaryKey(db, target)
	pgPkCache.Store(key, pk)
	return pk
}

// pgTruncate 清空 PostgreSQL 目标表（支持 schema.table 形式）。
func pgTruncate(db *sql.DB, table string) error {
	q := "TRUNCATE TABLE " + quotePg(table)
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("清空表 %s 失败: %w", table, err)
	}
	return nil
}

// pgTableExists 判断 PostgreSQL 目标表是否存在。
func pgTableExists(db *sql.DB, database, table string) (bool, error) {
	schema, name := splitPgSchemaTable(table)
	q := `SELECT COUNT(*) FROM information_schema.tables WHERE table_name = $1 AND table_schema NOT IN ('pg_catalog','information_schema')`
	args := []interface{}{name}
	if schema != "" {
		q = `SELECT COUNT(*) FROM information_schema.tables WHERE table_name = $1 AND table_schema = $2`
		args = []interface{}{name, schema}
	}
	var cnt int
	if err := db.QueryRow(q, args...).Scan(&cnt); err != nil {
		return false, fmt.Errorf("查询表 %s 是否存在失败: %w", table, err)
	}
	return cnt > 0, nil
}

// pgExecDDL 在目标库执行一条 DDL。
func pgExecDDL(db *sql.DB, ddl string) error {
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("执行 DDL 失败: %w\nDDL: %s", err, ddl)
	}
	return nil
}

// pgRowCount 返回 PostgreSQL 目标表当前行数。
func pgRowCount(db *sql.DB, table string) (int64, error) {
	var n int64
	q := "SELECT COUNT(*) FROM " + quotePg(table)
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计目标表 %s 行数失败: %w", table, err)
	}
	return n, nil
}

// pgTargetPrimaryKey 查询 PostgreSQL 目标表主键（按 ordinal_position 排序）。
func pgTargetPrimaryKey(db *sql.DB, table string) []string {
	schema, name := splitPgSchemaTable(table)
	if schema == "" {
		schema = "public"
	}
	q := `SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND LOWER(tc.table_schema) = LOWER($1)
		  AND LOWER(tc.table_name) = LOWER($2)
		ORDER BY kcu.ordinal_position`
	rows, err := db.Query(q, schema, name)
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

// splitPgSchemaTable 拆分 schema.table 形式（PostgreSQL 标识符大小写不敏感时用户可能用小写）。
func splitPgSchemaTable(table string) (schema, name string) {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[:i], table[i+1:]
	}
	return "", table
}

// quotePg 用双引号包裹 PostgreSQL 标识符，支持 schema.table 逐段引用。
func quotePg(table string) string {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return `"` + table[:i] + `"."` + table[i+1:] + `"`
	}
	return `"` + table + `"`
}

// ----------------------------------------------------------------
// 类型映射（源类型 → PostgreSQL 列类型）
// ----------------------------------------------------------------

// pgType 将源数据库列类型映射为 PostgreSQL 列类型定义（含长度/精度）。
// 源类型名来自各源方言的 DatabaseTypeName：Oracle（NUMBER/VARCHAR2/...）、
// MySQL（INT/VARCHAR/DATETIME/JSON/...）、SQL Server（NVARCHAR/INT/DATETIME2/...）
// 以及 PostgreSQL 自身（用于潜在的 PG→PG 场景）。
func pgType(c types.Column) (string, error) {
	t := strings.ToUpper(strings.TrimSpace(c.DBType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "VARCHAR", "NVARCHAR", "VARCHAR2", "NVARCHAR2", "BPCHAR":
		return pgVarcharType(c.Length, false), nil
	case "CHAR", "NCHAR":
		return pgCharType(c.Length), nil
	case "TEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT",
		"CLOB", "NCLOB", "LONG", "NTEXT", "CITEXT", "NAME":
		return "TEXT", nil
	case "INT", "INTEGER", "MEDIUMINT", "INT4", "SERIAL":
		return "INTEGER", nil
	case "BIGINT", "INT8", "BIGSERIAL":
		return "BIGINT", nil
	case "SMALLINT", "INT2", "SMALLSERIAL":
		return "SMALLINT", nil
	case "TINYINT":
		// MySQL TINYINT(1) 常作布尔；但 TINYINT 也可能存 0-255 数值，
		// 为兼容无符号溢出统一映射到 SMALLINT（与 SQL Server 目标一致）。
		return "SMALLINT", nil
	case "DECIMAL", "NUMERIC", "NUMBER":
		return pgNumberType(c.Precision, c.Scale), nil
	case "MONEY", "SMALLMONEY":
		return "NUMERIC(19,4)", nil
	case "FLOAT", "BINARY_FLOAT", "FLOAT4", "REAL":
		return "REAL", nil
	case "BINARY_DOUBLE", "DOUBLE", "DOUBLE PRECISION", "FLOAT8":
		return "DOUBLE PRECISION", nil
	case "BOOLEAN", "BOOL", "BIT":
		return "BOOLEAN", nil
	case "DATE":
		return "DATE", nil
	case "TIME", "TIMETZ":
		return "TIME", nil
	case "DATETIME", "DATETIME2", "SMALLDATETIME", "TIMESTAMP",
		"OCIDATE", "TIMESTAMPDTY", "TIMESTAMPTZ_DTY", "TIMESTAMPELTZ",
		"TIMESTAMPLTZ_DTY", "TIMESTAMP_WITH_TZ", "TIMESTAMP_WITH_LOCAL_TZ":
		return "TIMESTAMP", nil
	case "TIMESTAMPTZ", "DATETIMEOFFSET":
		return "TIMESTAMPTZ", nil
	case "YEAR":
		return "SMALLINT", nil
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB",
		"RAW", "LONG RAW", "BINARY", "VARBINARY", "IMAGE", "BYTEA":
		return "BYTEA", nil
	case "UUID", "UNIQUEIDENTIFIER":
		return "UUID", nil
	case "JSON", "JSONB":
		return "JSONB", nil
	case "XML":
		return "XML", nil
	case "ENUM", "SET":
		return "TEXT", nil
	default:
		return "", fmt.Errorf("未知源类型: %s（列 %s）", c.DBType, c.Name)
	}
}

// pgVarcharType 生成 PG 的 VARCHAR 定义；长度无效或过大时退化为 TEXT。
func pgVarcharType(length int64, isChar bool) string {
	if length <= 0 {
		if isChar {
			return "CHAR(1)"
		}
		return "TEXT"
	}
	const pgMaxVarlen = 10485760
	if length > pgMaxVarlen {
		return "TEXT"
	}
	return fmt.Sprintf("VARCHAR(%d)", length)
}

// pgCharType 生成 PG 的 CHAR 定义。
func pgCharType(length int64) string {
	if length <= 0 {
		length = 1
	}
	return fmt.Sprintf("CHAR(%d)", length)
}

// pgNumberType 根据精度/小数位生成 PG 的 NUMERIC 定义。
func pgNumberType(precision, scale int) string {
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
		return fmt.Sprintf("NUMERIC(%d,%d)", p, s)
	}
	switch {
	case precision <= 0:
		return "NUMERIC"
	case precision <= 4:
		return "SMALLINT"
	case precision <= 9:
		return "INTEGER"
	case precision <= 18:
		return "BIGINT"
	default:
		return fmt.Sprintf("NUMERIC(%d,0)", precision)
	}
}

// pgGenerateCreate 根据源列元数据生成 PostgreSQL 的 CREATE TABLE 语句。
// PostgreSQL 9.1+ 支持 CREATE TABLE IF NOT EXISTS，故此处可带 IF NOT EXISTS；
// 重复建表的防护仍由调用方 ensureTable 先查 TableExists 兜底。
func pgGenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	if len(cols) == 0 {
		return "", fmt.Errorf("表 %s 没有任何列，无法生成建表语句", table)
	}
	defs := make([]string, 0, len(cols)+1)
	pkSet := make(map[string]bool, len(pk))
	for _, k := range pk {
		pkSet[strings.ToUpper(k)] = true
	}

	for _, c := range cols {
		mt, err := pgType(c)
		if err != nil {
			return "", err
		}
		line := fmt.Sprintf("  \"%s\" %s", c.Name, mt)
		if !c.Nullable {
			line += " NOT NULL"
		}
		defs = append(defs, line)
	}

	if len(pk) > 0 {
		quoted := make([]string, 0, len(pk))
		for _, k := range pk {
			quoted = append(quoted, fmt.Sprintf("\"%s\"", k))
		}
		defs = append(defs, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(quoted, ", ")))
	}

	body := strings.Join(defs, ",\n")
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n%s\n)", quotePg(table), body), nil
}
