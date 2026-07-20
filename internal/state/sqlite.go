package state

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// sqliteBackend 把进度表存于本地 SQLite 文件（纯 Go，无 CGO）。
type sqliteBackend struct {
	db *sql.DB
}

// OpenSQLite 打开（或创建）本地 SQLite 状态库，并确保两张表存在。
// 由于是工具专属文件，建表在打开时完成，故 checkpoint 命令可不连任何数据库直接查看。
func OpenSQLite(path string) (Backend, error) {
	// 连接串附带 busy_timeout，避免并发写入立即返回 SQLITE_BUSY。
	// 单连接上限：SQLite 只支持单 writer，限制最大连接数为 1 可在 Go 层面
	// 串行化所有数据库操作，彻底消除 "database is locked"。
	dsn := path + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开本地状态库失败: %w", err)
	}
	db.SetMaxOpenConns(1)

	b := &sqliteBackend{db: db}
	if err := b.EnsureWatermarkTable(); err != nil {
		db.Close()
		return nil, err
	}
	if err := b.EnsureCheckpointTable(); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

func (b *sqliteBackend) EnsureWatermarkTable() error {
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		table_name TEXT PRIMARY KEY,
		watermark  TEXT NOT NULL,
		updated_at  TEXT NOT NULL
	)`, watermarkTable)
	if _, err := b.db.Exec(ddl); err != nil {
		return fmt.Errorf("创建水位表失败: %w", err)
	}
	return nil
}

func (b *sqliteBackend) GetWatermark(table string) (value string, found bool, err error) {
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

func (b *sqliteBackend) SetWatermark(table, value string) error {
	q := fmt.Sprintf(`INSERT INTO %s (table_name, watermark, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(table_name) DO UPDATE SET watermark = excluded.watermark, updated_at = CURRENT_TIMESTAMP`, watermarkTable)
	if _, err := b.db.Exec(q, table, value); err != nil {
		return fmt.Errorf("写入水位失败: %w", err)
	}
	return nil
}

func (b *sqliteBackend) EnsureCheckpointTable() error {
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		table_name  TEXT PRIMARY KEY,
		anchor      TEXT NOT NULL,
		anchor_kind TEXT NOT NULL,
		updated_at  TEXT NOT NULL
	)`, checkpointTable)
	if _, err := b.db.Exec(ddl); err != nil {
		return fmt.Errorf("创建断点表失败: %w", err)
	}
	return nil
}

func (b *sqliteBackend) GetCheckpoint(table string) (anchor, kind, updatedAt string, found bool, err error) {
	q := fmt.Sprintf("SELECT anchor, anchor_kind, updated_at FROM %s WHERE table_name = ?", checkpointTable)
	var a, k, u string
	if e := b.db.QueryRow(q, table).Scan(&a, &k, &u); e != nil {
		if e == sql.ErrNoRows {
			return "", "", "", false, nil
		}
		return "", "", "", false, fmt.Errorf("读取断点失败: %w", e)
	}
	return a, k, u, true, nil
}

func (b *sqliteBackend) SetCheckpoint(table, anchor, kind string) error {
	q := fmt.Sprintf(`INSERT INTO %s (table_name, anchor, anchor_kind, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(table_name) DO UPDATE SET anchor = excluded.anchor, anchor_kind = excluded.anchor_kind, updated_at = CURRENT_TIMESTAMP`, checkpointTable)
	if _, err := b.db.Exec(q, table, anchor, kind); err != nil {
		return fmt.Errorf("写入断点失败: %w", err)
	}
	return nil
}

func (b *sqliteBackend) ClearCheckpoint(table string) error {
	q := fmt.Sprintf("DELETE FROM %s WHERE table_name = ?", checkpointTable)
	if _, err := b.db.Exec(q, table); err != nil {
		return fmt.Errorf("清除断点失败: %w", err)
	}
	return nil
}

func (b *sqliteBackend) ListCheckpoints() ([]CheckpointRow, error) {
	q := fmt.Sprintf("SELECT table_name, anchor, anchor_kind, updated_at FROM %s ORDER BY table_name ASC", checkpointTable)
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

func (b *sqliteBackend) ClearAllCheckpoints() error {
	q := fmt.Sprintf("DELETE FROM %s", checkpointTable)
	if _, err := b.db.Exec(q); err != nil {
		return fmt.Errorf("清除全部断点失败: %w", err)
	}
	return nil
}

func (b *sqliteBackend) Close() error { return b.db.Close() }
