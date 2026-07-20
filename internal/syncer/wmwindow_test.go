package syncer

import "testing"

// 顺序提交：水位应随每个批次推进，最终 safe 等于最后一个序号。
func TestWMWindowSequential(t *testing.T) {
	w := newWMWindow()
	for i := int64(0); i < 5; i++ {
		if adv := w.commit(i, i, false); adv != 1 {
			t.Fatalf("顺序提交 seq=%d 应推进 1，实际 %d", i, adv)
		}
	}
	if wm, ok := w.safe(); !ok || wm != int64(4) {
		t.Fatalf("期望 safe=4，实际 ok=%v wm=%v", ok, wm)
	}
}

// 乱序提交：seq=2 先到时暂存 pending，待 seq=1 补齐后连续吸收。
func TestWMWindowOutOfOrder(t *testing.T) {
	w := newWMWindow()
	if adv := w.commit(0, int64(0), false); adv != 1 {
		t.Fatalf("seq=0 应推进 1，实际 %d", adv)
	}
	if wm, _ := w.safe(); wm != int64(0) {
		t.Fatalf("仅 seq=0 提交，safe 应为 0，实际 %v", wm)
	}
	// seq=2 乱序到达，暂存，不推进
	if adv := w.commit(2, int64(2), false); adv != 0 {
		t.Fatalf("seq=2 乱序不应推进，实际 %d", adv)
	}
	if wm, _ := w.safe(); wm != int64(0) {
		t.Fatalf("seq=2 暂存期间 safe 仍应为 0，实际 %v", wm)
	}
	// seq=1 补齐，应吸收 1、2 连续推进到 2
	if adv := w.commit(1, int64(1), false); adv != 2 {
		t.Fatalf("seq=1 补齐应推进 2（1 与 2），实际 %d", adv)
	}
	if wm, ok := w.safe(); !ok || wm != int64(2) {
		t.Fatalf("补齐后 safe 应为 2，实际 ok=%v wm=%v", ok, wm)
	}
}

// 失败批次（含 IgnoreErrors 跳过）毒化窗口：其后已提交批次不再推进水位。
func TestWMWindowPoisoned(t *testing.T) {
	w := newWMWindow()
	w.commit(0, int64(0), false) // safe=0, nextSeq=1
	// seq=1 失败 → poisoned
	if adv := w.commit(1, int64(1), true); adv != 0 {
		t.Fatalf("失败批次不应推进，实际 %d", adv)
	}
	// 后续 seq=2、3 即使提交成功，safe 也应停在 0（不越过缺口）
	w.commit(2, int64(2), false)
	w.commit(3, int64(3), false)
	if wm, ok := w.safe(); ok {
		t.Fatalf("poisoned 后 safe 应不可用，却返回 wm=%v", wm)
	}
}

// 空窗口 safe() 不可用。
func TestWMWindowEmpty(t *testing.T) {
	w := newWMWindow()
	if _, ok := w.safe(); ok {
		t.Fatalf("空窗口 safe() 应返回 ok=false")
	}
}

// 多段乱序：验证窗口能正确处理多个缺口与补齐。
func TestWMWindowMultiGap(t *testing.T) {
	w := newWMWindow()
	w.commit(0, int64(0), false) // safe=0, nextSeq=1
	w.commit(2, int64(2), false) // 暂存
	w.commit(4, int64(4), false) // 暂存
	// 补齐 seq=1，应吸收 1、2（seq=3 尚未到，故不吸收 4）
	if adv := w.commit(1, int64(1), false); adv != 2 {
		t.Fatalf("补齐 seq=1 应吸收 1、2，实际 %d", adv)
	}
	if wm, _ := w.safe(); wm != int64(2) {
		t.Fatalf("safe 应为 2（seq=3 缺失），实际 %v", wm)
	}
	// 补齐 seq=3，吸收 3、4
	if adv := w.commit(3, int64(3), false); adv != 2 {
		t.Fatalf("补齐 seq=3 应吸收 3、4，实际 %d", adv)
	}
	if wm, ok := w.safe(); !ok || wm != int64(4) {
		t.Fatalf("补齐后 safe 应为 4，实际 ok=%v wm=%v", ok, wm)
	}
}
