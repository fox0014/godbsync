package state

// CheckpointRow 是断点表中一行的快照，用于 CLI 展示。
type CheckpointRow struct {
	TableName  string
	Anchor     string
	AnchorKind string
	UpdatedAt  string
}

// Backend 是「同步进度」状态的存储后端抽象。
// 两个实现：MySQL（进度存于目标库）与 SQLite（进度存于本地文件）。
// 通过 -state 选项切换，使水位表/断点表的物理位置可配置：
//   - 存于 MySQL 目标库：不污染外部、无需额外组件，但与数据同库；
//   - 存于本地 SQLite：不写入目标库、无需 DDL 权限，但失去与数据的事务原子性
//     （由「先提交数据、再写本地进度」+ 幂等写入兜底保证正确）。
type Backend interface {
	// EnsureWatermarkTable 创建（若不存在）增量水位表。
	EnsureWatermarkTable() error
	// EnsureCheckpointTable 创建（若不存在）全量断点表。
	EnsureCheckpointTable() error

	// GetWatermark 读取表的水位值；found=false 表示尚未同步过。
	GetWatermark(table string) (value string, found bool, err error)
	// SetWatermark 写入/更新表的水位值。
	SetWatermark(table, value string) error

	// GetCheckpoint 读取表的全量断点（已同步到的最大锚点值）。
	GetCheckpoint(table string) (anchor, kind, updatedAt string, found bool, err error)
	// SetCheckpoint 写入/更新表的全量断点。
	SetCheckpoint(table, anchor, kind string) error
	// ClearCheckpoint 删除表的断点记录。
	ClearCheckpoint(table string) error
	// ListCheckpoints 列出全部表的断点记录（按表名排序）。
	ListCheckpoints() ([]CheckpointRow, error)
	// ClearAllCheckpoints 删除全部表的断点记录。
	ClearAllCheckpoints() error

	// Close 释放后端持有资源（MySQL 后端由调用方管理连接，此处为空操作）。
	Close() error
}

// ParseSpec 解析 -state 选项：
//
//	"" 或 "mysql"        -> MySQL 后端（进度存于目标库）
//	"file"               -> 本地 SQLite，默认 ./godbsync_state.db
//	"file=/path/to.db"   -> 本地 SQLite，指定路径
//
// 无法识别时退回 mysql。
func ParseSpec(s string) (kind, path string) {
	switch {
	case s == "" || s == "mysql":
		return "mysql", ""
	case s == "file":
		return "sqlite", "godbsync_state.db"
	case len(s) > 5 && s[:5] == "file=":
		p := s[5:]
		if p == "" {
			p = "godbsync_state.db"
		}
		return "sqlite", p
	default:
		return "mysql", ""
	}
}
