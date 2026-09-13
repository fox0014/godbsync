package syncer

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"

	"godbsync/config"
)

// ValidateBoundary 描述校验时的边界过滤条件。
// 非 nil 时仅对源端做 WHERE col <= value 行数比对，避免源表持续写入导致的 COUNT(*) 误报。
type ValidateBoundary struct {
	Column string      // 源端用作边界的列名（锚点列或水位列）
	Value  interface{} // 边界值，已转换为源驱动能绑定的 Go 类型
}

// Validate 校验单表：先比对两侧行数，若表存在主键则进一步做按主键抽样的内容比对与校验和。
// 行数比对分两级：
//  1. 全表实时 COUNT：若一致则直接通过；
//  2. 不一致时若 boundary 非 nil，以 WHERE col <= value 过滤源端后再比，
//     排除同步期间源端持续写入导致的全表 COUNT 误报。
func (s *Syncer) Validate(t config.TableConfig, boundary *ValidateBoundary) error {
	target := t.TargetName()
	srcRef := t.SourceRef(s.cfg.Source.Schema)

	// 1) 行数比对（两级）
	var src int64
	dst, err := s.writer.RowCount(s.targetDB, target)
	if err != nil {
		return err
	}

	// 第一级：全表实时 COUNT
	srcFull, err := s.reader.RowCount(s.sourceDB, srcRef, "", nil)
	if err != nil {
		return fmt.Errorf("统计源表 %s 行数失败: %w", srcRef, err)
	}

	if srcFull == dst {
		s.log.Infof("[OK] 表 %s 全表行数一致: 源端=%d, 目标端=%d", target, srcFull, dst)
		src = srcFull
	} else if boundary != nil && boundary.Value != nil {
		// 第二级：全表不一致，用边界过滤再比（排除同步期间源端写入）
		s.log.Warnf("表 %s 全表行数不一致: 源端=%d, 目标端=%d，降级到边界过滤校验 (%s <= %v)",
			target, srcFull, dst, boundary.Column, boundary.Value)
		q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s <= %s",
			srcRef, s.reader.QuoteIdentifier(boundary.Column), s.reader.Placeholder(1))
		var srcBounded int64
		if err := s.sourceDB.QueryRow(q, boundary.Value).Scan(&srcBounded); err != nil {
			return fmt.Errorf("统计源表 %s (边界过滤) 行数失败: %w", srcRef, err)
		}
		if srcBounded == dst {
			s.log.Infof("[OK] 表 %s 边界过滤行数一致: 源端(<=%v)=%d, 目标端=%d（全表差异 %d 来自同步期间的源端写入）",
				target, boundary.Value, srcBounded, dst, srcFull-dst)
			src = dst // 内容抽样以目标库行数为准，避免源端全表行数干扰偏移量计算
		} else {
			s.log.Errorf("[MISMATCH] 表 %s 边界过滤后仍不一致: 源端(<=%v)=%d, 目标端=%d",
				target, boundary.Value, srcBounded, dst)
			s.troubleshootValidate(target, "boundary_filtered", srcFull, srcBounded, dst, 0)
			return fmt.Errorf("行数不一致: 源端(过滤后)=%d, 目标端=%d", srcBounded, dst)
		}
	} else {
		s.log.Errorf("[MISMATCH] 表 %s 全表行数不一致: 源端=%d, 目标端=%d（无边界可用）", target, srcFull, dst)
		s.troubleshootValidate(target, "full_table", srcFull, 0, dst, 0)
		return fmt.Errorf("行数不一致: 源端=%d, 目标端=%d", srcFull, dst)
	}

	// 2) 主键抽样内容比对（优先源端，若查不到则回退查目标表）
	pk := s.reader.PrimaryKey(s.sourceDB, t.SourceSchema(s.cfg.Source.Schema), strings.ToUpper(t.Name))
	if len(pk) == 0 {
		targetPK := s.writer.PrimaryKey(s.targetDB, s.cfg.Target.Database, target)
		if len(targetPK) > 0 {
			pk = make([]string, len(targetPK))
			for i, mk := range targetPK {
				pk[i] = t.SourceColName(mk)
			}
			s.log.Infof("表 %s 源端无主键约束，改用目标端主键: %v → 源端列名: %v", target, targetPK, pk)
		}
	}
	if len(pk) == 0 {
		s.log.Warnf("表 %s 两侧均未获取到主键，跳过内容抽样校验（仅完成行数校验）", target)
		return nil
	}
	sampleSize := s.cfg.ResolveValidateSampleSize()
	offset := int64(t.ResolveSampleOffset(s.cfg.ValidateSampleOffset))
	random := t.ResolveSampleRandom(s.cfg.ValidateSampleRandom)

	// 第一批：选定区间内逐列比对。
	mismatch, err := s.validateContent(t, srcRef, target, pk, sampleSize, src, offset, random, true)
	if err != nil {
		return err
	}
	if mismatch > 0 {
		s.troubleshootValidate(target, "content", srcFull, 0, dst, mismatch)
		return fmt.Errorf("内容不一致: 表 %s 有 %d 行抽样数据不匹配", target, mismatch)
	}

	// 第二批：另一区间仅计算校验和，扩大覆盖范围。
	offset2 := offset + int64(sampleSize)
	csMismatch, err := s.validateContent(t, srcRef, target, pk, sampleSize, src, offset2, random, false)
	if err != nil {
		// 校验和不一致不阻塞流程，仅记录日志。
		s.log.Warnf("表 %s 独立校验和抽查失败: %v", target, err)
	} else if csMismatch > 0 {
		s.log.Warnf("表 %s 批量校验和不一致（抽查区间 [%d, %d)，非逐列比对区间），建议增大 validate_sample_size 后重跑 validate 确认",
			target, offset2, offset2+int64(sampleSize))
	}

	s.log.Infof("[OK] 表 %s 主键抽样内容校验通过", target)
	return nil
}

// troubleshootValidate 在校验失败时输出排查指引，帮助快速定位问题。
// kind 取值: "full_table"（全表行数不一致）、"boundary_filtered"（边界过滤后仍不一致）、"content"（内容不一致）。
func (s *Syncer) troubleshootValidate(target, kind string, srcFull, srcBounded, dst int64, contentMismatch int) {
	s.log.Warn("========== 校验失败排查指引 ==========")
	switch kind {
	case "full_table":
		s.log.Warnf("问题: 表 %s 全表 COUNT 与目标库不一致 (源端=%d, 目标端=%d)", target, srcFull, dst)
		s.log.Warn("可能原因与排查:")
		if srcFull > dst {
			s.log.Warn("  1) 源端行数 > 目标端 → 同步可能中断或源端有新写入")
			s.log.Warn("     → 执行 checkpoint 查看断点状态，若不需续传可重跑 full -fresh")
		} else {
			s.log.Warn("  1) 源端行数 < 目标端 → 目标库有额外写入源")
			s.log.Warn("     → 检查是否有其他应用/进程也向该表写入数据")
		}
		s.log.Warn("  2) 本次校验未携带边界（validate 子命令或全量同步未启用 auto_validate）")
		s.log.Warn("     → 可在 full/incr 同步后立即校验，利用同步边界排除源端并发写入干扰")
	case "boundary_filtered":
		s.log.Warnf("问题: 表 %s 同步范围内行数不一致 (源端=%d, 目标端=%d，全表源端=%d)",
			target, srcBounded, dst, srcFull)
		s.log.Warn("可能原因与排查:")
		s.log.Warn("  1) 部分批次写入失败被忽略 → 检查日志中是否有 [WARNING] 批量写入失败（ignore_errors=true 会跳过）")
		s.log.Warn("  2) 目标端 max_allowed_packet 过小导致大行 INSERT 截断 → 调大该参数或减小 batch_size")
		s.log.Warn("  3) 断点推进异常导致跳行 → 确认断点列类型与排序方向是否匹配")
		s.log.Warn("  4) 源端在扫描期间有 DELETE → 考虑在低峰期重跑 full -fresh")
	case "content":
		s.log.Warnf("问题: 表 %s 行数一致但抽样内容有 %d 行不匹配", target, contentMismatch)
		s.log.Warn("可能原因与排查:")
		s.log.Warn("  1) 类型精度丢失 → 检查日志中打印的差异列，确认源端 NUMBER/DECIMAL 映射是否正确")
		s.log.Warn("  2) 字符集/编码差异 → 对比源端 (AL32UTF8) 与目标端 (utf8mb4) 的字符串列差异")
		s.log.Warn("  3) 时间格式/时区 → 确认源端 DATE/TIMESTAMP 列在目标端侧的值是否一致")
		s.log.Warn("  4) 增大 validate_sample_size 扩大抽样范围，确认是否为偶发或系统性问题")
	}
	s.log.Warn("=======================================")
}

// validateContent 按主键抽样内容校验：在 [offset, offset+size) 区间内抽样，
// random=true 时起点在表内随机选取。
// compare=true：逐列比对源端与目标端，并计算抽样内容校验和。
// compare=false：仅计算并比对两侧校验和，不做逐列比对。
// total 为源表行数（用于随机起点与越界修正）。返回不一致的行数。
func (s *Syncer) validateContent(t config.TableConfig, srcRef, target string, pk []string, sampleSize int, total, offset int64, random, compare bool) (int, error) {
	if sampleSize < 1 {
		sampleSize = 100
	}
	cols, err := s.reader.TableColumns(s.sourceDB, srcRef)
	if err != nil {
		return 0, err
	}
	colNames := make([]string, len(cols))
	targetColNames := make([]string, len(cols))
	for i, c := range cols {
		colNames[i] = c.Name
		targetColNames[i] = t.TargetColName(c.Name)
	}

	// 确定抽样区间 [offset, offset+size)
	size := int64(sampleSize)
	if random && total > size {
		offset = rand.Int63n(total - size) // 随机起点：0 .. total-size-1
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= total { // 起点越界，回退到末尾 size 行
		offset = total - size
		if offset < 0 {
			offset = 0
		}
	}
	end := offset + size
	if end > total {
		end = total
	}
	if end <= offset {
		s.log.Warnf("表 %s 抽样区间无效（offset=%d, size=%d, total=%d），跳过内容校验", target, offset, size, total)
		return 0, nil
	}

	// 抽样主键：按主键排序，取排序后第 offset+1 .. end 行。
	// 分页语法由源方言自行实现（MySQL/PostgreSQL 用 LIMIT/OFFSET、Oracle 用 ROWNUM、
	// SQL Server 用 ROW_NUMBER），syncer 不硬编码窗口函数，
	// 从而兼容 MySQL 5.7 等不支持 ROW_NUMBER() OVER 的老版本数据库。
	sampleQ := s.reader.SamplePKQuery(srcRef, pk, offset, end-offset)
	keys, err := s.samplePrimaryKeys(sampleQ)
	if err != nil {
		return 0, fmt.Errorf("抽样主键失败: %w", err)
	}
	if len(keys) == 0 {
		s.log.Warnf("表 %s 抽样主键为空，跳过内容校验", target)
		return 0, nil
	}

	// 构造 WHERE 子句（多列主键）：源/目标各自使用本方言的占位符与标识符引用。
	oWhere := make([]string, len(pk))
	mWhere := make([]string, len(pk))
	for i, k := range pk {
		oWhere[i] = fmt.Sprintf("%s = %s", s.reader.QuoteIdentifier(k), s.reader.Placeholder(i+1))
		mWhere[i] = fmt.Sprintf("%s = %s", s.writer.QuoteIdentifier(t.TargetColName(k)), s.writer.Placeholder(i+1))
	}
	oCols := make([]string, len(colNames))
	for i, c := range colNames {
		oCols[i] = s.reader.QuoteIdentifier(c)
	}
	mCols := make([]string, len(targetColNames))
	for i, c := range targetColNames {
		mCols[i] = s.writer.QuoteIdentifier(c)
	}
	oRowQ := fmt.Sprintf("SELECT %s FROM %s WHERE %s", strings.Join(oCols, ", "), srcRef, strings.Join(oWhere, " AND "))
	mRowQ := fmt.Sprintf("SELECT %s FROM %s WHERE %s",
		strings.Join(mCols, ", "), s.writer.QuoteIdentifier(target), strings.Join(mWhere, " AND "))

	var (
		mismatch   int
		srcSum     [32]byte
		dstSum     [32]byte
		maxLog     = 10
		loggedDiff int
	)
	for _, key := range keys {
		oVals, err := s.fetchRow(s.sourceDB, oRowQ, key)
		if err != nil {
			return 0, fmt.Errorf("读取源端抽样行失败: %w", err)
		}
		mVals, err := s.fetchRow(s.targetDB, mRowQ, key)
		if err != nil {
			return 0, fmt.Errorf("读取目标端抽样行失败: %w", err)
		}
		srcSum = xorSum(srcSum, rowChecksum(key, oVals))
		dstSum = xorSum(dstSum, rowChecksum(key, mVals))

		if !compare {
			// 仅校验和模式：跳过逐列比对。
			continue
		}

		if len(oVals) != len(mVals) {
			mismatch++
			if loggedDiff < maxLog {
				s.log.Errorf("表 %s 主键=%v 两侧列数不一致: 源端=%d, 目标端=%d", target, key, len(oVals), len(mVals))
				loggedDiff++
			}
			continue
		}
		rowDiff := false
		for i := range colNames {
			if !valuesEqual(oVals[i], mVals[i]) {
				rowDiff = true
				if loggedDiff < maxLog {
					s.log.Errorf("表 %s 主键=%v 列 `%s` 内容不一致: 源端=%v, 目标端=%v",
						target, key, colNames[i], oVals[i], mVals[i])
					loggedDiff++
				}
			}
		}
		if rowDiff {
			mismatch++
		}
	}

	label := "批量校验和"
	if compare {
		label = "抽样内容校验和"
	}
	s.log.Infof("表 %s %s: 源端=%s, 目标端=%s",
		target, label, hex.EncodeToString(srcSum[:]), hex.EncodeToString(dstSum[:]))

	if srcSum != dstSum {
		mismatch++
	}
	return mismatch, nil
}

// samplePrimaryKeys 执行抽样查询，返回每行主键组合（[][]interface{}）。
func (s *Syncer) samplePrimaryKeys(q string) ([][]interface{}, error) {
	rows, err := s.sourceDB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]interface{}
	for rows.Next() {
		vals, e := s.reader.ScanRow(rows, len(cols))
		if e != nil {
			return nil, e
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// fetchRow 按主键 key 查询单行，返回归一化前的原始扫描值。
func (s *Syncer) fetchRow(db *sql.DB, q string, key []interface{}) ([]interface{}, error) {
	rows, err := db.Query(q, key...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if e := rows.Err(); e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("未找到匹配行")
	}
	cols, _ := rows.Columns()
	return s.reader.ScanRow(rows, len(cols))
}
