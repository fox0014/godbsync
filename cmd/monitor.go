package cmd

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	Source      string         `json:"source"`       // 源端标签：方言 host:port/db
	Target      string         `json:"target"`       // 目标端标签
	GeneratedAt string         `json:"generated_at"` // 快照生成时间
	RefreshMs   int            `json:"refresh_ms"`   // 建议前端刷新间隔（毫秒）
	Quick       bool           `json:"quick"`        // 是否 quick（无实时行数）
	StateCounts map[string]int `json:"state_counts"` // 各状态表数量
	Tables      []StatusRow    `json:"tables"`
}

// collectOverview 汇总所有表的同步状态快照，供 /api/status 与测试复用。
func collectOverview(cfg *config.Config, st state.Backend, odb, mdb *sql.DB,
	reader dialect.Reader, writer dialect.Writer, tables []config.TableConfig,
	quick bool, refreshMs int, log *logrus.Logger) StatusOverview {

	ov := StatusOverview{
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		RefreshMs:   refreshMs,
		Quick:       quick,
		StateCounts: map[string]int{},
	}
	for _, t := range tables {
		r := computeStatusRow(cfg, st, odb, mdb, reader, writer, t, quick, log)
		ov.StateCounts[r.State]++
		ov.Tables = append(ov.Tables, r)
	}
	return ov
}

// runMonitor 启动内嵌 HTTP 服务，提供可视化同步状态监控面板。
// 与 daemon 解耦：monitor 只读进度后端（+ 可选实时行数），daemon 负责写入，
// 因此二者可分别运行于不同进程。Ctrl+C / SIGTERM 优雅关闭。
func runMonitor(cfg *config.Config, st state.Backend, odb, mdb *sql.DB, quick bool,
	addr string, refresh time.Duration, filter []string, log *logrus.Logger) int {

	var reader dialect.Reader
	var writer dialect.Writer
	if !quick {
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

	// 将服务端的建议刷新间隔注入内嵌 HTML（仅离线字符串替换，无外部依赖）。
	html := strings.Replace(monitorHTML, "/*__REFRESH_MS__*/2000", fmt.Sprintf("/*__REFRESH_MS__*/%d", refreshMs), 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		ov := collectOverview(cfg, st, odb, mdb, reader, writer, tables, quick, refreshMs, log)
		ov.Source = srcLabel
		ov.Target = tgtLabel
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
