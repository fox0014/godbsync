package state

import (
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
)

const (
	watermarkTable   = "godbsync_watermark"
	checkpointTable  = "godbsync_checkpoint"
	writtenRowsTable = "godbsync_written_rows"
)

// mysqlBackend 把进度表建在 MySQL 目标库内。
type mysqlBackend struct {
	db *sql.DB
}

// NewMySQLBackend 以 MySQL 连接构造状态后端。
// 水位表/断点表将创建在 db 所指向的目标库。
func NewMySQLBackend(db *sql.DB) Backend {
	return &mysqlBackend{db: db}
}

func (b *mysqlBackend) EnsureWatermarkTable() error {
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		table_name VARCHAR(128) NOT NULL,
		watermark VARCHAR(64) NOT NULL,
		updated_at DATETIME NOT NULL,
		PRIMARY KEY (table_name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`, watermarkTable)
	if _, err := b.db.Exec(ddl); err != nil {
		return fmt.Errorf("创建水位表失败: %w", err)
	}
	return nil
}

func (b *mysqlBackend) GetWatermark(table string) (value string, found bool, err error) {
	q := fmt.Sprintf("SELECT watermark FROM %s WHERE table_name = ?", watermarkTable)
	var v string
	if e := b.db.QueryRow(q, table).Scan(&v); e != nil {
		if e == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("读取水位失败: %w", e)
	}
	return v, true, nil
}

func (b *mysqlBackend) SetWatermark(table, value string) error {
	q := fmt.Sprintf(`INSERT INTO %s (table_name, watermark, updated_at) VALUES (?, ?, NOW())
		ON DUPLICATE KEY UPDATE watermark = VALUES(watermark), updated_at = NOW()`, watermarkTable)
	if _, err := b.db.Exec(q, table, value); err != nil {
		return fmt.Errorf("写入水位失败: %w", err)
	}
	return nil
}

func (b *mysqlBackend) EnsureCheckpointTable() error {
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		table_name  VARCHAR(128) NOT NULL,
		anchor      VARCHAR(256) NOT NULL,
		anchor_kind VARCHAR(16) NOT NULL,
		updated_at  DATETIME NOT NULL,
		PRIMARY KEY (table_name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`, checkpointTable)
	if _, err := b.db.Exec(ddl); err != nil {
		return fmt.Errorf("创建断点表失败: %w", err)
	}
	return nil
}

func (b *mysqlBackend) GetCheckpoint(table string) (anchor, kind, updatedAt string, found bool, err error) {
	// updated_at 用 DATE_FORMAT 直接读出字符串，统一接口返回类型。
	q := fmt.Sprintf("SELECT anchor, anchor_kind, DATE_FORMAT(updated_at,'%%Y-%%m-%%d %%H:%%i:%%s') FROM %s WHERE table_name = ?", checkpointTable)
	var a, k, u string
	if e := b.db.QueryRow(q, table).Scan(&a, &k, &u); e != nil {
		if e == sql.ErrNoRows {
			return "", "", "", false, nil
		}
		return "", "", "", false, fmt.Errorf("读取断点失败: %w", e)
	}
	return a, k, u, true, nil
}

func (b *mysqlBackend) SetCheckpoint(table, anchor, kind string) error {
	q := fmt.Sprintf(`INSERT INTO %s (table_name, anchor, anchor_kind, updated_at)
		VALUES (?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE anchor = VALUES(anchor), anchor_kind = VALUES(anchor_kind), updated_at = NOW()`, checkpointTable)
	if _, err := b.db.Exec(q, table, anchor, kind); err != nil {
		return fmt.Errorf("写入断点失败: %w", err)
	}
	return nil
}

func (b *mysqlBackend) ClearCheckpoint(table string) error {
	q := fmt.Sprintf("DELETE FROM %s WHERE table_name = ?", checkpointTable)
	if _, err := b.db.Exec(q, table); err != nil {
		return fmt.Errorf("清除断点失败: %w", err)
	}
	return nil
}

func (b *mysqlBackend) ListCheckpoints() ([]CheckpointRow, error) {
	q := fmt.Sprintf("SELECT table_name, anchor, anchor_kind, DATE_FORMAT(updated_at,'%%Y-%%m-%%d %%H:%%i:%%s') FROM %s ORDER BY table_name ASC", checkpointTable)
	rows, err := b.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("列举断点失败: %w", err)
	}
	defer rows.Close()
	var out []CheckpointRow
	for rows.Next() {
		var r CheckpointRow
		if e := rows.Scan(&r.TableName, &r.Anchor, &r.AnchorKind, &r.UpdatedAt); e != nil {
			return nil, fmt.Errorf("读取断点行失败: %w", e)
		}
		out = append(out, r)
	}
	if e := rows.Err(); e != nil {
		return nil, fmt.Errorf("遍历断点失败: %w", e)
	}
	return out, nil
}

func (b *mysqlBackend) ClearAllCheckpoints() error {
	q := fmt.Sprintf("DELETE FROM %s", checkpointTable)
	if _, err := b.db.Exec(q); err != nil {
		return fmt.Errorf("清除全部断点失败: %w", err)
	}
	return nil
}

// ---- 已写入行数（供 monitor 实时进度展示） ----

func (b *mysqlBackend) EnsureWrittenRowsTable() error {
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		table_name   VARCHAR(128) NOT NULL PRIMARY KEY,
		source_total BIGINT       NOT NULL DEFAULT 0,
		written_rows BIGINT       NOT NULL DEFAULT 0,
		updated_at   DATETIME     NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`, writtenRowsTable)
	if _, err := b.db.Exec(ddl); err != nil {
		return fmt.Errorf("创建已写入行数表失败: %w", err)
	}
	// 迁移：旧表可能缺 source_total 列，补上。
	var cnt int
	chk := fmt.Sprintf(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = 'source_total'`)
	if err := b.db.QueryRow(chk, writtenRowsTable).Scan(&cnt); err == nil && cnt == 0 {
		alt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN source_total BIGINT NOT NULL DEFAULT 0", writtenRowsTable)
		if _, err := b.db.Exec(alt); err != nil {
			return fmt.Errorf("迁移 written_rows 表失败: %w", err)
		}
	}
	return nil
}

func (b *mysqlBackend) GetWrittenRows(table string) (int64, bool, error) {
	q := fmt.Sprintf("SELECT written_rows FROM %s WHERE table_name = ?", writtenRowsTable)
	var n int64
	if e := b.db.QueryRow(q, table).Scan(&n); e != nil {
		if e == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("读取已写入行数失败: %w", e)
	}
	return n, true, nil
}

func (b *mysqlBackend) GetSourceTotal(table string) (int64, bool, error) {
	q := fmt.Sprintf("SELECT source_total FROM %s WHERE table_name = ?", writtenRowsTable)
	var n int64
	if e := b.db.QueryRow(q, table).Scan(&n); e != nil {
		if e == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("读取源总行数失败: %w", e)
	}
	return n, true, nil
}

func (b *mysqlBackend) SetWrittenRows(table string, sourceTotal, written int64) error {
	q := fmt.Sprintf(`INSERT INTO %s (table_name, source_total, written_rows, updated_at) VALUES (?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE source_total = VALUES(source_total), written_rows = VALUES(written_rows), updated_at = NOW()`, writtenRowsTable)
	if _, err := b.db.Exec(q, table, sourceTotal, written); err != nil {
		return fmt.Errorf("写入已写入行数失败: %w", err)
	}
	return nil
}

func (b *mysqlBackend) ClearWrittenRows(table string) error {
	q := fmt.Sprintf("DELETE FROM %s WHERE table_name = ?", writtenRowsTable)
	if _, err := b.db.Exec(q, table); err != nil {
		return fmt.Errorf("清除已写入行数失败: %w", err)
	}
	return nil
}

func (b *mysqlBackend) Close() error { return nil }
