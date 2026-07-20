package syncer

import (
	"database/sql"
	"fmt"
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

func (s *Syncer) filterTables(filter []string) []config.TableConfig {
	if len(filter) == 0 {
		return s.cfg.Tables
	}
	set := make(map[string]bool, len(filter))
	for _, f := range filter {
		set[strings.ToUpper(f)] = true
	}
	var out []config.TableConfig
	for _, t := range s.cfg.Tables {
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
