package syncer

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"godbsync/config"
	"godbsync/internal/dialect"
	"godbsync/internal/state"
	"godbsync/internal/types"
)

// Syncer 持有源/目标库连接与配置，负责编排各类同步任务。
//
// 本包按职责拆分源文件，但同属 syncer 包、共享 Syncer 类型与方法：
//   - syncer.go      ：核心生命周期与辅助方法（构造、建表、命名空间、水位解析等）
//   - full.go        ：全量同步入口 FullSync 与顺序/并发全量传输
//   - incr.go        ：增量同步入口 IncrementalSync 与水位传输
//   - pipeline.go    ：并发写入引擎 runConcurrentPipeline（全量/增量共用）
//   - progress.go    ：进度追踪器 progressTracker
//   - wmwindow.go    ：有序水位/断点窗口 wmWindow
//   - validate.go    ：一致性校验 ValidateBoundary 与校验逻辑
//   - utils.go       ：表名/列名辅助
type Syncer struct {
	cfg      *config.Config
	sourceDB *sql.DB
	targetDB *sql.DB
	reader   dialect.Reader
	writer   dialect.Writer
	state    state.Backend
	log      *logrus.Entry

	// expandedTables 是通配符 * 展开后的真实表列表；expanded 标记是否已展开（幂等）。
	expanded       bool
	expandedTables []config.TableConfig
}

// New 创建 Syncer 并完成连接池配置。
func New(cfg *config.Config, sourceDB, targetDB *sql.DB, reader dialect.Reader, writer dialect.Writer, st state.Backend, log *logrus.Entry) *Syncer {
	workers := cfg.Workers
	if workers < 1 {
		workers = 4
	}
	sourceDB.SetMaxOpenConns(5)
	targetDB.SetMaxOpenConns(workers + 4)
	return &Syncer{cfg: cfg, sourceDB: sourceDB, targetDB: targetDB, reader: reader, writer: writer, state: st, log: log}
}

// InitSchema 为配置中的表（或全部，当 filter 为空时）在目标侧建表。
func (s *Syncer) InitSchema(filter []string) error {
	if err := s.state.EnsureWatermarkTable(); err != nil {
		return err
	}
	if err := s.state.EnsureCheckpointTable(); err != nil {
		return err
	}
	if err := s.state.EnsureWrittenRowsTable(); err != nil {
		return err
	}
	for _, t := range s.filterTables(filter) {
		if err := s.ensureTable(t); err != nil {
			return err
		}
	}
	return nil
}

// sourceNamespace 返回源端用于定位库/属主的命名空间。
// MySQL 的「库(database)」等价于 Oracle/SQL Server 的「属主(schema)」，故 MySQL 取
// s.cfg.Source.Database，其余方言取 s.cfg.Source.Schema（表级 schema 优先）。
func (s *Syncer) sourceNamespace(t config.TableConfig) string {
	if s.cfg.Source.Dialect == "mysql" {
		if t.Schema != "" {
			return t.Schema
		}
		return s.cfg.Source.Database
	}
	return t.SourceSchema(s.cfg.Source.Schema)
}

const prefetchFactor = 4
const maxPrefetch = 50000

// sourceReadDB 返回用于批量读取源端数据的连接。当源端为 Oracle 时，按 batch_size
// 推断 PREFETCH_ROWS 重建连接以加速数组抓取（go-ora 默认仅 25 行/次，是 Oracle→其他库
// 反向同步的主要瓶颈）；其他方言或重建失败时复用既有 s.sourceDB。返回的 cleanup 须在读取结束后调用。
func (s *Syncer) sourceReadDB(t config.TableConfig) (*sql.DB, func(), error) {
	if !strings.EqualFold(s.cfg.Source.Dialect, "oracle") {
		return s.sourceDB, func() {}, nil
	}
	batchSize := t.ResolveBatchSize(s.cfg.ResolveBatchSize())
	prefetch := batchSize * prefetchFactor
	if prefetch > maxPrefetch {
		prefetch = maxPrefetch
	}
	db, err := s.reader.Open(s.cfg.Source.DSNWithPrefetch(prefetch))
	if err != nil {
		return nil, nil, fmt.Errorf("打开源端读取连接失败: %w", err)
	}
	return db, func() { _ = db.Close() }, nil
}

func (s *Syncer) ensureTable(t config.TableConfig) error {
	target := t.TargetName()

	exists, err := s.writer.TableExists(s.targetDB, s.cfg.Target.Database, target)
	if err != nil {
		return fmt.Errorf("检查目标表 %s 是否存在失败: %w", target, err)
	}
	if exists {
		s.log.Infof("目标表 %s 已存在，跳过建表（源表 %s）", target, t.Name)
		return nil
	}

	srcRef := t.SourceRef(s.cfg.Source.Schema)
	cols, err := s.reader.TableColumns(s.sourceDB, srcRef)
	if err != nil {
		s.log.Errorf("目标表 %s 创建失败：获取源列信息失败: %v", target, err)
		return err
	}
	pk := s.reader.PrimaryKey(s.sourceDB, s.sourceNamespace(t), strings.ToUpper(t.Name))

	mappedCols := make([]types.Column, len(cols))
	for i, c := range cols {
		mappedCols[i] = c
		mappedCols[i].Name = t.TargetColName(c.Name)
	}
	mappedPK := make([]string, len(pk))
	for i, k := range pk {
		mappedPK[i] = t.TargetColName(k)
	}

	ddl, err := s.writer.GenerateCreate(s.targetDB, t.TargetName(), mappedCols, mappedPK)
	if err != nil {
		s.log.Errorf("目标表 %s 创建失败：生成 DDL 失败: %v", target, err)
		return err
	}
	if err := s.writer.ExecDDL(s.targetDB, ddl); err != nil {
		s.log.Errorf("目标表 %s 创建失败：执行 DDL 失败: %v", target, err)
		return err
	}
	s.log.Infof("目标表 %s 创建成功（源表 %s）", target, t.Name)
	return nil
}

func (s *Syncer) anchorString(v interface{}, dbType string) string {
	if v == nil {
		return ""
	}
	n := s.reader.Normalize(v, dbType)
	if tm, ok := n.(time.Time); ok {
		return tm.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%v", n)
}

// Tables 返回（按 filter 过滤后的）待同步表列表。
func (s *Syncer) Tables(filter []string) []config.TableConfig {
	return s.filterTables(filter)
}

// Expand 将配置中的通配符表（name == "*"）展开为源库真实表列表。
// 必须在源连接已建立后调用；幂等。展开后的真实表继承通配符条目的同步选项（mode/truncate/batch_size 等），
// 目标表名与源表同名，并清空表级 schema（整库级已通过 sourceNamespace 定位，避免重复限定）。
func (s *Syncer) Expand() error {
	if s.expanded {
		return nil
	}
	schema := s.sourceNamespace(config.TableConfig{})
	out := make([]config.TableConfig, 0, len(s.cfg.Tables))
	for _, t := range s.cfg.Tables {
		if t.Name != "*" {
			out = append(out, t)
			continue
		}
		names, err := s.reader.ListTables(s.sourceDB, schema)
		if err != nil {
			return fmt.Errorf("展开通配符 * 失败: %w", err)
		}
		s.log.Infof("通配符 * 在 %s 匹配到 %d 张表", schema, len(names))
		for _, name := range names {
			nt := t
			nt.Name = name
			nt.Target = name
			nt.Schema = ""
			out = append(out, nt)
		}
	}
	s.expandedTables = out
	s.expanded = true
	return nil
}

// PreSyncStructureCheck 整库同步（通配符 * 展开）前的结构预检。
// 仅当已展开（s.expanded，即配置含 name: "*"）时生效；对每张「已存在于目标库」的表，
// 读取源列并与目标表现有列比对，由 Writer.GenerateAlter 生成差异 DDL。
// 发现任意结构不一致即返回错误（错误信息内含每张表的完整变更 SQL），调用方据此退出；
// 此时尚未执行任何 truncate / 写入，目标库保持原状（即「还原」语义）。
// 目标表不存在的表跳过（将由后续建表流程新建，无需比对）。
func (s *Syncer) PreSyncStructureCheck(filter []string) error {
	if !s.expanded {
		return nil
	}
	var diffs []string
	for _, t := range s.filterTables(filter) {
		target := t.TargetName()
		exists, err := s.writer.TableExists(s.targetDB, s.cfg.Target.Database, target)
		if err != nil {
			return fmt.Errorf("结构预检：检查目标表 %s 是否存在失败: %w", target, err)
		}
		if !exists {
			continue // 目标表不存在，将新建，无需比对
		}
		srcRef := t.SourceRef(s.cfg.Source.Schema)
		cols, err := s.reader.TableColumns(s.sourceDB, srcRef)
		if err != nil {
			return fmt.Errorf("结构预检：读取源表 %s 列失败: %w", srcRef, err)
		}
		mapped := make([]types.Column, len(cols))
		for i, c := range cols {
			mapped[i] = c
			mapped[i].Name = t.TargetColName(c.Name)
		}
		alter, err := s.writer.GenerateAlter(s.targetDB, target, mapped)
		if err != nil {
			return fmt.Errorf("结构预检：生成表 %s 变更SQL失败: %w", target, err)
		}
		if alter != "" {
			diffs = append(diffs, fmt.Sprintf("-- 表 %s 结构不一致，需先执行以下 DDL 再重跑（目标库保持原状）：\n%s", target, alter))
		}
	}
	if len(diffs) > 0 {
		// 关键：不要用 logrus 的 msg 字段承载多行 DDL——logrus 文本格式器在消息含空格时会
		// 加引号并把真实换行转义成字面 "\n"（appendEscaped），导致 DDL 显示成一行、无法复制。
		// 因此改用原始多行输出（真实换行）并同时落盘 .sql 文件，保证可读、可直接执行。
		block := strings.Join(diffs, "\n\n")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "==================================================================")
		fmt.Fprintln(os.Stderr, "检测到以下表目标结构与源不一致，已生成变更SQL并终止同步（目标库未做任何改动）：")
		fmt.Fprintln(os.Stderr, "==================================================================")
		fmt.Fprintln(os.Stderr, block)
		sqlFile := "godbsync_alter.sql"
		if err := os.WriteFile(sqlFile, []byte(block+"\n"), 0644); err == nil {
			fmt.Fprintf(os.Stderr, "\n变更SQL已写入 %s（可直接执行后再重跑 full）\n", sqlFile)
		}
		return fmt.Errorf("检测到 %d 张表目标结构与源不一致，已生成变更SQL并终止同步（目标库未做任何改动，详见上方输出或 godbsync_alter.sql）", len(diffs))
	}
	return nil
}

func (s *Syncer) filterTables(filter []string) []config.TableConfig {
	base := s.cfg.Tables
	if s.expanded {
		base = s.expandedTables
	}
	if len(filter) == 0 {
		return base
	}
	set := make(map[string]bool, len(filter))
	for _, f := range filter {
		set[strings.ToUpper(f)] = true
	}
	var out []config.TableConfig
	for _, t := range base {
		if set[strings.ToUpper(t.Name)] || set[strings.ToUpper(t.TargetName())] {
			out = append(out, t)
		}
	}
	return out
}

func (s *Syncer) queryTotal(srcRef, where string, args []interface{}) *progressTracker {
	total, err := s.reader.RowCount(s.sourceDB, srcRef, where, args)
	if err != nil {
		s.log.Warnf("统计源表 %s 行数失败（不影响同步）: %v", srcRef, err)
		return newProgressTracker(0)
	}
	return newProgressTracker(total)
}

func findWatermarkColumn(cols []types.Column, name string) (types.Column, int, error) {
	want := strings.ToUpper(name)
	for i, c := range cols {
		if strings.ToUpper(c.Name) == want {
			return c, i, nil
		}
	}
	return types.Column{}, -1, fmt.Errorf("水位列 %s 不在表列中", name)
}

func formatWatermark(v interface{}) string {
	if tm, ok := v.(time.Time); ok {
		return tm.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%v", v)
}

func (s *Syncer) parseWatermark(wm string, col types.Column) (interface{}, error) {
	if s.reader.IsTimeType(col.DBType) {
		if tm, err := time.Parse(time.RFC3339Nano, wm); err == nil {
			return tm, nil
		}
		if tm, err := time.Parse(time.RFC3339, wm); err == nil {
			return tm, nil
		}
		return nil, fmt.Errorf("无法将水位 %q 解析为时间", wm)
	}
	if s.reader.IsNumericType(col.DBType) {
		if i, err := strconv.ParseInt(wm, 10, 64); err == nil {
			return i, nil
		}
		if f, err := strconv.ParseFloat(wm, 64); err == nil {
			return f, nil
		}
	}
	return wm, nil
}
