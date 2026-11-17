package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"godbsync/internal/types"
)

// Reader 抽象源数据库的读取操作，使 syncer 不直接依赖具体驱动包。
// 每种数据库（Oracle、MySQL、PostgreSQL 等）实现此接口即可作为同步源端。
type Reader interface {
	// Dialect 返回方言标识，如 "oracle"、"mysql"。
	Dialect() string
	// Open 打开数据库连接。
	Open(dsn string) (*sql.DB, error)

	// TableColumns 读取源表的列元数据。
	TableColumns(db *sql.DB, tableRef string) ([]types.Column, error)
	// PrimaryKey 查询源表主键列（按位置排序），无主键时返回空切片。
	PrimaryKey(db *sql.DB, schema, table string) []string
	// RowCount 统计源表行数，可附带 WHERE 条件与参数。
	RowCount(db *sql.DB, tableRef, where string, args []interface{}) (int64, error)
	// ScanRow 将当前行扫描为 []interface{}，保留驱动原生值。
	ScanRow(rows *sql.Rows, n int) ([]interface{}, error)

	// Placeholder 返回第 n 个占位符（从 1 开始）。
	// Oracle: ":1", MySQL/PostgreSQL: "?"。
	Placeholder(n int) string
	// QuoteIdentifier 用本方言的引号包裹标识符。
	// Oracle: `"col"`, MySQL: `` `col` ``。
	QuoteIdentifier(name string) string
	// SupportsRowID 返回本方言是否支持 ROWID 伪列（仅 Oracle 为 true）。
	SupportsRowID() bool
	// Normalize 将驱动原生值规整为可跨库绑定的基础类型。
	// dbType 为列的原始数据库类型名（如 "UNIQUEIDENTIFIER"），方言据此做差异化规整。
	Normalize(v interface{}, dbType string) interface{}
	// IsTimeType 判断给定的数据库类型名是否为时间类型。
	IsTimeType(dbType string) bool
	// IsNumericType 判断给定的数据库类型名是否为数值类型。
	IsNumericType(dbType string) bool
	// ListTables 枚举源库（schema）下所有基表名，用于通配符 * 整库同步展开。
	ListTables(db *sql.DB, schema string) ([]string, error)
	// SamplePKQuery 生成「按 orderCols 有序、取第 [offset, offset+limit) 行」的主键抽样查询，
	// 供校验阶段抽样比对使用；返回的查询只投影 orderCols 这些主键列。
	// 分页语法由各方言自行实现：MySQL/PostgreSQL 用 LIMIT/OFFSET，Oracle 用 ROWNUM 子查询，
	// SQL Server 用 ROW_NUMBER()。syncer 不硬编码任何窗口函数，从而兼容不支持
	// ROW_NUMBER() OVER 的老版本数据库（如 MySQL 5.7）。
	// orderCols 为未加引号的原始列名，由方言内部用 QuoteIdentifier 自行引用。
	SamplePKQuery(tableRef string, orderCols []string, offset, limit int64) string
}

// Writer 抽象目标数据库的写入操作，使 syncer 不直接依赖具体驱动包。
// 每种数据库实现此接口即可作为同步目标端。
type Writer interface {
	// Dialect 返回方言标识。
	Dialect() string
	// Open 打开数据库连接。
	Open(dsn string) (*sql.DB, error)

	// BulkUpsert 批量写入。upsert=true 时使用幂等 upsert（如 MERGE / ON DUPLICATE KEY UPDATE），
	// 适用于增量同步；upsert=false 时使用普通 INSERT，适用于 truncate 后的全量首跑（所有行均为新行，
	// 且避免 MERGE 的匹配开销）。ctx 为 nil 时不限制超时。
	BulkUpsert(ctx context.Context, db *sql.DB, target string, cols []string, rows [][]interface{}, upsert bool) (int64, error)
	// Truncate 清空目标表。
	Truncate(db *sql.DB, table string) error

	// TableExists 判断目标库中指定表是否存在。
	TableExists(db *sql.DB, database, table string) (bool, error)
	// ExecDDL 执行一条 DDL 语句。
	ExecDDL(db *sql.DB, ddl string) error
	// GenerateCreate 根据源列元数据生成本方言的 CREATE TABLE 语句。
	// db 为目标库连接，方言可据此探测服务端版本并自动降级 DDL（如老版本 MySQL/Oracle）。
	GenerateCreate(db *sql.DB, table string, cols []types.Column, pk []string) (string, error)
	// GenerateAlter 比对目标表现有列与（已映射为目标列名的）源列，生成使其一致的 ALTER TABLE 语句。
	// 仅处理两类差异：目标缺列（ADD COLUMN）与列类型不一致（MODIFY / ALTER COLUMN）；
	// 目标多出而源没有的列不参与（避免误删业务列）。只读目标库、不执行任何 DDL；
	// 返回空字符串表示结构一致。供整库同步前的结构预检：发现不一致时由调用方打印并退出，目标库保持原状。
	GenerateAlter(db *sql.DB, table string, srcCols []types.Column) (string, error)

	// RowCount 返回目标表当前行数（用于校验）。
	RowCount(db *sql.DB, table string) (int64, error)
	// PrimaryKey 查询目标表主键列（用于校验）。
	PrimaryKey(db *sql.DB, database, table string) []string

	// QuoteIdentifier 用本方言的引号包裹标识符。
	QuoteIdentifier(name string) string
	// Placeholder 返回第 n 个占位符。
	Placeholder(n int) string
}

// TableMetaReader 是 Reader 的可选扩展：源方言可提供表级属性
// （ENGINE / 默认字符集 / 排序规则 / 表注释），供目标方言在表尾还原。
// 未实现该接口的源方言不提供表级元数据（目标按默认值建表）。
type TableMetaReader interface {
	TableMeta(db *sql.DB, tableRef string) (types.TableMeta, error)
}

// TableMetaWriter 是 Writer 的可选扩展：目标方言可利用源表级属性生成带表尾选项的
// CREATE TABLE。未实现该接口时 syncer 回退调用 GenerateCreate，建表行为保持不变。
type TableMetaWriter interface {
	GenerateCreateWithMeta(db *sql.DB, table string, cols []types.Column, pk []string, meta types.TableMeta) (string, error)
}

var (
	mu      sync.RWMutex
	readers = map[string]func() Reader{}
	writers = map[string]func() Writer{}
)

// RegisterReader 注册一种方言的 Reader 工厂。应在 init() 中调用。
func RegisterReader(name string, factory func() Reader) {
	mu.Lock()
	defer mu.Unlock()
	readers[name] = factory
}

// RegisterWriter 注册一种方言的 Writer 工厂。应在 init() 中调用。
func RegisterWriter(name string, factory func() Writer) {
	mu.Lock()
	defer mu.Unlock()
	writers[name] = factory
}

// NewReader 按方言名创建 Reader 实例。
func NewReader(name string) (Reader, error) {
	mu.RLock()
	f, ok := readers[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("不支持的源方言: %s", name)
	}
	return f(), nil
}

// NewWriter 按方言名创建 Writer 实例。
func NewWriter(name string) (Writer, error) {
	mu.RLock()
	f, ok := writers[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("不支持的目标方言: %s", name)
	}
	return f(), nil
}
