package syncer

import (
	"fmt"

	"godbsync/config"
	"godbsync/internal/types"
)

// IncrementalSync 增量同步一张表。
func (s *Syncer) IncrementalSync(t config.TableConfig) (interface{}, error) {
	target := t.TargetName()
	srcRef := t.SourceRef(s.cfg.Source.Schema)
	if err := s.ensureTable(t); err != nil {
		return nil, err
	}
	cols, err := s.reader.TableColumns(s.sourceDB, srcRef)
	if err != nil {
		return nil, err
	}
	wmCol, idx, err := findWatermarkColumn(cols, t.Watermark)
	if err != nil {
		return nil, err
	}

	prev, found, err := s.state.GetWatermark(target)
	if err != nil {
		return nil, err
	}
	ph1 := s.reader.Placeholder(1)
	var query string
	var args []interface{}
	if !found {
		s.log.Infof("目标表 %s 无历史水位，将全量拉取并按 %s 推进", target, t.Watermark)
		query = fmt.Sprintf("SELECT * FROM %s ORDER BY %s ASC", srcRef, s.reader.QuoteIdentifier(wmCol.Name))
	} else {
		s.log.Infof("目标表 %s 上次水位=%s，增量拉取 %s >= %s", target, prev, t.Watermark, prev)
		pv, perr := s.parseWatermark(prev, wmCol)
		if perr != nil {
			return nil, perr
		}
		// 用 >= 而非 > ：水位列可能非唯一（如按秒的时间戳），若仅取 > 上次水位，
		// 在检查点之后到达、但水位值恰好等于边界值的行会被漏同步。>= 会重新拉取边界行，
		// 由增量路径已有的幂等 upsert 去重，保证不丢数据。
		query = fmt.Sprintf("SELECT * FROM %s WHERE %s >= %s ORDER BY %s ASC", srcRef, s.reader.QuoteIdentifier(wmCol.Name), ph1, s.reader.QuoteIdentifier(wmCol.Name))
		args = []interface{}{pv}
	}

	var countWhere string
	if len(args) > 0 {
		countWhere = fmt.Sprintf("%s >= %s", s.reader.QuoteIdentifier(wmCol.Name), ph1)
	}
	tracker := s.queryTotal(srcRef, countWhere, args)

	s.log.Infof("开始增量同步 %s -> %s", srcRef, target)
	total, newWM, err := s.transferWithWatermark(t, target, cols, query, args, idx, tracker)
	if err != nil {
		return nil, err
	}
	if newWM != nil {
		if err := s.state.SetWatermark(target, formatWatermark(newWM)); err != nil {
			return nil, err
		}
		s.log.Infof("增量同步完成 %s: 写入 %d 行，新水位=%s", target, total, formatWatermark(newWM))
	} else {
		s.log.Infof("增量同步完成 %s: 无新增数据", target)
	}
	tracker.Summary(s.log, target)
	return newWM, nil
}

func (s *Syncer) transferWithWatermark(t config.TableConfig, target string, cols []types.Column, query string, args []interface{}, wmIdx int, tracker *progressTracker) (int64, interface{}, error) {
	colNames := make([]string, len(cols))
	for i, c := range cols {
		colNames[i] = t.TargetColName(c.Name)
	}
	batchSize := t.ResolveBatchSize(s.cfg.ResolveBatchSize())

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
			seq   int64
			batch = make([][]interface{}, 0, batchSize)
			bwm   interface{}
		)
		for rows.Next() {
			select {
			case <-stop:
				return nil
			default:
			}
			vals, e := s.reader.ScanRow(rows, len(cols))
			if e != nil {
				return fmt.Errorf("扫描行失败: %w", e)
			}
			norm := make([]interface{}, len(vals))
			for i, v := range vals {
				norm[i] = s.reader.Normalize(v, cols[i].DBType)
			}
			if wmIdx >= 0 && wmIdx < len(norm) {
				bwm = norm[wmIdx]
			}
			batch = append(batch, norm)
			if len(batch) == batchSize {
				select {
				case jobs <- pipelineJob{seq: seq, anchor: bwm, rows: batch}:
				case <-stop:
					return nil
				}
				seq++
				batch = make([][]interface{}, 0, batchSize)
			}
		}
		if len(batch) > 0 {
			select {
			case jobs <- pipelineJob{seq: seq, anchor: bwm, rows: batch}:
			case <-stop:
				return nil
			}
		}
		_ = rows.Err()
		return nil
	}

	persist := func(wm interface{}) error {
		return s.state.SetWatermark(target, formatWatermark(wm))
	}

	return s.runConcurrentPipeline(target, colNames, batchSize, t.IgnoreErrors, true, reader, persist, tracker)
}
