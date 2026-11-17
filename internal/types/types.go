package types

// Column 描述源表的一列元数据，用于跨数据库类型映射与建表。
// DBType 由 Reader 填充为源数据库的原始类型名（如 Oracle 的 VARCHAR2、MySQL 的 varchar）。
type Column struct {
	Name      string // 列名
	DBType    string // 源数据库的类型名，如 VARCHAR2 / NUMBER / DATE / varchar / int
	Length    int64  // 字符/字节长度（CHAR、VARCHAR2 等）
	Precision int    // 数字精度（NUMBER(p,s) 的 p）
	Scale     int    // 数字小数位（NUMBER(p,s) 的 s）
	Nullable  bool   // 是否允许为空

	// 以下字段仅由 MySQL 源端（INFORMATION_SCHEMA.COLUMNS）填充，用于 MySQL→MySQL 建表保真；
	// 其他方言的源端保持零值，行为保持不变。
	//
	// RawType 是 COLUMN_TYPE 原文（如 varchar(32) / int unsigned / decimal(14,2) / enum('a','b')）。
	// go-sql-driver 不实现 ColumnType.Length()，按基础类型名重新映射会丢掉长度、unsigned、
	// enum/set 取值等信息（varchar(n) 会被降级成 TEXT），故 MySQL 目标端直接沿用该原文。
	RawType   string
	CharSet   string // CHARACTER_SET_NAME（文本列有值，其他类型为空）
	Collation string // COLLATION_NAME
	// HasDefault 为 false 表示源列没有默认值（COLUMN_DEFAULT 为 SQL NULL）。
	// Default 为默认值原文；DefaultIsExpr 表示 MySQL 8.0.13+ 的表达式默认值（EXTRA 含 DEFAULT_GENERATED）。
	HasDefault    bool
	Default       string
	DefaultIsExpr bool
	Comment       string // 列注释原文

	AutoIncrement            bool   // EXTRA 含 auto_increment
	AutoIncrementStart       int64  // 源表 TABLES.AUTO_INCREMENT（0 表示未知/未插过数据），仅自增列携带
	OnUpdateCurrentTimestamp bool   // EXTRA 含 on update CURRENT_TIMESTAMP
	GenerationExpression     string // GENERATION_EXPRESSION 原文（生成列才有值）
	GenerationType           string // 生成列类型：VIRTUAL / STORED
}

// TableMeta 描述源表的表级属性，用于 MySQL→MySQL 建表时还原表尾选项
// （ENGINE / DEFAULT CHARSET / COLLATE / COMMENT）。
// 仅 MySQL 源端（INFORMATION_SCHEMA.TABLES）填充；其他方言保持零值，建表行为不变。
type TableMeta struct {
	Engine    string // 存储引擎（如 InnoDB / MyISAM）
	Charset   string // 表默认字符集（由 TABLE_COLLATION 反查）
	Collation string // 表默认排序规则（TABLE_COLLATION）
	Comment   string // 表注释（TABLE_COMMENT，无注释时为空）
}
