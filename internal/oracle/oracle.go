package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/sijms/go-ora/v2"
	"github.com/sirupsen/logrus"

	"godbsync/internal/dialect"
	"godbsync/internal/types"
)

func init() {
	dialect.RegisterReader("oracle", func() dialect.Reader { return &Reader{} })
}

// oracleArrayBinding 控制是否对 Oracle 目标写入使用「数组绑定」(array binding)。
// 默认开启以获得更优的批量写入性能；若所用 go-ora 版本/环境对数组绑定支持异常
// （执行报错或产生异常数据），可经 SetArrayBinding(false) 关闭，回退到已验证的
// UNION ALL 行展开写法。关闭后即便数组绑定报错也不会再尝试，直接走传统路径。
var oracleArrayBinding = true

// oracleLog 为 Oracle 方言包内的日志器，由 cmd 在启动时注入（复用主日志格式）。
var oracleLog = logrus.StandardLogger()

// SetArrayBinding 设置是否启用 Oracle 数组绑定（默认 true）。
func SetArrayBinding(enable bool) { oracleArrayBinding = enable }

// SetLogger 注入日志器，使回退告警与主程序日志格式一致。
func SetLogger(l *logrus.Logger) {
	if l != nil {
		oracleLog = l
	}
}

// Reader 实现 dialect.Reader 接口，封装 Oracle 源数据库的读取操作。
type Reader struct{}

func (r *Reader) Dialect() string { return "oracle" }

func (r *Reader) Open(dsn string) (*sql.DB, error) { return Open(dsn) }

func (r *Reader) TableColumns(db *sql.DB, tableRef string) ([]types.Column, error) {
	return TableColumns(db, tableRef)
}

func (r *Reader) PrimaryKey(db *sql.DB, schema, table string) []string {
	return PrimaryKey(db, schema, table)
}

func (r *Reader) RowCount(db *sql.DB, tableRef, where string, args []interface{}) (int64, error) {
	return RowCount(db, tableRef, where, args)
}

func (r *Reader) ScanRow(rows *sql.Rows, n int) ([]interface{}, error) {
	return ScanRow(rows, n)
}

func (r *Reader) Placeholder(n int) string {
	return fmt.Sprintf(":%d", n)
}

func (r *Reader) QuoteIdentifier(name string) string {
	return `"` + name + `"`
}

func (r *Reader) SupportsRowID() bool { return true }

func (r *Reader) Normalize(v interface{}, dbType string) interface{} { return normalize(v) }

func (r *Reader) IsTimeType(dbType string) bool { return isTimeType(dbType) }

func (r *Reader) IsNumericType(dbType string) bool { return isNumericType(dbType) }

// Open 打开一个 Oracle 连接。
// go-ora 是纯 Go 实现的 Oracle 驱动，无需安装 Oracle 客户端。
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 Oracle 连接失败: %w", err)
	}
	// go-ora 建议在 Open 后做一次 Ping 以尽早发现连接问题。
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("连接 Oracle 失败(Ping): %w", err)
	}
	return db, nil
}

// TableColumns 读取 Oracle 源表的列元数据。
// 通过 "SELECT * FROM <table> WHERE 1=0" 获取列类型，避免依赖 DBA 视图权限。
func TableColumns(db *sql.DB, table string) ([]types.Column, error) {
	rows, err := db.Query(fmt.Sprintf("SELECT * FROM %s WHERE 1=0", table))
	if err != nil {
		return nil, fmt.Errorf("读取表 %s 列元数据失败: %w", table, err)
	}
	defer rows.Close()

	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("获取表 %s 列类型失败: %w", table, err)
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
		return nil, fmt.Errorf("表 %s 未返回任何列，请确认表名是否正确", table)
	}
	return cols, nil
}

// PrimaryKey 查询 Oracle 表的主键列（按位置排序）。
// 当 owner 为空时，使用 user_constraints / user_cons_columns（当前用户拥有的表）；
// 当指定 owner 时（表属于其他 schema），改用 all_constraints / all_cons_columns 并按属主过滤，
// 以便在没有 DBA 权限、仅对源表有 SELECT 权限的场景下也能取到主键。
// 若查询失败或表无主键，返回空切片。
func PrimaryKey(db *sql.DB, owner, table string) []string {
	var q string
	var args []interface{}
	if owner == "" {
		q = `SELECT cc.column_name
			FROM user_constraints c
			JOIN user_cons_columns cc ON c.constraint_name = cc.constraint_name
			WHERE c.constraint_type = 'P' AND c.table_name = :1
			ORDER BY cc.position`
		args = []interface{}{table}
	} else {
		q = `SELECT cc.column_name
			FROM all_constraints c
			JOIN all_cons_columns cc ON c.owner = cc.owner AND c.constraint_name = cc.constraint_name
			WHERE c.constraint_type = 'P' AND c.owner = :1 AND c.table_name = :2
			ORDER BY cc.position`
		args = []interface{}{owner, table}
	}
	rows, err := db.Query(q, args...)
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

// RowCount 统计 Oracle 表行数，可附带 WHERE 条件与参数（Oracle 占位符 :1/:2）。
// where 为空时不追加 WHERE 子句。
func RowCount(db *sql.DB, tableRef, where string, args []interface{}) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s", tableRef)
	if where != "" {
		q += " WHERE " + where
	}
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计表 %s 行数失败: %w", tableRef, err)
	}
	return n, nil
}

// ScanRow 将当前行扫描为 []interface{}，保留驱动解码出的原生 Go 值。
// 注意：database/sql 在 Scan 时会拷贝数据，因此返回的值可安全跨行保留。
func ScanRow(rows *sql.Rows, n int) ([]interface{}, error) {
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

// normalize 将扫描出的驱动原生值规整为目标驱动能可靠绑定的基础类型。
// 对于 go-ora 可能返回的 big.Int/自定义 decimal 等类型，转为其字符串表示，
// 由目标驱动在写入时按列类型完成转换，从而完整保留精度。
func normalize(v interface{}) interface{} {
	switch v.(type) {
	case nil, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64, string, []byte, bool, time.Time:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func isTimeType(dbType string) bool {
	t := strings.ToUpper(dbType)
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "DATE", "TIMESTAMP", "TIMESTAMP_WITH_TZ", "TIMESTAMP_WITH_LOCAL_TZ", "DATETIME",
		"OCIDATE", "TIMESTAMPDTY", "TIMESTAMPTZ_DTY", "TIMESTAMPELTZ",
		"TIMESTAMPLTZ_DTY", "TIMESTAMPTZ", "TIMETZ":
		return true
	}
	return false
}

func isNumericType(dbType string) bool {
	t := strings.ToUpper(dbType)
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "NUMBER", "NUMERIC", "FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE", "DOUBLE", "DOUBLE PRECISION", "REAL",
		"BFLOAT", "BDOUBLE", "IBFLOAT", "IBDOUBLE":
		return true
	}
	return false
}

// ----------------------------------------------------------------
// Writer 实现（Oracle 作为同步目标端）
// ----------------------------------------------------------------

func init() {
	// oracle 包同时提供 Reader（作为源端）与 Writer（作为目标端）。
	dialect.RegisterWriter("oracle", func() dialect.Writer { return &Writer{} })
}

// Writer 实现 dialect.Writer 接口，封装 Oracle 目标数据库的写入操作。
type Writer struct{}

func (w *Writer) Dialect() string { return "oracle" }

func (w *Writer) Open(dsn string) (*sql.DB, error) { return Open(dsn) }

func (w *Writer) BulkUpsert(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	return oracleBulkUpsert(ctx, db, target, cols, rows, upsert)
}

func (w *Writer) Truncate(db *sql.DB, table string) error { return oracleTruncate(db, table) }

func (w *Writer) TableExists(db *sql.DB, database, table string) (bool, error) {
	return oracleTableExists(db, database, table)
}

func (w *Writer) ExecDDL(db *sql.DB, ddl string) error { return oracleExecDDL(db, ddl) }

func (w *Writer) GenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	return oracleGenerateCreate(db, table, cols, pk)
}

func (w *Writer) RowCount(db *sql.DB, table string) (int64, error) { return oracleRowCount(db, table) }

func (w *Writer) PrimaryKey(db *sql.DB, database, table string) []string {
	owner, name := splitOwnerTable(table)
	if owner == "" {
		owner = database
	}
	return PrimaryKey(db, owner, name)
}

func (w *Writer) QuoteIdentifier(name string) string { return `"` + name + `"` }

func (w *Writer) Placeholder(n int) string { return fmt.Sprintf(":%d", n) }

// oracleTruncate 清空 Oracle 目标表（支持 owner.table 形式）。
func oracleTruncate(db *sql.DB, table string) error {
	q := "TRUNCATE TABLE " + quoteID(table)
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("清空表 %s 失败: %w", table, err)
	}
	return nil
}

// oracleTableExists 判断 Oracle 目标表是否存在。
func oracleTableExists(db *sql.DB, database, table string) (bool, error) {
	owner, name := splitOwnerTable(table)
	if owner == "" {
		owner = database
	}
	var n int
	var err error
	if owner == "" {
		q := `SELECT COUNT(*) FROM user_tables WHERE TABLE_NAME = :1`
		err = db.QueryRow(q, strings.ToUpper(name)).Scan(&n)
	} else {
		q := `SELECT COUNT(*) FROM all_tables WHERE OWNER = :1 AND TABLE_NAME = :2`
		err = db.QueryRow(q, strings.ToUpper(owner), strings.ToUpper(name)).Scan(&n)
	}
	if err != nil {
		return false, fmt.Errorf("查询表 %s 是否存在失败: %w", table, err)
	}
	return n > 0, nil
}

// oracleExecDDL 在 Oracle 目标库执行一条 DDL。
func oracleExecDDL(db *sql.DB, ddl string) error {
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("执行 DDL 失败: %w\nDDL: %s", err, ddl)
	}
	return nil
}

// oracleRowCount 返回 Oracle 目标表当前行数。
func oracleRowCount(db *sql.DB, table string) (int64, error) {
	var n int64
	q := "SELECT COUNT(*) FROM " + quoteID(table)
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计目标表 %s 行数失败: %w", table, err)
	}
	return n, nil
}

// oracleVersion 记录目标 Oracle 服务端版本，用于按版本自动降级 DDL（如 9i 无 BINARY_FLOAT/DOUBLE）。
type oracleVersion struct{ major, minor, patch int }

// atLeast 判断版本是否不低于 major.minor.patch。
func (v oracleVersion) atLeast(major, minor, patch int) bool {
	if v.major != major {
		return v.major > major
	}
	if v.minor != minor {
		return v.minor > minor
	}
	return v.patch >= patch
}

// oracleVerCache 按 *sql.DB 缓存探测到的版本，避免每次建表都查版本视图。
var oracleVerCache sync.Map

// detectOracleVersion 探测目标 Oracle 版本（缓存于 db 指针）。
// db 为 nil（如测试）或探测失败时使用现代版本默认值，保证已测的 12c 路径不变。
// product_component_version 为 PUBLIC 可读的目录视图，任意连接用户均可查询。
func detectOracleVersion(db *sql.DB) oracleVersion {
	def := oracleVersion{major: 12, minor: 0, patch: 0}
	if db == nil {
		return def
	}
	if v, ok := oracleVerCache.Load(db); ok {
		return v.(oracleVersion)
	}
	v := def
	var raw string
	if err := db.QueryRow("SELECT version FROM product_component_version WHERE product LIKE 'Oracle Database%'").Scan(&raw); err == nil {
		if parsed, ok := parseOracleVersion(raw); ok {
			v = parsed
		}
	}
	oracleVerCache.Store(db, v)
	return v
}

// parseOracleVersion 解析 Oracle 版本串（如 "19.0.0.0.0"、"11.2.0.4.0"、"9.2.0.8.0"）。
func parseOracleVersion(raw string) (oracleVersion, bool) {
	nums := strings.Split(raw, ".")
	var v oracleVersion
	if len(nums) >= 1 {
		fmt.Sscanf(nums[0], "%d", &v.major)
	}
	if len(nums) >= 2 {
		fmt.Sscanf(nums[1], "%d", &v.minor)
	}
	if len(nums) >= 3 {
		fmt.Sscanf(nums[2], "%d", &v.patch)
	}
	if v.major == 0 {
		return v, false
	}
	return v, true
}

// oracleGenerateCreate 根据源列元数据（MySQL 等来源）生成 Oracle 的 CREATE TABLE 语句。
// db 为目标库连接，用于按版本自动降级类型（9i 无 BINARY_FLOAT/DOUBLE 时改用 NUMBER）。
// Oracle 不支持 IF NOT EXISTS，调用方（ensureTable）会先判定表不存在再执行。
func oracleGenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error) {
	if len(cols) == 0 {
		return "", fmt.Errorf("表 %s 没有任何列，无法生成建表语句", table)
	}
	ver := detectOracleVersion(db)
	owner, name := splitOwnerTable(table)
	defs := make([]string, 0, len(cols)+1)
	pkSet := make(map[string]bool, len(pk))
	for _, k := range pk {
		pkSet[strings.ToUpper(k)] = true
	}

	for _, c := range cols {
		mt, err := oracleType(c, ver)
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

	tableName := name
	if owner != "" {
		tableName = owner + "\".\"" + name
	}
	body := strings.Join(defs, ",\n")
	return fmt.Sprintf("CREATE TABLE \"%s\" (\n%s\n)", tableName, body), nil
}

// oracleType 将源数据库列类型（典型为 MySQL 类型名）映射为 Oracle 列类型定义。
// ver 为目标 Oracle 版本：10g+ 用 BINARY_FLOAT/BINARY_DOUBLE，更早版本（如 9i）退化为 NUMBER。
func oracleType(c types.Column, ver oracleVersion) (string, error) {
	t := strings.ToUpper(strings.TrimSpace(c.DBType))
	base := t
	if i := strings.Index(t, "("); i >= 0 {
		base = t[:i]
	}
	switch base {
	case "VARCHAR", "NVARCHAR", "VARCHAR2", "NVARCHAR2", "BPCHAR":
		n := c.Length
		if n <= 0 {
			return "VARCHAR2(4000)", nil
		}
		if n > 4000 {
			return "CLOB", nil
		}
		return fmt.Sprintf("VARCHAR2(%d)", n), nil
	case "CHAR", "NCHAR":
		n := c.Length
		if n <= 0 {
			n = 1
		}
		if n > 2000 {
			n = 2000
		}
		return fmt.Sprintf("CHAR(%d)", n), nil
	case "INT", "INTEGER", "MEDIUMINT", "INT4", "SERIAL", "SMALLSERIAL":
		return "NUMBER(10,0)", nil
	case "BIGINT", "INT8", "BIGSERIAL":
		return "NUMBER(19,0)", nil
	case "SMALLINT", "INT2":
		return "NUMBER(5,0)", nil
	case "TINYINT", "BOOLEAN", "BOOL", "BIT":
		// TINYINT(1) 常用于布尔语义
		if base == "TINYINT" && c.Length == 1 {
			return "NUMBER(1,0)", nil
		}
		return "NUMBER(3,0)", nil
	case "DECIMAL", "NUMERIC":
		return oracleNumberType(c.Precision, c.Scale), nil
	case "FLOAT", "FLOAT4":
		// BINARY_FLOAT 自 10g 引入；9i 及更早退化为 NUMBER（精度足够，仅损失二进制浮点存储）。
		if ver.atLeast(10, 0, 0) {
			return "BINARY_FLOAT", nil
		}
		return "NUMBER(38,10)", nil
	case "DOUBLE", "DOUBLE PRECISION", "REAL", "FLOAT8":
		if ver.atLeast(10, 0, 0) {
			return "BINARY_DOUBLE", nil
		}
		return "NUMBER(38,15)", nil
	case "DATE", "DATETIME", "TIMESTAMP", "TIME", "TIMESTAMPTZ":
		return "TIMESTAMP", nil
	case "YEAR":
		return "NUMBER(4,0)", nil
	case "TEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT", "CITEXT", "NAME":
		return "CLOB", nil
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BYTEA":
		return "BLOB", nil
	case "JSON", "JSONB":
		return "CLOB", nil
	case "UUID":
		return "VARCHAR2(36)", nil
	case "ENUM", "SET":
		return "VARCHAR2(255)", nil
	default:
		return "", fmt.Errorf("未知源类型: %s（列 %s）", c.DBType, c.Name)
	}
}

func oracleNumberType(precision, scale int) string {
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
		return fmt.Sprintf("NUMBER(%d,%d)", p, s)
	}
	switch {
	case precision <= 0:
		return "NUMBER(38,0)"
	case precision <= 2:
		return "NUMBER(3,0)"
	case precision <= 4:
		return "NUMBER(5,0)"
	case precision <= 9:
		return "NUMBER(10,0)"
	case precision <= 18:
		return "NUMBER(19,0)"
	default:
		return fmt.Sprintf("NUMBER(%d,0)", precision)
	}
}

// oracleBulkUpsert 批量写入 Oracle。优先使用 go-ora 的「数组绑定」(array binding)：
// 把整批行按列转置为若干 []interface{} 数组，一条 INSERT/MERGE 语句中每个占位符绑定
// 一个列数组，由驱动在单次网络往返内完成整批写入，语句长度与占位符数从
// 「行数×列数」降为常数（=列数），彻底消除巨型 SQL 的硬解析开销。
// 若数组绑定执行失败（个别 go-ora 版本/环境不支持），整体回退到已验证的
// UNION ALL 行展开写法（见 oracleBulkUpsertImpl 的 useArray=false 分支），保证写入正确性。
// upsert=false 时强制普通 INSERT（全量首跑，所有行均为新行，且避免 MERGE 的匹配开销）；
// upsert=true 且有主键时按主键构建 MERGE（命中更新非主键列、未命中插入）。
// ctx 为 nil 时不限制超时。
func oracleBulkUpsert(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error) {
	n := len(rows)
	if n == 0 {
		return 0, nil
	}
	perRow := len(cols)
	if perRow == 0 {
		return 0, fmt.Errorf("批量写入 %s 失败: 列数为空", target)
	}
	if oracleArrayBinding {
		total, err := oracleBulkUpsertImpl(ctx, db, target, cols, rows, upsert, true)
		if err == nil {
			return total, nil
		}
		// 数组绑定执行失败：整体回退到已验证的 UNION ALL 行展开写法，保证写入正确。
		oracleLog.Warnf("Oracle 数组绑定写入失败，回退到传统行展开写法: %v", err)
	}
	return oracleBulkUpsertImpl(ctx, db, target, cols, rows, upsert, false)
}

// oracleBulkUpsertImpl 批量写入 Oracle 的实际实现。
// useArray=true 时使用 go-ora 数组绑定（每个占位符绑定一列数组，单条语句展开整批）；
// useArray=false 时回退到 UNION ALL 行展开写法（每行一组 SELECT ... FROM dual），
// 语句更长但已被充分验证。
// upsert=false 强制普通 INSERT；upsert=true 且目标表有主键时按主键构建 MERGE。
// 整批共用一个事务，失败即整体回滚，调用方据此决定是否回退到另一种写法。ctx 为 nil 时不限制超时。
func oracleBulkUpsertImpl(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert, useArray bool) (int64, error) {
	n := len(rows)
	perRow := len(cols)

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

	// 仅当 upsert 且目标表有主键时才走 MERGE；否则普通 INSERT。
	var pk []string
	useMerge := upsert
	if useMerge {
		pk = oraclePrimaryKey(db, target)
		useMerge = len(pk) > 0
	}

	// 数组绑定：一次转置整批，每个占位符绑定一个列数组。
	var colVals [][]interface{}
	if useArray {
		colVals = make([][]interface{}, perRow)
		for c := range colVals {
			colVals[c] = make([]interface{}, n)
		}
		for r := 0; r < n; r++ {
			row := rows[r]
			for c := 0; c < perRow; c++ {
				colVals[c][r] = row[c]
			}
		}
	}

	// 分批大小：数组绑定按内存/网络包上限取 5000；传统行展开按占位符上限（~65535）折算。
	maxRows := 5000
	if !useArray {
		const maxPlaceholders = 60000
		maxRows = maxPlaceholders / perRow
		if maxRows < 1 {
			maxRows = 1
		}
	}

	var total int64
	for start := 0; start < n; start += maxRows {
		end := start + maxRows
		if end > n {
			end = n
		}
		chunk := rows[start:end]

		var sqlStr string
		var args []interface{}
		if useArray {
			if useMerge {
				sqlStr = buildMergeSQLArray(target, cols, pk)
			} else {
				sqlStr = buildInsertSQLArray(target, cols)
			}
			// 每个列取本子块切片，触发 go-ora 数组绑定（一次性写入该子块）。
			args = make([]interface{}, perRow)
			for c := 0; c < perRow; c++ {
				args[c] = colVals[c][start:end]
			}
		} else {
			if useMerge {
				sqlStr = buildMergeSQL(target, cols, pk, len(chunk))
			} else {
				sqlStr = buildInsertSQL(target, cols, len(chunk))
			}
			args = make([]interface{}, 0, len(chunk)*perRow)
			for _, r := range chunk {
				for _, v := range r {
					args = append(args, v)
				}
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
		total += int64(end - start)
	}

	if err = tx.Commit(); err != nil {
		return total, fmt.Errorf("提交事务失败: %w", err)
	}
	return total, nil
}

// buildInsertSQLArray 生成数组绑定版的单行多列 INSERT：
// 每个占位符 :n 绑定一个列数组（由 go-ora 在单次执行中展开为整批行）。
func buildInsertSQLArray(target string, cols []string) string {
	ph := make([]string, len(cols))
	qcols := make([]string, len(cols))
	for i := range cols {
		ph[i] = fmt.Sprintf(":%d", i+1)
		qcols[i] = quoteCol(cols[i])
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		quoteID(target), strings.Join(qcols, ", "), strings.Join(ph, ", "))
}

// buildMergeSQLArray 生成数组绑定版 MERGE：USING 仅一个 SELECT，
// 每个占位符 :n 绑定一列数组（由 go-ora 在单次执行中展开为整批行）。
func buildMergeSQLArray(target string, cols []string, pk []string) string {
	pkSet := make(map[string]bool, len(pk))
	for _, k := range pk {
		pkSet[strings.ToUpper(k)] = true
	}
	selects := make([]string, len(cols))
	for i := range cols {
		selects[i] = fmt.Sprintf(":%d c%d", i+1, i)
	}
	using := "SELECT " + strings.Join(selects, ", ") + " FROM dual"

	onParts := make([]string, 0, len(pk))
	for i, c := range cols {
		if pkSet[strings.ToUpper(c)] {
			onParts = append(onParts, fmt.Sprintf("t.%s = s.c%d", quoteCol(c), i))
		}
	}
	onClause := strings.Join(onParts, " AND ")

	var updateClause string
	var updates []string
	for i, c := range cols {
		if pkSet[strings.ToUpper(c)] {
			continue
		}
		updates = append(updates, fmt.Sprintf("t.%s = s.c%d", quoteCol(c), i))
	}
	if len(updates) > 0 {
		updateClause = " WHEN MATCHED THEN UPDATE SET " + strings.Join(updates, ", ")
	}

	insertCols := make([]string, len(cols))
	insertVals := make([]string, len(cols))
	for i, c := range cols {
		insertCols[i] = quoteCol(c)
		insertVals[i] = fmt.Sprintf("s.c%d", i)
	}
	insertClause := fmt.Sprintf(" WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s)",
		strings.Join(insertCols, ", "), strings.Join(insertVals, ", "))

	return fmt.Sprintf("MERGE INTO %s t USING (%s) s ON (%s)%s %s",
		quoteID(target), using, onClause, updateClause, insertClause)
}

// buildInsertSQL 生成无主键时的普通多行 INSERT（非幂等），用 UNION ALL 把每行展开为
// 一组 SELECT ... FROM dual。作为数组绑定的回退路径，已被充分验证。
func buildInsertSQL(target string, cols []string, rowCount int) string {
	ph := 0
	nextPH := func() string {
		ph++
		return fmt.Sprintf(":%d", ph)
	}
	selectRow := func() string {
		parts := make([]string, len(cols))
		for i := range cols {
			parts[i] = nextPH()
		}
		return "SELECT " + strings.Join(parts, ", ") + " FROM dual"
	}
	selects := make([]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		selects = append(selects, selectRow())
	}
	insertCols := make([]string, len(cols))
	for i, c := range cols {
		insertCols[i] = quoteCol(c)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) %s",
		quoteID(target), strings.Join(insertCols, ", "), strings.Join(selects, " UNION ALL "))
}

// buildMergeSQL 生成 Oracle MERGE 语句（数组绑定的回退路径）：把 chunk 行按序展开为
// USING 子查询，以主键列做 ON 匹配，命中更新非主键列、未命中插入整行。占位符 :1.. 跨所有行
// 顺序编号，与 args 扁平化顺序一致。已被充分验证。
func buildMergeSQL(target string, cols []string, pk []string, rowCount int) string {
	pkSet := make(map[string]bool, len(pk))
	for _, k := range pk {
		pkSet[strings.ToUpper(k)] = true
	}
	ph := 0
	nextPH := func() string {
		ph++
		return fmt.Sprintf(":%d", ph)
	}

	selectRow := func() string {
		parts := make([]string, len(cols))
		for i := range cols {
			parts[i] = fmt.Sprintf("%s c%d", nextPH(), i)
		}
		return "SELECT " + strings.Join(parts, ", ") + " FROM dual"
	}
	selects := make([]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		selects = append(selects, selectRow())
	}
	using := strings.Join(selects, " UNION ALL ")

	onParts := make([]string, 0, len(pk))
	for i, c := range cols {
		if pkSet[strings.ToUpper(c)] {
			onParts = append(onParts, fmt.Sprintf("t.%s = s.c%d", quoteCol(c), i))
		}
	}
	onClause := strings.Join(onParts, " AND ")

	var updateClause string
	var updates []string
	for i, c := range cols {
		if pkSet[strings.ToUpper(c)] {
			continue
		}
		updates = append(updates, fmt.Sprintf("t.%s = s.c%d", quoteCol(c), i))
	}
	if len(updates) > 0 {
		updateClause = " WHEN MATCHED THEN UPDATE SET " + strings.Join(updates, ", ")
	}

	insertCols := make([]string, len(cols))
	insertVals := make([]string, len(cols))
	for i, c := range cols {
		insertCols[i] = quoteCol(c)
		insertVals[i] = fmt.Sprintf("s.c%d", i)
	}
	insertClause := fmt.Sprintf(" WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s)",
		strings.Join(insertCols, ", "), strings.Join(insertVals, ", "))

	return fmt.Sprintf("MERGE INTO %s t USING (%s) s ON (%s)%s %s",
		quoteID(target), using, onClause, updateClause, insertClause)
}

// oraclePrimaryKey 查询目标表主键列，按 *sql.DB+表名 缓存（建表后首次写入时填充）。
func oraclePrimaryKey(db *sql.DB, target string) []string {
	key := fmt.Sprintf("%p|%s", db, target)
	if v, ok := pkCache.Load(key); ok {
		return v.([]string)
	}
	owner, name := splitOwnerTable(target)
	pk := PrimaryKey(db, owner, name)
	pkCache.Store(key, pk)
	return pk
}

var pkCache sync.Map

// splitOwnerTable 将 "owner.table" 拆分为 (owner, table)；无点时返回 ("", table)。
func splitOwnerTable(table string) (owner, name string) {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[:i], table[i+1:]
	}
	return "", table
}

// quoteID 用 Oracle 双引号包裹标识符，支持 owner.table 形式。
func quoteID(table string) string {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return `"` + table[:i] + `"."` + table[i+1:] + `"`
	}
	return `"` + table + `"`
}

// quoteCol 用 Oracle 双引号包裹列名。
func quoteCol(col string) string { return `"` + col + `"` }
