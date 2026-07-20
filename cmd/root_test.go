package cmd

import (
	"reflect"
	"strings"
	"testing"

	"godbsync/config"

	"github.com/sirupsen/logrus"
)

// TestResolveCheckpointTargets 验证 -table 过滤项到 checkpoint 表实际目标表名的映射：
// 支持按源表名、目标表名（大小写不敏感）匹配，未匹配时回退为原始输入。
func TestResolveCheckpointTargets(t *testing.T) {
	cfg := &config.Config{
		Tables: []config.TableConfig{
			{Name: "EMPLOYEES", Target: "emp"},
			{Name: "DEPT", Target: ""}, // 未配置 target，目标名 = 源名
			{Name: "ORDERS", Schema: "SCOTT", Target: "scott_orders"},
		},
	}
	cases := []struct {
		name   string
		filter []string
		want   []string // nil 表示「全部表」
	}{
		{"nil -> all", nil, nil},
		{"empty -> all", []string{}, nil},
		{"match by source name", []string{"EMPLOYEES"}, []string{"emp"}},
		{"match by target name", []string{"emp"}, []string{"emp"}},
		{"target==name case-insensitive", []string{"dept"}, []string{"DEPT"}},
		{"match by target with schema", []string{"SCOTT_ORDERS"}, []string{"scott_orders"}},
		{"no match -> raw input", []string{"MISSING"}, []string{"MISSING"}},
		{"multi, dedup by target", []string{"emp", "EMPLOYEES"}, []string{"emp"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveCheckpointTargets(cfg, c.filter)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("filter=%v got=%v want=%v", c.filter, got, c.want)
			}
		})
	}
}

// TestParseFilter 验证逗号分隔表名过滤器的解析。
func TestParseFilter(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty string", "", nil},
		{"whitespace only", "  ", nil},
		{"single table", "EMPLOYEES", []string{"EMPLOYEES"}},
		{"multiple tables", "EMP,DEPT,SAL", []string{"EMP", "DEPT", "SAL"}},
		{"with spaces around", " EMP , DEPT ", []string{"EMP", "DEPT"}},
		{"trailing comma", "A,B,", []string{"A", "B"}},
		{"leading comma", ",A", []string{"A"}},
		{"empty middle", "A,,B", []string{"A", "B"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseFilter(c.input)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got=%v want=%v", got, c.want)
			}
		})
	}
}

// TestNewLogger 验证日志器按级别字符串创建。
func TestNewLogger(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  logrus.Level
	}{
		{"debug", "debug", logrus.DebugLevel},
		{"info", "info", logrus.InfoLevel},
		{"warn", "warn", logrus.WarnLevel},
		{"warning alias", "warning", logrus.WarnLevel},
		{"error", "error", logrus.ErrorLevel},
		{"empty defaults to info", "", logrus.InfoLevel},
		{"unknown defaults to info", "nonsense", logrus.InfoLevel},
		{"case insensitive", "DEBUG", logrus.DebugLevel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLogger(c.level)
			if got := l.GetLevel(); got != c.want {
				t.Errorf("level=%q: got %v, want %v", c.level, got, c.want)
			}
			// 验证 format 已设置
			if ff, ok := l.Formatter.(*logrus.TextFormatter); !ok || !ff.FullTimestamp {
				t.Errorf("TextFormatter 应设置 FullTimestamp=true")
			}
			if ff, ok := l.Formatter.(*logrus.TextFormatter); !ok || ff.TimestampFormat != "2006-01-02 15:04:05" {
				t.Errorf("时间格式应为 2006-01-02 15:04:05, got %q", ff.TimestampFormat)
			}
		})
	}
}

// TestNewLogger_StructuredOutput 验证日志格式中不含低效颜色代码。因项目运行
// 时通常重定向到文件或容器日志，带颜色的输出反而干扰解析。
func TestNewLogger_StructuredOutput(t *testing.T) {
	l := newLogger("info")
	tf, ok := l.Formatter.(*logrus.TextFormatter)
	if !ok {
		t.Fatal("expected TextFormatter")
	}
	// logrus 默认的 DisableColors 为 false，而 TextFormatter 在不是 TTY 输出
	// 时会自动禁用颜色，因此这里只验证 ForceColors 未被显式开启。
	if tf.ForceColors {
		t.Error("ForceColors 不应显式开启")
	}
}

// TestVersion 确保编译时常量非空。
func TestVersion(t *testing.T) {
	if Version == "" {
		t.Error("Version 不应为空")
	}
}

// TestPrintUsage 确保帮助文本可执行（不 panic）。
func TestPrintUsage(t *testing.T) {
	// 仅验证函数存在且不会 panic
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("printUsage panic: %v", r)
		}
	}()
	// printUsage 写入 stdout，这里只调用确保不 panic
	printUsage()
}

// TestResolveCheckpointTargets_EmptyCfg 空配置下 filter 全回退为原始输入。
func TestResolveCheckpointTargets_EmptyCfg(t *testing.T) {
	cfg := &config.Config{}
	filter := []string{"T1", "T2"}
	got := resolveCheckpointTargets(cfg, filter)
	// 配置表为空，全部 filter 无匹配，回退原始输入
	want := []string{"T1", "T2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

// TestResolveCheckpointTargets_DedupCaseInsensitive 多 filter 项会去重。
func TestResolveCheckpointTargets_DedupCaseInsensitive(t *testing.T) {
	cfg := &config.Config{
		Tables: []config.TableConfig{
			{Name: "EMP"},
			{Name: "DEPT"},
		},
	}
	filter := []string{"emp", "EMP"}
	got := resolveCheckpointTargets(cfg, filter)
	if len(got) != 1 || !strings.EqualFold(got[0], "EMP") {
		t.Fatalf("应去重后仅剩 EMP, got %v", got)
	}
}
