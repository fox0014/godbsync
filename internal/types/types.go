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
}
