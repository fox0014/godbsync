package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"godbsync/config"
	"godbsync/internal/dialect"
	"godbsync/internal/state"
)

// errQuickSkip 标记 quick 模式下未统计行数，用于让 SOURCE/TARGET/DELTA 列显示 “—”。
var errQuickSkip = errors.New("quick mode: row counts skipped")

// monitorQueryTimeout 是 monitor 实时行数（COUNT(*)）查询的超时上限。
// 全量同步进行中，目标库（尤其 SQL Server）在活跃批量写入下 COUNT(*) 可能极慢甚至挂起，
// 不加超时会导致 /api/status 整个响应阻塞、前端永远拿不到数据（页面空白）。
// 超时即视为查询失败，对应列显示 “—”，但不影响 written_rows 进度展示。
const monitorQueryTimeout = 3 * time.Second

// rowCountWithTimeout 在超时保护下执行行数统计，避免慢查询/挂起阻塞调用方。
// 超时返回显式错误（不返回部分结果），由调用方决定如何降级展示。
func rowCountWithTimeout(fn func() (int64, error), timeout time.Duration) (int64, error) {
	type rc struct {
		n   int64
		err error
	}
	ch := make(chan rc, 1)
	go func() {
		n, err := fn()
		ch <- rc{n, err}
	}()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-time.After(timeout):
		return 0, fmt.Errorf("行数统计超时（%s），已降级展示", timeout)
	}
}

// 状态着色的 ANSI 转义码（尊重 NO_COLOR 环境变量）。
const (
	cGreen  = "\033[32m"
	cRed    = "\033[31m"
	cYellow = "\033[33m"
	cReset  = "\033[0m"
)

// statusColorize 在 s 外层包裹颜色码；当 NO_COLOR 非空时原样返回（不破坏管道/重定向）。
func statusColorize(s, code string) string {
	if os.Getenv("NO_COLOR") != "" {
		return s
	}
	return code + s + cReset
}

// 表格列宽（CHAR 计，不含颜色转义）。
const (
	wTable = 22
	wMode  = 12
	wState = 9
	wNum   = 12
	wProg  = 9
)

// runStatus 渲染同步状态表。live=true 时额外执行实时 COUNT(*) 做一致性校验；
// watch>0 时按该间隔循环刷新（不清屏，每次重打印并带刷新时间）。
// filter 非空时仅展示匹配（按 Name 或 TargetName，大小写不敏感）的表。
func runStatus(cfg *config.Config, st state.Backend, odb, mdb *sql.DB, live bool, watch time.Duration, scroll bool, log *logrus.Logger, filter []string) int {
	var reader dialect.Reader
	var writer dialect.Writer
	if live {
		if r, err := dialect.NewReader(cfg.Source.Dialect); err == nil {
			reader = r
		}
		if w, err := dialect.NewWriter(cfg.Target.Dialect); err == nil {
			writer = w
		}
	}

	tables := filterTables(cfg.Tables, filter)
	if len(tables) == 0 {
		fmt.Println("没有匹配的表（检查 -table 参数）")
		return 0
	}

	// 注：状态后端由 openStateBackend 打开时已确保断点/水位两张表存在，
	// status 仅做只读展示，不主动建表、不写进度。
	// scroll=true（默认）：watch 模式每次刷新向下追加（翻滚）；
	// scroll=false：原地覆盖——清屏重绘（仅当 stdout 为终端时生效，管道输出自动回退为翻滚）。
	overwrite := !scroll && isTerminal(os.Stdout)
	first := true
	for {
		if !first && overwrite {
			// 清屏并回到左上角：擦掉上一帧，原地重绘。
			fmt.Print("\033[2J\033[H")
		}
		fmt.Printf("godbsync 同步状态 · %s · %s(%s:%d) → %s(%s:%d)\n",
			time.Now().Format("2006-01-02 15:04:05"),
			cfg.Source.Dialect, cfg.Source.Host, cfg.Source.Port,
			cfg.Target.Dialect, cfg.Target.Host, cfg.Target.Port)
		renderStatusHeader()
		for _, t := range tables {
			renderStatusRow(cfg, st, odb, mdb, reader, writer, t, live, log)
		}
		if watch <= 0 {
			break
		}
		if overwrite {
			// 原地覆盖模式：不额外留空行，下一帧会整体清屏。
			fmt.Printf("（每 %s 刷新，原地覆盖，Ctrl+C 退出）\n", watch)
		} else {
			fmt.Printf("（每 %s 刷新，Ctrl+C 退出）\n\n", watch)
		}
		first = false
		time.Sleep(watch)
	}
	return 0
}

// isTerminal 判断 w 是否为字符设备（终端）。非终端（管道/重定向）时状态刷新
// 应平铺追加而非清屏，否则会向文件写入 ANSI 转义序列。
func isTerminal(w *os.File) bool {
	fi, err := w.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// filterTables 按 -table 过滤（匹配 Name 或 TargetName，大小写不敏感）。filter 为空返回全部。
func filterTables(tables []config.TableConfig, filter []string) []config.TableConfig {
	if len(filter) == 0 {
		return tables
	}
	set := make(map[string]bool, len(filter))
	for _, f := range filter {
		if f = strings.TrimSpace(f); f != "" {
			set[strings.ToUpper(f)] = true
		}
	}
	out := make([]config.TableConfig, 0, len(tables))
	for _, t := range tables {
		if set[strings.ToUpper(t.Name)] || set[strings.ToUpper(t.TargetName())] {
			out = append(out, t)
		}
	}
	return out
}

func renderStatusHeader() {
	line := pad("TABLE", wTable) +
		pad("MODE", wMode) +
		pad("STATE", wState) + " " +
		rpad("SOURCE", wNum) +
		rpad("TARGET", wNum) +
		rpad("DELTA", wNum) +
		rpad("PROGRESS", wProg) +
		"LAST SYNC"
	fmt.Println(line)
	// 分隔线长度与实际表头一致：各定宽列 + 列间 1 空格 + LAST SYNC 列宽。
	sepLen := wTable + wMode + wState + 1 + (wNum+1)*3 + (wProg + 1) + len("LAST SYNC")
	fmt.Println(strings.Repeat("-", sepLen))
}

// StatusRow 是一张表的同步状态快照，供 status 文本渲染与 monitor Web 仪表盘共用。
// 指针字段（Source/Target/Delta）为 nil 表示「未统计」（对应文本界面的 "—"）；
// Progress 为 nil 表示进度未知（对应文本界面的 "—"）；
// WrittenRows 为 nil 表示 state 后端无已写入行数记录（同步未开始或已结束）。
type StatusRow struct {
	Table       string   `json:"table"`
	Mode        string   `json:"mode"`
	State       string   `json:"state"` // OK/DIVERGED/PENDING/SYNCED/ERR
	Source      *int64   `json:"source,omitempty"`
	Target      *int64   `json:"target,omitempty"`
	Delta       *int64   `json:"delta,omitempty"`
	Progress    *float64 `json:"progress,omitempty"`     // 0..100，nil 表示未知
	WrittenRows *int64   `json:"written_rows,omitempty"` // syncer 上报的已写入行数
	LastSync    string   `json:"last_sync"`
}

// computeStatusRow 计算单表状态快照。
// 默认（live=false）纯后端驱动：源行数来自 state 后端的 source_total，已写入来自 written_rows，
// 进度 = written_rows / source_total，不查询任何业务库（monitor 默认，避免对大表反复 COUNT(*)）。
// live=true 时额外执行实时 COUNT(*) 做一致性校验（status --verify / monitor ?verify=1），
// 填充目标行数/差值并据真实计数判定 OK/DIVERGED/ERR。
// timeout>0 时对实时行数（COUNT(*)）查询施加超时保护，避免慢查询/挂起阻塞调用方；
// timeout==0 表示不限制（status CLI 一次性检查可用，monitor 应传 monitorQueryTimeout）。
func computeStatusRow(cfg *config.Config, st state.Backend, odb, mdb *sql.DB,
	reader dialect.Reader, writer dialect.Writer, t config.TableConfig, live bool, timeout time.Duration, log *logrus.Logger) StatusRow {

	target := t.TargetName()
	mode := t.ResolveMode()

	row := StatusRow{Table: t.Name, Mode: mode, LastSync: "—"}

	// —— 最后同步位置 ——
	found := false
	if mode == "incremental" {
		if wm, wf, werr := st.GetWatermark(target); werr != nil {
			log.Warnf("读取表 %s 水位失败: %v", target, werr)
		} else if wf {
			found = true
			row.LastSync = "wm=" + wm
		}
	}
	if !found {
		if _, _, updatedAt, cpf, cerr := st.GetCheckpoint(target); cerr != nil {
			log.Warnf("读取表 %s 断点失败: %v", target, cerr)
		} else if cpf {
			found = true
			if updatedAt != "" {
				row.LastSync = updatedAt
			}
		}
	}

	// —— 后端派生数据（monitor 默认数据源，零业务库查询）——
	var srcTotal, written int64
	var srcTotalFound, writtenFound bool
	if w, wf, werr := st.GetWrittenRows(target); werr != nil {
		log.Warnf("读取表 %s 已写入行数失败: %v", target, werr)
	} else if wf {
		written = w
		writtenFound = true
	}
	if st, stf, serr := st.GetSourceTotal(target); serr != nil {
		log.Warnf("读取表 %s 源总行数失败: %v", target, serr)
	} else if stf {
		srcTotal = st
		srcTotalFound = true
	}

	// —— 实时行数（仅 live 模式做一致性校验，默认跳过）——
	var srcCnt, tgtCnt int64
	var srcErr, tgtErr error
	if !live {
		srcErr, tgtErr = errQuickSkip, errQuickSkip
	} else {
		if odb != nil && reader != nil {
			if timeout > 0 {
				srcCnt, srcErr = rowCountWithTimeout(func() (int64, error) {
					return reader.RowCount(odb, t.SourceRef(cfg.Source.Schema), "", nil)
				}, timeout)
			} else {
				srcCnt, srcErr = reader.RowCount(odb, t.SourceRef(cfg.Source.Schema), "", nil)
			}
		} else if odb != nil {
			// 防御：连接已建但 Reader 不可用（理论上不会发生，openSourceDB 已校验方言）。
			srcErr = fmt.Errorf("源端 Reader 不可用（方言 %s 未注册）", cfg.Source.Dialect)
		}
		if mdb != nil && writer != nil {
			if timeout > 0 {
				tgtCnt, tgtErr = rowCountWithTimeout(func() (int64, error) {
					return writer.RowCount(mdb, target)
				}, timeout)
			} else {
				tgtCnt, tgtErr = writer.RowCount(mdb, target)
			}
		} else if mdb != nil {
			tgtErr = fmt.Errorf("目标端 Writer 不可用（方言 %s 未注册）", cfg.Target.Dialect)
		}
	}

	// —— 状态判定 ——
	stateStr := "PENDING"
	if live {
		// verify 模式：以真实计数一致性为准。
		if srcErr == nil && tgtErr == nil {
			if srcCnt == tgtCnt {
				stateStr = "OK"
			} else {
				stateStr = "DIVERGED"
			}
		} else {
			stateStr = "ERR"
			if srcErr != nil && srcErr != errQuickSkip {
				log.Warnf("表 %s 源行数统计失败: %v", t.Name, srcErr)
			}
			if tgtErr != nil && tgtErr != errQuickSkip {
				log.Warnf("表 %s 目标行数统计失败: %v", t.Name, tgtErr)
			}
		}
	} else {
		// 后端模式：依据 written_rows 与 source_total 判定，不查业务库。
		if writtenFound && srcTotalFound && srcTotal > 0 {
			if written >= srcTotal {
				stateStr = "OK"
			} else {
				stateStr = "SYNCING" // 同步进行中：written < source_total
			}
		} else if writtenFound {
			// 有写入记录但无源总数（理论上不会发生，source_total 与 written 同时写入）。
			stateStr = "SYNCING"
		} else if found {
			// 无写入记录但曾同步过（断点/水位存在）→ 视为已同步完成。
			stateStr = "SYNCED"
		}
	}
	row.State = stateStr

	// —— 行数 ——
	// 源行数：后端 source_total 优先（仅当 >0，避免旧数据迁移出的 0 误导）；verify 模式退化为实时 COUNT 结果。
	if srcTotalFound && srcTotal > 0 {
		row.Source = &srcTotal
	} else if srcErr == nil {
		row.Source = &srcCnt
	}
	// 目标行数与差值：仅 verify 模式有真实值；后端模式不查目标库，留空（—）。
	if tgtErr == nil {
		row.Target = &tgtCnt
	}
	if srcErr == nil && tgtErr == nil {
		d := srcCnt - tgtCnt
		row.Delta = &d
	}

	// —— 进度百分比 ——
	// 分母优先用后端 source_total，回退实时 srcCnt（仅 verify 模式有）。
	var denom int64
	if srcTotalFound && srcTotal > 0 {
		denom = srcTotal
	} else if srcErr == nil && srcCnt > 0 {
		denom = srcCnt
	} else if srcErr == nil && srcCnt == 0 {
		denom = 0 // 源 0 行
	}
	if writtenFound {
		row.WrittenRows = &written
		if denom > 0 {
			p := float64(written) / float64(denom) * 100
			row.Progress = &p
		} else if denom == 0 && written == 0 {
			// 源 0 行且未写入 → 100% 完成。
			p := 100.0
			row.Progress = &p
		}
	}

	return row
}

func renderStatusRow(cfg *config.Config, st state.Backend, odb, mdb *sql.DB,
	reader dialect.Reader, writer dialect.Writer, t config.TableConfig, live bool, log *logrus.Logger) {

	// status CLI 一次性检查，允许较长等待（30s），但不无限阻塞。
	row := computeStatusRow(cfg, st, odb, mdb, reader, writer, t, live, 30*time.Second, log)

	// 状态着色：OK/SYNCED→绿，DIVERGED/ERR→红，PENDING→黄。
	stateCode := cYellow
	switch row.State {
	case "OK", "SYNCED":
		stateCode = cGreen
	case "DIVERGED", "ERR":
		stateCode = cRed
	}

	srcShow := "—"
	if row.Source != nil {
		srcShow = fmt.Sprintf("%d", *row.Source)
	}
	tgtShow := "—"
	if row.Target != nil {
		tgtShow = fmt.Sprintf("%d", *row.Target)
	}
	deltaShow := "—"
	if row.Delta != nil {
		deltaShow = fmt.Sprintf("%d", *row.Delta)
	}
	progressShow := "—"
	if row.Progress != nil {
		progressShow = fmt.Sprintf("%.1f%%", *row.Progress)
	}

	fmt.Print(
		pad(trunc(row.Table, wTable), wTable) +
			pad(row.Mode, wMode) +
			statusColorize(pad(row.State, wState), stateCode) + " " +
			rpad(srcShow, wNum) +
			rpad(tgtShow, wNum) +
			rpad(deltaShow, wNum) +
			rpad(progressShow, wProg) +
			row.LastSync + "\n",
	)
}

// pad 左对齐定宽（右侧补空格）；超宽内容由调用方先 trunc。
func pad(s string, w int) string {
	if len(s) >= w {
		return s + " "
	}
	return s + strings.Repeat(" ", w-len(s))
}

// rpad 右对齐定宽（左侧补空格，便于数字列对齐）。
func rpad(s string, w int) string {
	if len(s) >= w {
		return s + " "
	}
	return strings.Repeat(" ", w-len(s)) + s + " "
}

// trunc 超长字符串截断为 w，末尾以 … 占位（保证表格对齐）。
func trunc(s string, w int) string {
	if len(s) <= w {
		return s
	}
	if w <= 1 {
		return s[:w]
	}
	return s[:w-1] + "…"
}
