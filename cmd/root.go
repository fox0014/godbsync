package cmd

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"godbsync/config"
	"godbsync/internal/dialect"
	"godbsync/internal/state"
	"godbsync/internal/syncer"

	// 方言实现：通过 init() 自注册到 dialect 工厂。
	// blank import 确保 oracle/mysql/sqlserver 的 RegisterReader/RegisterWriter 被执行，
	// 同时 cmd 本身不直接依赖驱动包，保持源/目标方言的可替换性。
	_ "godbsync/internal/mysql"
	oracle "godbsync/internal/oracle"
	_ "godbsync/internal/postgresql"
	_ "godbsync/internal/sqlserver"
)

// Version 构建版本号。
const Version = "0.1.0"

// Execute 解析命令行参数并分发到对应子命令。返回进程退出码。
func Execute() int {
	args := os.Args[1:]
	if len(args) < 1 {
		printUsage()
		return 2
	}
	cmdName := args[0]

	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "配置文件路径")
	tableFlag := fs.String("table", "", "仅处理指定表，逗号分隔；留空表示全部")
	interval := fs.Duration("interval", 30*time.Second, "daemon 模式下增量轮询间隔")
	logLevel := fs.String("log", "", "日志级别 debug/info/warn/error（覆盖配置文件）")
	fresh := fs.Bool("fresh", false, "full 模式：忽略已有断点，从头全量同步（不续传）")
	clear := fs.Bool("clear", false, "checkpoint 模式：清除断点（需配合 -table 或 -all）")
	all := fs.Bool("all", false, "checkpoint -clear 模式：清除全部表断点（不与 -table 共用）")
	stateFlag := fs.String("state", "", "进度存储后端（覆盖配置文件 state 段）：file[=/path/db]（SQLite 本地文件）或 mysql（存入独立进度库，见 state.mysql 段）")
	watch := fs.Duration("watch", 0, "status 模式：每隔 N 持续刷新（如 2s）")
	quick := fs.Bool("quick", false, "status / monitor 模式：只显示断点/水位，不实时统计源/目标行数")
	noScroll := fs.Bool("scroll", false, "status --watch 模式：翻滚输出（向下追加）；默认原地覆盖刷新（清屏重绘），仅终端有效")
	addr := fs.String("addr", ":8080", "monitor 模式：HTTP 监听地址（如 :8080 或 127.0.0.1:9000）")
	refresh := fs.Duration("refresh", 2*time.Second, "monitor 模式：前端自动刷新间隔（如 2s）")
	_ = fs.Parse(args[1:])

	if cmdName == "version" || cmdName == "-v" || cmdName == "--version" {
		fmt.Printf("godbsync %s\n", Version)
		return 0
	}

	if cmdName == "help" || cmdName == "-h" || cmdName == "--help" {
		printUsage()
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return 1
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}
	log := newLogger(cfg.LogLevel)

	// 注入 Oracle 方言的日志器与数组绑定开关（默认开启；异常时可经配置关闭回退传统写法）。
	oracle.SetLogger(log)
	oracle.SetArrayBinding(cfg.ResolveOracleArrayBinding())

	stateKind, statePath := state.ParseSpec(cfg.ResolveStateSpec(*stateFlag))

	// checkpoint 子命令仅需「状态后端」：
	//   - SQLite 后端：直接打开本地文件，连目标库都不需要；
	//   - MySQL  后端：进度存于独立配置的 state.mysql（与目标库解耦），
	//     未配置时回退复用目标 MySQL 连接（向后兼容）。
	if cmdName == "checkpoint" {
		st, extra, e := openStateBackend(cfg, stateKind, statePath, nil)
		if e != nil {
			log.Errorf("%v", e)
			return 1
		}
		if extra != nil {
			defer extra.Close()
		}
		defer st.Close()
		return runCheckpoint(st, cfg, parseFilter(*tableFlag), *clear, *all, log)
	}

	// status 子命令：读取进度后端（断点/水位）+ 实时源/目标行数，展示各表同步状态。
	// quick=false 时需要源端 + 目标端双连接以做一致性统计；quick=true 仅读进度后端。
	if cmdName == "status" {
		st, extra, e := openStateBackend(cfg, stateKind, statePath, nil)
		if e != nil {
			log.Errorf("%v", e)
			return 1
		}
		if extra != nil {
			defer extra.Close()
		}
		defer st.Close()

		var odb, mdb *sql.DB
		if !*quick {
			odb, e = openSourceDB(cfg.Source)
			if e != nil {
				log.Errorf("源库 %s 连接失败: %v", cfg.Source.Dialect, e)
				return 1
			}
			defer odb.Close()
			mdb, e = openTargetDB(cfg.Target)
			if e != nil {
				log.Errorf("目标库 %s 连接失败: %v", cfg.Target.Dialect, e)
				return 1
			}
			defer mdb.Close()
		}
		return runStatus(cfg, st, odb, mdb, *quick, *watch, *noScroll, log, parseFilter(*tableFlag))
	}

	// monitor 子命令：启动内嵌 HTTP 服务，浏览器查看可视化同步状态。
	// 与 status 类似——只读进度后端（+ 非 quick 时的源/目标实时行数）；
	// 与 daemon 解耦，可分别运行于不同进程（daemon 写进度，monitor 读进度）。
	if cmdName == "monitor" {
		st, extra, e := openStateBackend(cfg, stateKind, statePath, nil)
		if e != nil {
			log.Errorf("%v", e)
			return 1
		}
		if extra != nil {
			defer extra.Close()
		}
		defer st.Close()

		var odb, mdb *sql.DB
		if !*quick {
			odb, e = openSourceDB(cfg.Source)
			if e != nil {
				log.Errorf("源库 %s 连接失败: %v", cfg.Source.Dialect, e)
				return 1
			}
			defer odb.Close()
			mdb, e = openTargetDB(cfg.Target)
			if e != nil {
				log.Errorf("目标库 %s 连接失败: %v", cfg.Target.Dialect, e)
				return 1
			}
			defer mdb.Close()
		}
		return runMonitor(cfg, st, odb, mdb, *quick, *addr, *refresh, parseFilter(*tableFlag), log)
	}

	// 其余命令需要源端 + 目标端双连接。
	odb, err := openSourceDB(cfg.Source)
	if err != nil {
		log.Errorf("源库 %s 连接失败: %v", cfg.Source.Dialect, err)
		return 1
	}
	defer odb.Close()

	mdb, err := openTargetDB(cfg.Target)
	if err != nil {
		log.Errorf("目标库 %s 连接失败: %v", cfg.Target.Dialect, err)
		return 1
	}
	defer mdb.Close()

	// 构造方言实现（读取走 Reader，写入走 Writer）。
	reader, err := dialect.NewReader(cfg.Source.Dialect)
	if err != nil {
		log.Errorf("不支持的源方言: %v", err)
		return 1
	}
	writer, err := dialect.NewWriter(cfg.Target.Dialect)
	if err != nil {
		log.Errorf("不支持的目标方言: %v", err)
		return 1
	}

	// 构造状态后端（水位表/断点表所在位置）。
	// MySQL 后端优先使用独立配置的 state.mysql；未配置时回退复用目标 MySQL 连接。
	st, extra, err := openStateBackend(cfg, stateKind, statePath, mdb)
	if err != nil {
		log.Errorf("构造进度后端失败: %v", err)
		return 1
	}
	if extra != nil {
		defer extra.Close()
	}
	defer st.Close()

	s := syncer.New(cfg, odb, mdb, reader, writer, st, log.WithField("app", "godbsync"))

	// 日志显示源/目标连接摘要：Oracle 显示 Service，MySQL/SQL Server 显示 Database。
	srcExtra := cfg.Source.Service
	if cfg.Source.Dialect == "mysql" || cfg.Source.Dialect == "sqlserver" {
		srcExtra = cfg.Source.Database
	}
	tgtExtra := cfg.Target.Database
	if cfg.Target.Dialect == "oracle" {
		tgtExtra = cfg.Target.Service
	}
	log.Infof("源 %s %s:%d/%s → 目标 %s %s:%d/%s",
		cfg.Source.Dialect, cfg.Source.Host, cfg.Source.Port, srcExtra,
		cfg.Target.Dialect, cfg.Target.Host, cfg.Target.Port, tgtExtra)

	filter := parseFilter(*tableFlag)

	switch cmdName {
	case "init":
		if err := s.InitSchema(filter); err != nil {
			log.Errorf("init 失败: %v", err)
			return 1
		}
		log.Infof("init 完成")
	case "full":
		for _, t := range s.Tables(filter) {
			boundary, err := s.FullSync(t, *fresh)
			if err != nil {
				log.Errorf("全量同步 %s 失败: %v", t.Name, err)
				return 1
			}
			if cfg.ResolveAutoValidate() {
				if err := s.Validate(t, boundary); err != nil {
					log.Errorf("校验 %s 失败: %v", t.Name, err)
					return 1
				}
			}
		}
	case "incr":
		for _, t := range s.Tables(filter) {
			if t.ResolveMode() != "incremental" {
				log.Warnf("表 %s 非增量模式（mode=%s），跳过", t.Name, t.ResolveMode())
				continue
			}
			newWM, err := s.IncrementalSync(t)
			if err != nil {
				log.Errorf("增量同步 %s 失败: %v", t.Name, err)
				return 1
			}
			if cfg.ResolveAutoValidate() {
				var boundary *syncer.ValidateBoundary
				if newWM != nil && t.Watermark != "" {
					boundary = &syncer.ValidateBoundary{Column: t.Watermark, Value: newWM}
				}
				if err := s.Validate(t, boundary); err != nil {
					log.Errorf("校验 %s 失败: %v", t.Name, err)
					return 1
				}
			}
		}
	case "daemon":
		return runDaemon(s, filter, *interval, cfg.ResolveAutoValidate(), log)
	case "validate":
		failed := false
		for _, t := range s.Tables(filter) {
			if err := s.Validate(t, nil); err != nil {
				log.Errorf("校验 %s 失败: %v", t.Name, err)
				failed = true
			}
		}
		if failed {
			return 1
		}
		log.Infof("validate 完成，所有表行数一致")
	default:
		printUsage()
		return 2
	}
	return 0
}

// openSourceDB 按方言打开源数据库连接。
func openSourceDB(cfg config.DBConfig) (*sql.DB, error) {
	r, err := dialect.NewReader(cfg.Dialect)
	if err != nil {
		return nil, err
	}
	return r.Open(cfg.DSN())
}

// openTargetDB 按方言打开目标数据库连接。
func openTargetDB(cfg config.DBConfig) (*sql.DB, error) {
	w, err := dialect.NewWriter(cfg.Dialect)
	if err != nil {
		return nil, err
	}
	return w.Open(cfg.DSN())
}

// openStateDB 打开独立配置的进度 MySQL（state.mysql 段）并探活。
// 进度库始终是 MySQL，直接用 mysql 驱动，与目标库方言无关。
func openStateDB(cfg config.Config) (*sql.DB, error) {
	db, err := sql.Open("mysql", cfg.State.MySQL.DSN())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openStateBackend 构造进度后端：
//   - sqlite：打开本地 SQLite 文件（statePath）；
//   - mysql ：进度存于 MySQL。若 state.mysql 段配置了独立连接（host 非空），
//     则连接该独立 MySQL（适用任意目标方言）；否则回退复用 target 连接
//     （要求目标为 MySQL，已由 Validate 保证）。
//
// 返回三元组：后端、需要 defer Close 的独立连接（sqlite 或回退复用场景为 nil，
// 由调用方已有的 target 连接 defer 处理）、错误。
func openStateBackend(cfg *config.Config, stateKind, statePath string, target *sql.DB) (state.Backend, *sql.DB, error) {
	if stateKind == "sqlite" {
		st, err := state.OpenSQLite(statePath)
		if err != nil {
			return nil, nil, err
		}
		return st, nil, nil
	}
	// mysql 后端
	if cfg.State.MySQL.Host != "" {
		db, err := openStateDB(*cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("打开独立进度 MySQL 失败: %w", err)
		}
		return state.NewMySQLBackend(db), db, nil
	}
	// 回退：复用目标库连接（仅当目标为 mysql）
	if target == nil {
		var err error
		target, err = openTargetDB(cfg.Target)
		if err != nil {
			return nil, nil, fmt.Errorf("打开目标库失败: %w", err)
		}
	}
	return state.NewMySQLBackend(target), nil, nil
}

// runDaemon 循环执行增量同步，直到收到退出信号。
func runDaemon(s *syncer.Syncer, filter []string, interval time.Duration, autoValidate bool, log *logrus.Logger) int {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	tables := s.Tables(filter)
	incrTables := make([]config.TableConfig, 0, len(tables))
	for _, t := range tables {
		if t.ResolveMode() == "incremental" {
			incrTables = append(incrTables, t)
		} else {
			log.Warnf("daemon 跳过非增量表 %s（mode=%s）", t.Name, t.ResolveMode())
		}
	}
	if len(incrTables) == 0 {
		log.Errorf("没有可增量同步的表，请检查配置 mode=incremental 与 watermark")
		return 1
	}

	log.Infof("进入 daemon 模式，每 %s 轮询一次，Ctrl+C 退出", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		round := time.Now()
		for _, t := range incrTables {
			newWM, err := s.IncrementalSync(t)
			if err != nil {
				log.Errorf("增量同步 %s 失败: %v", t.Name, err)
				continue
			}
			if autoValidate {
				var boundary *syncer.ValidateBoundary
				if newWM != nil && t.Watermark != "" {
					boundary = &syncer.ValidateBoundary{Column: t.Watermark, Value: newWM}
				}
				if err := s.Validate(t, boundary); err != nil {
					log.Errorf("校验 %s 失败: %v", t.Name, err)
				}
			}
		}
		log.Infof("本轮增量同步耗时 %s", time.Since(round))
		select {
		case <-ticker.C:
		case <-stop:
			log.Infof("收到退出信号，daemon 停止")
			return 0
		}
	}
}

// parseFilter 将逗号分隔的表名字符串解析为切片。
func parseFilter(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resolveCheckpointTargets 将 -table 过滤项解析为 checkpoint 表中实际存储的目标表名。
// 优先按配置中 TableConfig 的 Name / TargetName 匹配，回退使用原始输入（大小写不敏感）。
// 返回 nil 表示「全部表」（未指定 -table）。
func resolveCheckpointTargets(cfg *config.Config, filter []string) []string {
	if len(filter) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		key := strings.ToUpper(name)
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	for _, f := range filter {
		matched := false
		fu := strings.ToUpper(f)
		for _, t := range cfg.Tables {
			if strings.ToUpper(t.Name) == fu || strings.ToUpper(t.TargetName()) == fu {
				add(t.TargetName())
				matched = true
			}
		}
		if !matched {
			add(f) // 配置中无匹配时，按用户输入原样尝试
		}
	}
	return out
}

// runCheckpoint 实现 checkpoint 子命令：列出或清除全量断点。
//   - clear=false：列出全部表断点，或 -table 指定的单表/多表断点；
//   - clear=true ：清除 -table 指定的表断点，或 -all 清除全部表断点。
//
// 该函数仅依赖「状态后端」（MySQL 或本地 SQLite），故在 Execute 中于
// 源库连接之前被调用；本地 SQLite 后端下甚至无需连接目标库。
func runCheckpoint(st state.Backend, cfg *config.Config, filter []string, clear, all bool, log *logrus.Logger) int {
	if err := st.EnsureCheckpointTable(); err != nil {
		log.Errorf("确保断点表存在失败: %v", err)
		return 1
	}
	targets := resolveCheckpointTargets(cfg, filter)

	// —— 清除模式 ——
	if clear {
		if all && len(targets) > 0 {
			log.Errorf("-all 与 -table 不能同时使用")
			return 2
		}
		if all {
			if err := st.ClearAllCheckpoints(); err != nil {
				log.Errorf("清除全部断点失败: %v", err)
				return 1
			}
			fmt.Println("已清除全部表的全量断点")
			return 0
		}
		if len(targets) == 0 {
			log.Errorf("清除断点必须指定 -table <表名> 或 -all")
			return 2
		}
		var failed bool
		for _, t := range targets {
			if err := st.ClearCheckpoint(t); err != nil {
				log.Errorf("清除表 %s 断点失败: %v", t, err)
				failed = true
				continue
			}
			fmt.Printf("已清除表 %s 的断点\n", t)
		}
		if failed {
			return 1
		}
		return 0
	}

	// —— 查看模式 ——
	if len(targets) > 0 {
		// 单表/多表查看
		var missing bool
		for _, t := range targets {
			anchor, kind, updatedAt, found, err := st.GetCheckpoint(t)
			if err != nil {
				log.Errorf("读取表 %s 断点失败: %v", t, err)
				return 1
			}
			if !found {
				fmt.Printf("表 %s: 暂无断点记录（从未同步或已清除）\n", t)
				missing = true
				continue
			}
			fmt.Printf("表 %s:\n  锚点值: %s\n  锚点类型: %s\n  更新时间: %s\n",
				t, anchor, kind, updatedAt)
		}
		if missing {
			return 0
		}
		return 0
	}

	// 未指定 -table：列出全部
	rows, err := st.ListCheckpoints()
	if err != nil {
		log.Errorf("列举断点失败: %v", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Println("断点表暂无任何断点记录")
		return 0
	}
	const (
		fmtT = "%-32s %-40s %-8s %s\n"
		sep  = "-------------------------------- ---------------------------------------- -------- -------------------\n"
	)
	fmt.Printf(fmtT, "TABLE", "ANCHOR", "KIND", "UPDATED_AT")
	fmt.Print(sep)
	for _, r := range rows {
		fmt.Printf(fmtT, r.TableName, r.Anchor, r.AnchorKind, r.UpdatedAt)
	}
	fmt.Printf("\n共 %d 条断点记录\n", len(rows))
	return 0
}

// newLogger 根据级别字符串创建 logrus 日志器。
func newLogger(level string) *logrus.Logger {
	l := logrus.New()
	l.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})
	lv := logrus.InfoLevel
	switch strings.ToLower(level) {
	case "debug":
		lv = logrus.DebugLevel
	case "info":
		lv = logrus.InfoLevel
	case "warn", "warning":
		lv = logrus.WarnLevel
	case "error":
		lv = logrus.ErrorLevel
	}
	l.SetLevel(lv)
	return l
}

func printUsage() {
	fmt.Print(`godbsync — 多源数据库同步工具（默认 Oracle → MySQL）

用法:
  godbsync <command> [flags]

命令:
  init      根据源表在目标侧建表（含水位表）
  full      全量同步（支持断点续传；可选先清空目标表）
  incr      增量同步（基于水位列推进）
  daemon    以固定间隔循环执行增量同步
  validate  对比源端与目标端两侧行数
  status    展示各表同步状态（断点/水位 + 源目标行数一致性）
  monitor   启动内嵌 Web 服务，浏览器可视化监控同步状态（http://:8080/）
  checkpoint 查看或清除全量断点（godbsync_checkpoint）
  version   打印版本号

全局参数:
  -config string   配置文件路径（默认 config.yaml）
  -table  string   仅处理指定表，逗号分隔（默认全部）
  -log    string   日志级别 debug/info/warn/error（覆盖配置文件）
  -interval duration  daemon 轮询间隔（默认 30s）
  -fresh    bool      full 模式专用：忽略已有断点，从头全量同步
  -clear    bool      checkpoint 模式专用：清除断点（需配合 -table 或 -all）
  -all      bool      checkpoint -clear 专用：清除全部表断点（不与 -table 共用）
  -state    string    进度存储后端（覆盖配置文件 state 段）：
                          file[=/path/db]（SQLite 本地文件）或 mysql（存入独立进度库，需在 state.mysql 段配置连接）

示例:
  godbsync init -config config.yaml
  godbsync full -config config.yaml -table EMPLOYEES
  godbsync full -config config.yaml -table EMPLOYEES -fresh
  godbsync daemon -config config.yaml -interval 1m
  godbsync checkpoint                                    # 列出全部表断点
  godbsync checkpoint -table EMPLOYEES                   # 查看单表断点
  godbsync checkpoint -clear -table EMPLOYEES            # 清除单表断点
  godbsync checkpoint -clear -all                        # 清除全部断点
  godbsync status -config config.yaml                    # 展示各表同步状态（含源/目标行数一致性）
  godbsync status -config config.yaml --watch 2s        # 每 2s 刷新（默认原地覆盖，清屏重绘）
  godbsync status -config config.yaml --watch 2s --scroll    # 改为翻滚输出（向下追加）
  godbsync status -config config.yaml --quick           # 仅读断点/水位，不统计行数
  godbsync monitor -config config.yaml                   # 启动 Web 监控（默认 :8080，浏览器打开）
  godbsync monitor -config config.yaml -addr 127.0.0.1:9000 -refresh 5s   # 指定地址与刷新间隔
  godbsync monitor -config config.yaml --quick           # 仅断点/水位，不连库统计行数
  # CLI 覆盖配置文件 state 段（临时切换后端）：
  #   -state mysql 需要配置文件中已填 state.mysql 段（独立进度库），或目标库本身为 MySQL
  godbsync init -config config.yaml -state mysql
  godbsync full -config config.yaml -state file=./other.db
`)
}
