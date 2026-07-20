package syncer

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type pipelineJob struct {
	seq    int64
	anchor interface{}
	rows   [][]interface{}
}

type pipelineResult struct {
	seq    int64
	anchor interface{}
	n      int64
	err    error
}

// runConcurrentPipeline 是同步引擎的并发心脏：一个读取协程把源端行分批推入 jobs 通道，
// workers 个写入协程并发调用 writer.BulkUpsert，结果经 results 通道汇总；一个汇总协程按
// 序号把「连续已提交批次」推进到有序窗口（wmWindow），并按周期或每 N 批持久化进度。
// reader 负责产生 pipelineJob（遇 stop 应尽早退出），persistAnchor 负责把窗口安全点落盘。
// 返回总写入行数与最终推进到的水位/锚点（无则 nil）。
func (s *Syncer) runConcurrentPipeline(
	target string,
	colNames []string,
	batchSize int,
	ignoreErrors bool,
	upsert bool,
	reader func(jobs chan<- pipelineJob, stop <-chan struct{}) error,
	persistAnchor func(wm interface{}) error,
	tracker *progressTracker,
) (int64, interface{}, error) {
	workers := s.cfg.Workers
	if workers < 1 {
		workers = 4
	}
	batchTimeout := s.cfg.ResolveBatchTimeout()

	jobs := make(chan pipelineJob, 16)
	results := make(chan pipelineResult, 16)
	stop := make(chan struct{})
	var stopOnce sync.Once

	var wg sync.WaitGroup
	var firstErr error
	var mu sync.Mutex

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := range jobs {
				select {
				case <-stop:
					continue
				default:
				}
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), batchTimeout)
				n, e := s.writer.BulkUpsert(ctx, s.targetDB, target, colNames, j.rows, upsert)
				cancel()
				elapsed := time.Since(start)
				if elapsed > 30*time.Second {
					s.log.Warnf("  ⚠ 单批写入耗时过长 worker=%d seq=%d rows=%d elapsed=%v (%s)",
						workerID, j.seq, len(j.rows), elapsed.Truncate(time.Millisecond), target)
				}
				if e != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = e
					}
					mu.Unlock()
					if !ignoreErrors {
						stopOnce.Do(func() { close(stop) })
					}
					results <- pipelineResult{seq: j.seq, anchor: j.anchor, n: 0, err: e}
					continue
				}
				results <- pipelineResult{seq: j.seq, anchor: j.anchor, n: n, err: nil}
			}
		}(i)
	}

	go func() {
		defer close(jobs)
		if err := reader(jobs, stop); err != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}
	}()

	var total atomic.Int64
	var resultsDone sync.WaitGroup
	resultsDone.Add(1)

	win := newWMWindow()
	var ckMu sync.Mutex
	var dirty bool
	var advancedTotal int64
	const ckBatchMod int64 = 50
	ckInterval := s.cfg.ResolveWatermarkCheckpointInterval()

	var doFlushMu sync.Mutex

	doFlush := func() {
		ckMu.Lock()
		if !dirty {
			ckMu.Unlock()
			return
		}
		wm, ok := win.safe()
		if !ok {
			ckMu.Unlock()
			return
		}
		dirty = false
		advancedTotal = 0
		ckMu.Unlock()

		doFlushMu.Lock()
		if e := persistAnchor(wm); e != nil {
			s.log.Warnf("周期持久化进度失败 %s: %v", target, e)
		}
		doFlushMu.Unlock()
	}

	go func() {
		defer resultsDone.Done()
		for r := range results {
			total.Add(r.n)
			tracker.Add(r.n)
			if tracker.ShouldLog() {
				tracker.LogProgress(s.log, target)
			}
			ckMu.Lock()
			adv := win.commit(r.seq, r.anchor, r.err != nil)
			dirty = true
			advancedTotal += adv
			need := adv > 0 && advancedTotal >= ckBatchMod
			ckMu.Unlock()
			if need {
				doFlush()
			}
		}
	}()

	ticker := time.NewTicker(ckInterval)
	var tickerDone sync.WaitGroup
	tickerDone.Add(1)
	go func() {
		defer tickerDone.Done()
		for {
			select {
			case <-ticker.C:
				doFlush()
			case <-stop:
				return
			}
		}
	}()

	probeTicker := time.NewTicker(30 * time.Second)
	var probeDone sync.WaitGroup
	probeDone.Add(1)
	var probeLastTotal int64
	go func() {
		defer probeDone.Done()
		for {
			select {
			case <-probeTicker.C:
				cur := total.Load()
				stalled := cur == probeLastTotal
				probeLastTotal = cur
				if tracker.ShouldLog() {
					tracker.LogProgress(s.log, target)
				} else if stalled && cur > 0 {
					s.log.Warnf("  ⚠ 进度停滞：已写入 %d 行 (%s) — 可能目标写入阻塞或源端扫描缓慢",
						cur, target)
				}
			case <-stop:
				return
			}
		}
	}()

	wg.Wait()
	close(results)
	resultsDone.Wait()

	stopOnce.Do(func() { close(stop) })

	probeTicker.Stop()
	ticker.Stop()
	probeDone.Wait()
	tickerDone.Wait()

	mu.Lock()
	ferr := firstErr
	mu.Unlock()
	if ferr != nil {
		return total.Load(), nil, ferr
	}
	doFlush()
	if wm, ok := win.safe(); ok {
		return total.Load(), wm, nil
	}
	return total.Load(), nil, nil
}
