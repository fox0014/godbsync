package cmd

import (
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"

	"godbsync/config"
	"godbsync/internal/state"
)

// TestCollectOverview_QuickMode 用 SQLite 进度后端校验 collectOverview 在 quick 模式下的行为：
//   - 有断点记录的表状态应为 SYNCED
//   - LastSync 应反映断点来源（非空、非 “—”）
//   - quick 模式不应包含实时行数（Source/Target 为 nil）
func TestCollectOverview_QuickMode(t *testing.T) {
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
	// 表名使用 TargetName() 解析结果（此处未设 Target，回退为 Name）。
	if err := st.SetCheckpoint("ORDERS", "1000", "pk"); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}

	cfg := &config.Config{
		Source: config.DBConfig{Dialect: "mysql", Host: "h1", Port: 3306, Database: "src"},
		Target: config.DBConfig{Dialect: "oracle", Host: "h2", Port: 1521, Service: "orcl"},
		Tables: []config.TableConfig{
			{Name: "ORDERS", Mode: "full"},
			{Name: "NEVER_SYNCED", Mode: "full"},
		},
	}
	log := logrus.New()

	ov := collectOverview(cfg, st, nil, nil, nil, nil, cfg.Tables, true, 2000, log)
	if len(ov.Tables) != 2 {
		t.Fatalf("期望 2 张表，实际 %d", len(ov.Tables))
	}

	byName := map[string]StatusRow{}
	for _, r := range ov.Tables {
		byName[r.Table] = r
	}

	orders, ok := byName["ORDERS"]
	if !ok {
		t.Fatal("缺少 ORDERS 表")
	}
	if orders.State != "SYNCED" {
		t.Errorf("quick 且有断点应 SYNCED，实际 %s", orders.State)
	}
	if orders.LastSync == "—" || orders.LastSync == "" {
		t.Errorf("LastSync 应反映断点，实际 %q", orders.LastSync)
	}
	if orders.Source != nil || orders.Target != nil || orders.Delta != nil {
		t.Errorf("quick 模式不应有实时行数")
	}
	if orders.Progress != nil {
		t.Errorf("quick 模式不应有进度")
	}

	never, ok := byName["NEVER_SYNCED"]
	if !ok {
		t.Fatal("缺少 NEVER_SYNCED 表")
	}
	if never.State != "PENDING" {
		t.Errorf("从未同步应为 PENDING，实际 %s", never.State)
	}

	// 汇总计数校验
	if ov.StateCounts["SYNCED"] != 1 || ov.StateCounts["PENDING"] != 1 {
		t.Errorf("状态计数不符：%v", ov.StateCounts)
	}
}
