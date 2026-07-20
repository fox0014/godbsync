package state

import (
	"path/filepath"
	"testing"
)

// TestSQLiteBackend 验证本地 SQLite 后端的水位/断点读写往返。
func TestSQLiteBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state_test.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite 失败: %v", err)
	}
	defer st.Close()

	// 水位：未设置 -> found=false
	if _, found, err := st.GetWatermark("T1"); err != nil || found {
		t.Fatalf("空水位应 found=false, got found=%v err=%v", found, err)
	}
	if err := st.SetWatermark("T1", "2026-07-11T12:00:00Z"); err != nil {
		t.Fatalf("SetWatermark 失败: %v", err)
	}
	v, found, err := st.GetWatermark("T1")
	if err != nil || !found || v != "2026-07-11T12:00:00Z" {
		t.Fatalf("GetWatermark 往返失败: v=%q found=%v err=%v", v, found, err)
	}

	// 断点：写入 -> 读取 -> 清除
	if err := st.SetCheckpoint("T1", "999", "pk"); err != nil {
		t.Fatalf("SetCheckpoint 失败: %v", err)
	}
	a, kind, _, found, err := st.GetCheckpoint("T1")
	if err != nil || !found || a != "999" || kind != "pk" {
		t.Fatalf("GetCheckpoint 往返失败: a=%q kind=%q found=%v err=%v", a, kind, found, err)
	}

	// 列表应含 T1
	rows, err := st.ListCheckpoints()
	if err != nil {
		t.Fatalf("ListCheckpoints 失败: %v", err)
	}
	if len(rows) != 1 || rows[0].TableName != "T1" {
		t.Fatalf("ListCheckpoints 应返回 1 行 T1, got %+v", rows)
	}

	// 清除单表
	if err := st.ClearCheckpoint("T1"); err != nil {
		t.Fatalf("ClearCheckpoint 失败: %v", err)
	}
	if _, _, _, found, _ = st.GetCheckpoint("T1"); found {
		t.Fatalf("清除后 GetCheckpoint 应 found=false")
	}

	// 多表 + 清除全部
	_ = st.SetCheckpoint("A", "1", "pk")
	_ = st.SetCheckpoint("B", "2", "rowid")
	rows, _ = st.ListCheckpoints()
	if len(rows) != 2 {
		t.Fatalf("清除单表后应剩 2 行, got %d", len(rows))
	}
	if err := st.ClearAllCheckpoints(); err != nil {
		t.Fatalf("ClearAllCheckpoints 失败: %v", err)
	}
	rows, _ = st.ListCheckpoints()
	if len(rows) != 0 {
		t.Fatalf("ClearAllCheckpoints 后应 0 行, got %d", len(rows))
	}
}
