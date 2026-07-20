package syncer

// wmWindow 维护「连续已提交批次」的水位窗口。
//
// 增量同步并发写入时，读取端按水位列升序产生批次（序号 0,1,2,...），但写入由多个
// worker 并发完成，提交顺序与读取顺序不一定一致。为避免水位越过尚未提交（或失败被
// 忽略）的批次而导致下次重跑漏数据，水位只能推进到「序号连续已成功提交」的最高边界。
//
// 关键性质：
//   - commit(seq, wm, failed)：报告某序号批次的提交结果。
//     只有 seq 恰为 nextSeq（下一段连续边界）时才推进 safeWM，并把 pending 中连续
//     的后继一并吸收；否则暂存 pending。
//   - 任一失败批次（failed=true，含 IgnoreErrors 跳过）将 poisoned 置真，此后窗口
//     永久停在最后一个连续成功边界，不再推进——即使其后的批次已成功提交（它们已写入
//     目标库，重跑时靠幂等 upsert 覆盖，不会重复、不会丢失）。
//   - safe() 返回当前可安全持久化的水位与是否已有值；poisoned 时返回 (_, false)。
//
// 该结构不依赖数据库，可独立单测。
type wmWindow struct {
	nextSeq  int64
	safeWM   interface{}
	haveSafe bool
	pending  map[int64]interface{}
	poisoned bool
}

// newWMWindow 创建空窗口。
func newWMWindow() *wmWindow {
	return &wmWindow{pending: make(map[int64]interface{})}
}

// commit 报告序号为 seq 的批次已提交（failed=false）或失败（failed=true）。
// 返回本次调用向前推进的连续批次数（用于触发周期持久化）。
func (w *wmWindow) commit(seq int64, wm interface{}, failed bool) int64 {
	if failed {
		w.poisoned = true
		return 0
	}
	if w.poisoned {
		// 已被毒化：窗口冻结，后续提交不再推进（已写库，重跑幂等）。
		return 0
	}
	if seq == w.nextSeq {
		w.safeWM = wm
		w.haveSafe = true
		w.nextSeq++
		advanced := int64(1)
		// 吸收紧跟其后的连续已提交批次。
		for {
			pw, ok := w.pending[w.nextSeq]
			if !ok {
				break
			}
			w.safeWM = pw
			delete(w.pending, w.nextSeq)
			w.nextSeq++
			advanced++
		}
		return advanced
	}
	// 非连续：暂存，待前面的批次补齐后再吸收。
	w.pending[seq] = wm
	return 0
}

// safe 返回当前可安全持久化的水位。poisoned 或尚无提交时 ok=false。
func (w *wmWindow) safe() (interface{}, bool) {
	if w.poisoned || !w.haveSafe {
		return nil, false
	}
	return w.safeWM, true
}
