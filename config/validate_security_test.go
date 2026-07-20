package config

import "testing"

func TestValidIdentifier(t *testing.T) {
	ok := []string{"EMP", "emp", "emp_01", "a", "ID", "col123"}
	for _, s := range ok {
		if !validIdentifier(s) {
			t.Errorf("应为合法标识符: %q", s)
		}
	}
	bad := []string{
		"", "a b", "a;b", "a\"b", "a`b", "a'b", "a]b", "a[b",
		"a.b", "a-b", "a+b", "a*b", "$a", "#a", "a(", "a)",
		"a/b", "a\\b", "a<b", "..", ".", "a..b",
		"一二三",                    // 非 ASCII
		string(make([]byte, 64)), // 超长
	}
	for _, s := range bad {
		if validIdentifier(s) {
			t.Errorf("应为非法标识符: %q", s)
		}
	}
}

func TestValidQualifiedName(t *testing.T) {
	ok := []string{"EMP", "SCOTT.EMP", "dbo.orders", "db.table"}
	for _, s := range ok {
		if !validQualifiedName(s) {
			t.Errorf("应为合法限定名: %q", s)
		}
	}
	bad := []string{"SCOTT.EMP.X", "a..b", ".EMP", "EMP.", "a b.c", "a;drop", "a.b.c.d"}
	for _, s := range bad {
		if validQualifiedName(s) {
			t.Errorf("应为非法限定名: %q", s)
		}
	}
}

func TestValidHost(t *testing.T) {
	ok := []string{"127.0.0.1", "10.0.0.5", "db.example.com", "::1", "[::1]:5432", "/var/run/pg.sock", "host-1"}
	for _, s := range ok {
		if !validHost(s) {
			t.Errorf("应为合法 host: %q", s)
		}
	}
	bad := []string{"", "a b", "a;b", "a\"b", "a'b", "a`b", "a b.c"}
	for _, s := range bad {
		if validHost(s) {
			t.Errorf("应为非法 host: %q", s)
		}
	}
}

func TestValidServiceName(t *testing.T) {
	ok := []string{"orcl", "orcl.example.com", "XE", "PDB$SEED", "sid_1"}
	for _, s := range ok {
		if !validServiceName(s) {
			t.Errorf("应为合法 service: %q", s)
		}
	}
	bad := []string{"", "a b", "a;b", "a\"b", "a'b"}
	for _, s := range bad {
		if validServiceName(s) {
			t.Errorf("应为非法 service: %q", s)
		}
	}
}

// baselineValidConfig 返回一个能通过 Validate 的最小合法配置（Oracle -> MySQL）。
func baselineValidConfig() *Config {
	return &Config{
		Source: DBConfig{Dialect: "oracle", Host: "10.0.0.1", Username: "app", Service: "orcl"},
		Target: DBConfig{Dialect: "mysql", Host: "10.0.0.2", Username: "app", Database: "shop"},
		Tables: []TableConfig{{Name: "EMP"}},
		State:  StateConfig{Backend: "sqlite"},
	}
}

func TestValidateAllowsCleanConfig(t *testing.T) {
	cfg := baselineValidConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("合法配置应校验通过，但报错: %v", err)
	}
	// 带 schema / qualified target / watermark / column_mapping 的合法扩展
	cfg.Tables[0].Schema = "SCOTT"
	cfg.Tables[0].Target = "SCOTT.EMP"
	cfg.Tables[0].Mode = "incremental"
	cfg.Tables[0].Watermark = "UPDATED_AT"
	cfg.Tables[0].ColumnMapping = map[string]string{"UPDATED_AT": "updated_at"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("合法扩展配置应校验通过，但报错: %v", err)
	}
}

func TestValidateRejectsInjection(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"table name injection", func(c *Config) { c.Tables[0].Name = `EMP"; DROP TABLE X;--` }},
		{"table name semicolon", func(c *Config) { c.Tables[0].Name = "emp;delete" }},
		{"watermark injection", func(c *Config) {
			c.Tables[0].Mode = "incremental"
			c.Tables[0].Watermark = "id OR 1=1"
		}},
		{"target qualified injection", func(c *Config) { c.Tables[0].Target = "a.b;c" }},
		{"schema injection", func(c *Config) { c.Tables[0].Schema = "sc`ott" }},
		{"column_mapping src", func(c *Config) { c.Tables[0].ColumnMapping = map[string]string{"a'b": "x"} }},
		{"column_mapping tgt", func(c *Config) { c.Tables[0].ColumnMapping = map[string]string{"x": "a)b"} }},
		{"source host injection", func(c *Config) { c.Source.Host = "10.0.0.1;DROP" }},
		{"target database injection", func(c *Config) { c.Target.Database = "shop; drop" }},
		{"source service injection", func(c *Config) { c.Source.Service = "orcl' OR '1'='1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baselineValidConfig()
			tc.mut(cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("应拒绝非法配置（%s），但 Validate 通过", tc.name)
			}
		})
	}
}

func TestValidateRejectsNonAsciiIdentifier(t *testing.T) {
	// 回归保护：白名单仅允许 ASCII 标识符，含中文/全角等一律拒绝（与防御设计一致）。
	cfg := baselineValidConfig()
	cfg.Tables[0].Name = "员工表"
	if err := cfg.Validate(); err == nil {
		t.Errorf("白名单仅允许 ASCII 标识符，含中文表名应被拒绝，但 Validate 通过")
	}
}
