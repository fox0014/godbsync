package syncer

import (
	"fmt"
	"strings"
	"time"

	"godbsync/config"
	"godbsync/internal/types"
)

// FullSync 全量同步一张表，支持断点续传。
// sourceNamespace 返回源端用于定位库/属主的命名空间。
// MySQL 的「库(database)」等价于 Oracle/SQL Server 的「属主(schema)」，故 MySQL 取
// s.cfg.Source.Database，其余方言取 s.cfg.Source.Schema（表级 schema 优先）。
func (s *Syncer) FullSync(t config.TableConfig, fresh bool) (*ValidateBoundary, error) {
	target := t.TargetName()
	srcRef := t.SourceRef(s.cfg.Source.Schema)
	if err := s.ensureTable(t); err != nil {
		return nil, err
	}
	if err := s.state.EnsureCheckpointTable(); err != nil {
		return nil, err
	}
	if err := s.state.EnsureWrittenRowsTable(); err != nil {
		return nil, err
	}

	cols, err := s.reader.TableColumns(s.sourceDB, srcRef)
	if err != nil {
		return nil, err
	}
	colNames := make([]string, len(cols))
	for i, c := range cols {
		colNames[i] = t.TargetColName(c.Name)
	}

	pk := s.reader.PrimaryKey(s.sourceDB, s.sourceNamespace(t), strings.ToUpper(t.Name))
	var (
		anchorName    string
		anchorIdx     int
		anchorIsRowID bool
		anchorable    bool
		anchorKind    string
	)
	if len(pk) == 1 {
		anchorName = pk[0]
		idx := -1
		for i, c := range cols {
			if strings.EqualFold(c.Name, anchorName) {
				idx = i
				break
			}
		}
		if idx >= 0 {
			anchorIdx = idx
			anchorIsRowID = false
			anchorable = true
			anchorKind = "pk"
		}
	}
	if !anchorable && s.reader.SupportsRowID() {
		anchorName = "ROWID"
		anchorIdx = len(cols)
		anchorIsRowID = true
		anchorable = true
		anchorKind = "rowid"
	}
	// 多列主键 / 无主键且无 ROWID（如 MySQL 源常见）：无法用单一锚点做有序断点续传，
	// 退化为无序全量同步（仍可正常读写，只是不支持断点续传与边界校验）。
	if !anchorable {
		anchorIdx = -1
	}

	// anchorSQL：真实锚点列须按源方言规则引用（QuoteIdentifier）；ROWID 伪列不是真实列，
	// 必须保持裸名（如 Oracle 加引号会变成普通标识符而报错），故仅对非 ROWID 场景加引号。
	anchorSQL := anchorName
	if !anchorIsRowID {
		anchorSQL = s.reader.QuoteIdentifier(anchorName)
	}

	var baseQuery string
	if anchorIsRowID {
		baseQuery = fmt.Sprintf("SELECT a.*, a.ROWID FROM %s a", srcRef)
	} else {
		baseQuery = fmt.Sprintf("SELECT * FROM %s", srcRef)
	}
	orderBy := ""
	if anchorable {
		orderBy = fmt.Sprintf(" ORDER BY %s ASC", anchorSQL)
	}
	ph1 := s.reader.Placeholder(1)

	if t.Truncate {
		query := baseQuery + orderBy
		var args []interface{}

		if !fresh {
			cp, cpKind, _, found, cerr := s.state.GetCheckpoint(target)
			if cerr != nil {
				return nil, cerr
			}
			if found && anchorable {
				var bound interface{} = cp
				if !anchorIsRowID && s.reader.IsTimeType(cols[anchorIdx].DBType) {
					if tm, terr := time.Parse(time.RFC3339Nano, cp); terr == nil {
						bound = tm
					}
				}
				s.log.Infof("目标表 %s 存在断点=%s（%s），跳过清空，从断点续传 %s > %s", target, cp, cpKind, anchorName, cp)
				query = baseQuery + fmt.Sprintf(" WHERE %s > %s", anchorSQL, ph1) + orderBy
				args = []interface{}{bound}
			} else {
				s.log.Infof("清空目标表 %s 并重置断点", target)
				if err := s.writer.Truncate(s.targetDB, target); err != nil {
					return nil, err
				}
				if err := s.state.ClearCheckpoint(target); err != nil {
					return nil, err
				}
			}
		} else {
			s.log.Infof("指定 --fresh，清空目标表 %s 并重置断点", target)
			if err := s.writer.Truncate(s.targetDB, target); err != nil {
				return nil, err
			}
			if err := s.state.ClearCheckpoint(target); err != nil {
				return nil, err
			}
		}

		var countWhere string
		if len(args) > 0 {
			countWhere = fmt.Sprintf("%s > %s", anchorSQL, ph1)
		}
		tracker := s.queryTotal(srcRef, countWhere, args)
		s.log.Infof("开始全量同步 %s -> %s", srcRef, target)
		_, finalAnchor, err := s.transferFullConcurrent(t, target, cols, query, args, anchorIdx, anchorIsRowID, anchorKind, tracker)
		if err != nil {
			return nil, err
		}
		tracker.Summary(s.log, target)
		var boundary *ValidateBoundary
		if !anchorIsRowID && finalAnchor != nil {
			boundary = &ValidateBoundary{Column: anchorName, Value: finalAnchor}
		}
		return boundary, nil
	}

	query := baseQuery + orderBy
	var args []interface{}
	if !fresh {
		cp, cpKind, _, found, cerr := s.state.GetCheckpoint(target)
		if cerr != nil {
			return nil, cerr
		}
		if found && anchorable {
			var bound interface{} = cp
			if !anchorIsRowID && s.reader.IsTimeType(cols[anchorIdx].DBType) {
				if tm, terr := time.Parse(time.RFC3339Nano, cp); terr == nil {
					bound = tm
				}
			}
			s.log.Infof("目标表 %s 存在断点=%s（%s），从断点续传 %s > %s", target, cp, cpKind, anchorName, cp)
			query = baseQuery + fmt.Sprintf(" WHERE %s > %s", anchorSQL, ph1) + orderBy
			args = []interface{}{bound}
		} else {
			if anchorable {
				s.log.Infof("目标表 %s 无断点，从头全量同步（按 %s 排序）", target, anchorName)
			} else {
				s.log.Infof("目标表 %s 无断点，从头全量同步（无序：表无单列主键且无 ROWID，不支持断点续传）", target)
			}
		}
	} else {
		s.log.Infof("目标表 %s 指定 --fresh，忽略断点从头全量同步", target)
		if err := s.state.ClearCheckpoint(target); err != nil {
			return nil, err
		}
	}

	var countWhere string
	if len(args) > 0 {
		countWhere = fmt.Sprintf("%s > %s", anchorSQL, ph1)
	}
	tracker := s.queryTotal(srcRef, countWhere, args)
	s.log.Infof("开始全量同步 %s -> %s", srcRef, target)
	_, finalAnchor, err := s.transferFull(t, target, cols, colNames, anchorIdx, anchorIsRowID, query, args, tracker)
	if err != nil {
		return nil, err
	}
	tracker.Summary(s.log, target)
	var boundary *ValidateBoundary
	if !anchorIsRowID && finalAnchor != nil {
		boundary = &ValidateBoundary{Column: anchorName, Value: finalAnchor}
	}
	return boundary, nil
}

func (s *Syncer) transferFull(t config.TableConfig, target string, cols []types.Column, colNames []string, anchorIdx int, anchorIsRowID bool, query string, args []interface{}, tracker *progressTracker) (int64, interface{}, error) {
	srcDB, closeSrc, err := s.sourceReadDB(t)
	if err != nil {
		return 0, nil, err
	}
	defer closeSrc()
	rows, err := srcDB.Query(query, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("查询源表 %s 失败: %w", t.Name, err)
	}
	defer rows.Close()

	scanCols := len(cols)
	if anchorIsRowID {
		scanCols = len(cols) + 1
	}
	batchSize := t.ResolveBatchSize(s.cfg.ResolveBatchSize())
	anchorKind := "pk"
	if anchorIsRowID {
		anchorKind = "rowid"
	}

	var (
		total               int64
		batch               = make([][]interface{}, 0, batchSize)
		batchAnchor         string
		lastAnchorVal       interface{}
		lastCommittedAnchor interface{}
		cpOK                = true
	)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, e := s.writer.BulkUpsert(nil, s.targetDB, target, colNames, batch, true); e != nil {
			if t.IgnoreErrors {
				s.log.Warnf("批量写入 %s 失败（忽略并继续）: %v", target, e)
				cpOK = false
			} else {
				return fmt.Errorf("批量写入 %s 失败: %w", target, e)
			}
		} else if batchAnchor != "" && cpOK {
			lastCommittedAnchor = lastAnchorVal
			if e := s.state.SetCheckpoint(target, batchAnchor, anchorKind); e != nil {
				return fmt.Errorf("写入断点失败: %w", e)
			}
		}
		n := int64(len(batch))
		total += n
		tracker.Add(n)
		if tracker.ShouldLog() {
			tracker.LogProgress(s.log, target)
		}
		batch = make([][]interface{}, 0, batchSize)
		batchAnchor = ""
		return nil
	}

	for rows.Next() {
		vals, e := s.reader.ScanRow(rows, scanCols)
		if e != nil {
			if ferr := flush(); ferr != nil {
				return total, lastCommittedAnchor, ferr
			}
			return total, lastCommittedAnchor, fmt.Errorf("扫描行失败: %w", e)
		}

		var anchorVal interface{}
		if anchorIdx >= 0 && anchorIdx < len(vals) {
			anchorVal = vals[anchorIdx]
		}
		lastAnchorVal = anchorVal
		anchorDBType := ""
		if !anchorIsRowID && anchorIdx >= 0 && anchorIdx < len(cols) {
			anchorDBType = cols[anchorIdx].DBType
		}
		anchorStr := s.anchorString(anchorVal, anchorDBType)

		var norm []interface{}
		if anchorIsRowID {
			norm = make([]interface{}, len(cols))
			for i, v := range vals[:len(cols)] {
				norm[i] = s.reader.Normalize(v, cols[i].DBType)
			}
		} else {
			norm = make([]interface{}, len(vals))
			for i, v := range vals {
				norm[i] = s.reader.Normalize(v, cols[i].DBType)
			}
		}

		batchAnchor = anchorStr
		batch = append(batch, norm)
		if len(batch) >= batchSize {
			if e := flush(); e != nil {
				return total, lastCommittedAnchor, e
			}
		}
	}
	if e := flush(); e != nil {
		return total, lastCommittedAnchor, e
	}
	if e := rows.Err(); e != nil {
		return total, lastCommittedAnchor, fmt.Errorf("扫描 %s 出错: %w", target, e)
	}
	return total, lastCommittedAnchor, nil
}

func (s *Syncer) transferFullConcurrent(t config.TableConfig, target string, cols []types.Column, query string, args []interface{}, anchorIdx int, anchorIsRowID bool, anchorKind string, tracker *progressTracker) (int64, interface{}, error) {
	colNames := make([]string, len(cols))
	for i, c := range cols {
		colNames[i] = t.TargetColName(c.Name)
	}
	batchSize := t.ResolveBatchSize(s.cfg.ResolveBatchSize())
	scanCols := len(cols)
	if anchorIsRowID {
		scanCols = len(cols) + 1
	}

	var lastRawValue interface{}

	srcDB, closeSrc, err := s.sourceReadDB(t)
	if err != nil {
		return 0, nil, err
	}
	defer closeSrc()
	reader := func(jobs chan<- pipelineJob, stop <-chan struct{}) error {
		rows, err := srcDB.Query(query, args...)
		if err != nil {
			return fmt.Errorf("查询源表 %s 失败: %w", t.Name, err)
		}
		defer rows.Close()

		var (
			seq         int64
			batch       = make([][]interface{}, 0, batchSize)
			batchAnchor string
		)
		for rows.Next() {
			select {
			case <-stop:
				return nil
			default:
			}
			vals, e := s.reader.ScanRow(rows, scanCols)
			if e != nil {
				return fmt.Errorf("扫描行失败: %w", e)
			}
			if anchorIdx >= 0 && anchorIdx < len(vals) {
				anchorDBType := ""
				if !anchorIsRowID && anchorIdx < len(cols) {
					anchorDBType = cols[anchorIdx].DBType
				}
				batchAnchor = s.anchorString(vals[anchorIdx], anchorDBType)
				lastRawValue = vals[anchorIdx]
			}
			var norm []interface{}
			if anchorIsRowID {
				norm = make([]interface{}, len(cols))
				for i, v := range vals[:len(cols)] {
					norm[i] = s.reader.Normalize(v, cols[i].DBType)
				}
			} else {
				norm = make([]interface{}, len(vals))
				for i, v := range vals {
					norm[i] = s.reader.Normalize(v, cols[i].DBType)
				}
			}
			batch = append(batch, norm)
			if len(batch) == batchSize {
				select {
				case jobs <- pipelineJob{seq: seq, anchor: batchAnchor, rows: batch}:
				case <-stop:
					return nil
				}
				seq++
				batch = make([][]interface{}, 0, batchSize)
			}
		}
		if len(batch) > 0 {
			select {
			case jobs <- pipelineJob{seq: seq, anchor: batchAnchor, rows: batch}:
			case <-stop:
				return nil
			}
		}
		_ = rows.Err()
		return nil
	}

	persist := func(wm interface{}) error {
		if anchorIdx < 0 {
			// 无锚点（无序全量，如 MySQL 源无单列主键）：不持久化检查点。
			return nil
		}
		anchorStr, ok := wm.(string)
		if !ok {
			return fmt.Errorf("断点持久化失败：锚点类型不匹配，期望 string 但得到 %T", wm)
		}
		return s.state.SetCheckpoint(target, anchorStr, anchorKind)
	}

	total, _, err := s.runConcurrentPipeline(target, colNames, batchSize, t.IgnoreErrors, false, reader, persist, tracker)
	if err != nil {
		return total, nil, err
	}
	return total, lastRawValue, nil
}
