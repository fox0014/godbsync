package cmd

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"godbsync/config"
	"godbsync/internal/dialect"
	"godbsync/internal/state"
)

//go:embed web/monitor.html
var monitorHTML string

// StatusOverview 是 /api/status 端点返回的完整快照，供 Web 仪表盘消费。
type StatusOverview struct {
	Source      string         `json:"source"`          // 源端标签：方言 host:port/db
	Target      string         `json:"target"`          // 目标端标签
	GeneratedAt string         `json:"generated_at"`    // 快照生成时间
	RefreshMs   int            `json:"refresh_ms"`      // 建议前端刷新间隔（毫秒）
	Live        bool           `json:"live"`            // 本快照是否含实时 COUNT(*) 一致性校验
	Error       string         `json:"error,omitempty"` // verify 模式下连接/查询失败信息
	StateCounts map[string]int `json:"state_counts"`    // 各状态表数量
	Overall     OverallStatus  `json:"overall"`         // 跨表整体进度/状态（纯计算派生，无需存储）
	Tables      []StatusRow    `json:"tables"`
}

// OverallStatus 是跨所有表的整体进度与状态汇总，由 collectOverview 在请求时
// 从各表已有的 written_rows / source_total（已持久化在进度后端）现算得出，不新增任何存储。
//   - Progress：行数加权整体进度 = Σwritten_rows / Σsource_total（仅纳入两端已知的表，保证 ≤100%）。
//   - State/Label：由各表状态推导——有 SYNCING→进行中；verify 模式有 DIVERGED/ERR 则覆盖；
//     全 OK/SYNCED→已完成；有 PENDING（从未同步）且无运行→待同步。
type OverallStatus struct {
	State       string   `json:"state"`              // IN_PROGRESS/COMPLETED/PENDING/DIVERGED/ERROR
	Label       string   `json:"label"`              // 进行中/已完成/待同步/不一致/错误
	Progress    *float64 `json:"progress,omitempty"` // 0..100，nil 表示无法计算（无已知 source_total）
	TablesTotal int      `json:"tables_total"`       // 配置的表总数
	TablesDone  int      `json:"tables_done"`        // 已完成表数（OK + SYNCED）
}

// collectOverview 汇总所有表的同步状态快照，供 /api/status 与测试复用。
// live=true 时对实时行数（COUNT(*)）查询施加超时保护，避免慢查询阻塞 HTTP 响应。
func collectOverview(cfg *config.Config, st state.Backend, odb, mdb *sql.DB,
	reader dialect.Reader, writer dialect.Writer, tables []config.TableConfig,
	live bool, refreshMs int, queryTimeout time.Duration, log *logrus.Logger) StatusOverview {

	ov := StatusOverview{
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		RefreshMs:   refreshMs,
		Live:        live,
		StateCounts: map[string]int{},
	}
	var totalWritten, totalSource int64
	tablesTotal := len(tables)
	tablesDone := 0
	anySync, anyPending, anyDiv, anyErr := false, false, false, false
	for _, t := range tables {
		r := computeStatusRow(cfg, st, odb, mdb, reader, writer, t, live, queryTimeout, log)
		ov.StateCounts[r.State]++
		ov.Tables = append(ov.Tables, r)
		// 行数加权：仅纳入「源行数已知且 >0」的表，使分子分母对齐、整体进度恒 ≤100%。
		if r.Source != nil && *r.Source > 0 {
			totalSource += *r.Source
			w := int64(0)
			if r.WrittenRows != nil {
				w = *r.WrittenRows
			}
			totalWritten += w
		}
		switch r.State {
		case "SYNCING":
			anySync = true
		case "PENDING":
			anyPending = true
		case "DIVERGED":
			anyDiv = true
		case "ERR":
			anyErr = true
		case "OK", "SYNCED":
			tablesDone++
		}
	}

	// 整体状态推导（优先级：进行中 > 错误 > 不一致 > 待同步 > 已完成）。
	var oState, oLabel string
	switch {
	case anySync:
		oState, oLabel = "IN_PROGRESS", "进行中"
	case anyErr:
		oState, oLabel = "ERROR", "错误"
	case anyDiv:
		oState, oLabel = "DIVERGED", "不一致"
	case anyPending:
		oState, oLabel = "PENDING", "待同步"
	default:
		oState, oLabel = "COMPLETED", "已完成"
	}

	ov.Overall = OverallStatus{State: oState, Label: oLabel, TablesTotal: tablesTotal, TablesDone: tablesDone}
	if totalSource > 0 {
		// 四舍五入到 1 位小数，避免 73.24000000000001 这类浮点噪声污染 API。
		p := math.Round(float64(totalWritten)/float64(totalSource)*1000) / 10
		ov.Overall.Progress = &p
	}

	return ov
}

// runMonitor 启动内嵌 HTTP 服务，提供可视化同步状态监控面板。
// 默认纯后端驱动（live=false）：只读取进度后端（source_total / written_rows），
// 不建立任何源/目标库连接、不做 COUNT(*)，因此 monitor 可与业务库完全解耦运行。
// 仅当请求带 ?verify=1 或启动时带 --verify 时，才按需懒连接源/目标库做一次性一致性校验。
// 与 daemon 解耦：monitor 只读进度后端，daemon 负责写入，可分别运行于不同进程。
// Ctrl+C / SIGTERM 优雅关闭。
func runMonitor(cfg *config.Config, st state.Backend, live bool,
	addr string, refresh time.Duration, filter []string, log *logrus.Logger) int {

	tables := filterTables(cfg.Tables, filter)
	if len(tables) == 0 {
		fmt.Println("没有匹配的表（检查 -table 参数）")
		return 0
	}

	srcExtra := cfg.Source.Service
	if cfg.Source.Dialect == "mysql" || cfg.Source.Dialect == "sqlserver" {
		srcExtra = cfg.Source.Database
	}
	tgtExtra := cfg.Target.Database
	if cfg.Target.Dialect == "oracle" {
		tgtExtra = cfg.Target.Service
	}
	srcLabel := fmt.Sprintf("%s %s:%d/%s", cfg.Source.Dialect, cfg.Source.Host, cfg.Source.Port, srcExtra)
	tgtLabel := fmt.Sprintf("%s %s:%d/%s", cfg.Target.Dialect, cfg.Target.Host, cfg.Target.Port, tgtExtra)

	refreshMs := int(refresh / time.Millisecond)
	if refreshMs <= 0 {
		refreshMs = 2000
	}

	// 懒连接：仅在需要实时校验时（?verify=1 或 --verify）才连源/目标库，缓存复用。
	var connMu sync.Mutex
	var sODB, sMDB *sql.DB
	var sReader dialect.Reader
	var sWriterW dialect.Writer
	var connsReady bool
	var connsErr error
	ensureConns := func() error {
		connMu.Lock()
		defer connMu.Unlock()
		if connsReady {
			return connsErr
		}
		connsReady = true
		o, e1 := openSourceDB(cfg.Source)
		if e1 != nil {
			connsErr = fmt.Errorf("源库 %s 连接失败: %v", cfg.Source.Dialect, e1)
			return connsErr
		}
		m, e2 := openTargetDB(cfg.Target)
		if e2 != nil {
			_ = o.Close()
			connsErr = fmt.Errorf("目标库 %s 连接失败: %v", cfg.Target.Dialect, e2)
			return connsErr
		}
		if r, err := dialect.NewReader(cfg.Source.Dialect); err == nil {
			sReader = r
		}
		if w, err := dialect.NewWriter(cfg.Target.Dialect); err == nil {
			sWriterW = w
		}
		sODB, sMDB = o, m
		return nil
	}

	// 将服务端的建议刷新间隔注入内嵌 HTML（仅离线字符串替换，无外部依赖）。
	html := strings.Replace(monitorHTML, "/*__REFRESH_MS__*/2000", fmt.Sprintf("/*__REFRESH_MS__*/%d", refreshMs), 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		// 本请求是否做实时校验：启动时 --verify 或 URL 带 ?verify=1。
		reqLive := live || r.URL.Query().Get("verify") == "1"
		var odb, mdb *sql.DB
		var reader dialect.Reader
		var writer dialect.Writer
		var verifyErr string
		if reqLive {
			if e := ensureConns(); e != nil {
				verifyErr = e.Error()
			} else {
				odb, mdb = sODB, sMDB
				reader, writer = sReader, sWriterW
			}
		}
		ov := collectOverview(cfg, st, odb, mdb, reader, writer, tables, reqLive, refreshMs, monitorQueryTimeout, log)
		ov.Source = srcLabel
		ov.Target = tgtLabel
		ov.Error = verifyErr
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(ov)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Infof("monitor 收到退出信号，正在停止")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Infof("monitor 已启动，浏览器打开 http://%s/ （Ctrl+C 退出）", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Errorf("monitor 服务失败: %v", err)
		return 1
	}
	return 0
}
