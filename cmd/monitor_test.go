package cmd

import (
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"

	"godbsync/config"
	"godbsync/internal/state"
)

// TestCollectOverview_BackendMode 用 SQLite 进度后端校验 collectOverview 在默认（后端驱动）模式下的行为：
//   - 有断点记录的表状态应为 SYNCED
//   - 有 written_rows + source_total 的表状态应为 SYNCING（进行中）或 OK（已完成）
//   - 后端模式不应包含实时目标行数（Target 为 nil）
func TestCollectOverview_BackendMode(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := state.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer st.Close()
	if err := st.EnsureCheckpointTable(); err != nil {
		t.Fatalf("ensure checkpoint table: %v", err)
	}
	if err := st.EnsureWrittenRowsTable(); err != nil {
		t.Fatalf("ensure written_rows table: %v", err)
	}
	if err := st.SetCheckpoint("ORDERS_DONE", "1000", "pk"); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}
	// 进行中：written(300) < source_total(1000)
	if err := st.SetWrittenRows("ORDERS_SYNCING", 1000, 300); err != nil {
		t.Fatalf("set written: %v", err)
	}
	// 已完成：written >= source_total
	if err := st.SetWrittenRows("ORDERS_OK", 1000, 1000); err != nil {
		t.Fatalf("set written: %v", err)
	}

	cfg := &config.Config{
		Source: config.DBConfig{Dialect: "mysql", Host: "h1", Port: 3306, Database: "src"},
		Target: config.DBConfig{Dialect: "oracle", Host: "h2", Port: 1521, Service: "orcl"},
		Tables: []config.TableConfig{
			{Name: "ORDERS_DONE", Mode: "full"},
			{Name: "ORDERS_SYNCING", Mode: "full"},
			{Name: "ORDERS_OK", Mode: "full"},
			{Name: "NEVER", Mode: "full"},
		},
	}
	log := logrus.New()

	// live=false（默认后端模式）：不查业务库。
	ov := collectOverview(cfg, st, nil, nil, nil, nil, cfg.Tables, false, 2000, 0, log)
	if len(ov.Tables) != 4 {
		t.Fatalf("期望 4 张表，实际 %d", len(ov.Tables))
	}

	byName := map[string]StatusRow{}
	for _, r := range ov.Tables {
		byName[r.Table] = r
	}

	if byName["ORDERS_DONE"].State != "SYNCED" {
		t.Errorf("有断点应 SYNCED，实际 %s", byName["ORDERS_DONE"].State)
	}
	if byName["ORDERS_SYNCING"].State != "SYNCING" {
		t.Errorf("written<source 应 SYNCING，实际 %s", byName["ORDERS_SYNCING"].State)
	}
	if byName["ORDERS_OK"].State != "OK" {
		t.Errorf("written>=source 应 OK，实际 %s", byName["ORDERS_OK"].State)
	}
	if byName["NEVER"].State != "PENDING" {
		t.Errorf("从未同步应 PENDING，实际 %s", byName["NEVER"].State)
	}
	// 后端模式不应有实时目标行数。
	if byName["ORDERS_SYNCING"].Target != nil {
		t.Errorf("后端模式不应有目标行数（依赖 COUNT(*)）")
	}
	// SYNCING 应有进度。
	if byName["ORDERS_SYNCING"].Progress == nil || *byName["ORDERS_SYNCING"].Progress != 30.0 {
		t.Errorf("ORDERS_SYNCING 进度应为 30.0，实际 %v", byName["ORDERS_SYNCING"].Progress)
	}
	if byName["ORDERS_SYNCING"].WrittenRows == nil || *byName["ORDERS_SYNCING"].WrittenRows != 300 {
		t.Errorf("ORDERS_SYNCING 已写入应为 300，实际 %v", byName["ORDERS_SYNCING"].WrittenRows)
	}
	if byName["ORDERS_SYNCING"].Source == nil || *byName["ORDERS_SYNCING"].Source != 1000 {
		t.Errorf("ORDERS_SYNCING 源行数应来自 source_total=1000，实际 %v", byName["ORDERS_SYNCING"].Source)
	}

	// 整体进度应为行数加权：(300+1000)/(1000+1000) = 65.0%。
	if ov.Overall.Progress == nil || *ov.Overall.Progress != 65.0 {
		t.Errorf("整体进度应为 65.0，实际 %v", ov.Overall.Progress)
	}
	// 有 SYNCING 表 → 整体状态「进行中」。
	if ov.Overall.State != "IN_PROGRESS" || ov.Overall.Label != "进行中" {
		t.Errorf("整体状态应为 进行中(IN_PROGRESS)，实际 %s/%s", ov.Overall.State, ov.Overall.Label)
	}
	if ov.Overall.TablesTotal != 4 {
		t.Errorf("整体表总数应为 4，实际 %d", ov.Overall.TablesTotal)
	}
	// 已完成表数 = OK(ORDERS_OK) + SYNCED(ORDERS_DONE) = 2。
	if ov.Overall.TablesDone != 2 {
		t.Errorf("整体已完成表数应为 2，实际 %d", ov.Overall.TablesDone)
	}
}
