package syncer

import (
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	emaAlpha    = 0.3 // 指数移动平均平滑系数
	logInterval = 30 * time.Second
	percentStep = 10.0 // 进度每跨过 10% 输出一次
)

// progressTracker 追踪同步进度并计算 ETA。
// Add() 线程安全，供 worker goroutine 并发调用。
type progressTracker struct {
	total     int64
	startTime time.Time

	mu       sync.Mutex
	done     int64
	emaSpeed float64 // 指数移动平均速度 (rows/s)
	samples  int     // 速度采样次数（前 3 次用递增 alpha 抑制冷启动抖动）
	lastDone int64
	lastCalc time.Time
	lastLog  time.Time
	lastPct  float64
	hasTotal bool // total > 0 时才输出百分比/ETA
}

// newProgressTracker 创建进度追踪器。total <= 0 表示无总行数（不输出百分比/ETA）。
func newProgressTracker(total int64) *progressTracker {
	now := time.Now()
	return &progressTracker{
		total:     total,
		startTime: now,
		lastCalc:  now,
		lastLog:   now,
		hasTotal:  total > 0,
	}
}

// Add 累加已写入行数并刷新速度采样（线程安全）。
func (t *progressTracker) Add(n int64) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.done += n
	now := time.Now()
	elapsed := now.Sub(t.lastCalc).Seconds()

	// 至少间隔 0.5s 再采样，避免瞬时速度剧烈波动。
	if elapsed < 0.5 {
		return
	}
	if t.done <= t.lastDone {
		return
	}

	instant := float64(t.done-t.lastDone) / elapsed
	t.samples++

	alpha := emaAlpha
	if t.samples <= 3 {
		// 前 3 次用递增权重，避免第 1 批瞬时速度决定全部预估。
		alpha = 1.0 / float64(t.samples)
	}

	if t.emaSpeed == 0 {
		t.emaSpeed = instant
	} else {
		t.emaSpeed = alpha*instant + (1-alpha)*t.emaSpeed
	}
	t.lastDone = t.done
	t.lastCalc = now
}

// ShouldLog 距上次日志超过 logInterval 或进度跨过 percentStep 整数倍时返回 true。
func (t *progressTracker) ShouldLog() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if now.Sub(t.lastLog) >= logInterval {
		t.lastLog = now
		t.lastPct = t.percent()
		return true
	}
	if t.hasTotal {
		pct := t.percent()
		if pct-t.lastPct >= percentStep {
			t.lastLog = now
			t.lastPct = pct
			return true
		}
	}
	return false
}

func (t *progressTracker) percent() float64 {
	if !t.hasTotal || t.total <= 0 {
		return 0
	}
	p := float64(t.done) / float64(t.total) * 100
	// 钳制：防止浮点误差导致显示 100% 但实际未完成。
	if p > 99.9 && t.done < t.total {
		p = 99.9
	}
	return p
}

// Total 返回源端总行数（由 queryTotal 在同步开始时记录），供写入 state 后端作为进度分母。
func (t *progressTracker) Total() int64 { return t.total }

// Status 返回当前进度快照。
func (t *progressTracker) Status() (done, total int64, percent float64, eta time.Duration, speed float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	done = t.done
	total = t.total
	speed = t.emaSpeed
	percent = t.percent()

	if t.hasTotal && speed > 0 && done < total {
		eta = time.Duration(float64(total-done)/speed) * time.Second
	}
	return
}

// LogProgress 输出一行进度日志（含百分比、速度、ETA）。
func (t *progressTracker) LogProgress(log *logrus.Entry, target string) {
	done, total, pct, eta, speed := t.Status()

	if total > 0 {
		var etaStr string
		if eta > 0 {
			etaStr = fmt.Sprintf(" | 预计剩余 %s", eta.Truncate(time.Second))
		} else if speed == 0 {
			etaStr = " | 预计剩余 —（计算中...）"
		}
		log.Infof("  ... 已写入 %d/%d 行 (%.1f%%) (%s) | 速度 %.0f row/s%s",
			done, total, pct, target, speed, etaStr)
	} else {
		var speedStr string
		if speed > 0 {
			speedStr = fmt.Sprintf(" | 速度 %.0f row/s", speed)
		}
		log.Infof("  ... 已写入 %d 行 (%s)%s", done, target, speedStr)
	}
}

// Summary 同步完成后输出总结（总行数、耗时、平均速度）。
func (t *progressTracker) Summary(log *logrus.Entry, target string) {
	t.mu.Lock()
	done := t.done
	start := t.startTime
	total := t.total
	t.mu.Unlock()

	elapsed := time.Since(start).Truncate(time.Millisecond)
	avgSpeed := float64(0)
	if elapsed.Seconds() > 0 {
		avgSpeed = float64(done) / elapsed.Seconds()
	}

	if total > 0 {
		log.Infof("同步完成 %s: 共写入 %d/%d 行，耗时 %s，平均速度 %.0f row/s",
			target, done, total, elapsed, avgSpeed)
	} else {
		log.Infof("同步完成 %s: 共写入 %d 行，耗时 %s，平均速度 %.0f row/s",
			target, done, elapsed, avgSpeed)
	}
}
