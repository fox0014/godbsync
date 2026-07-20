package state

import "testing"

// TestParseSpec 验证 -state 选项解析的各类输入。
func TestParseSpec(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantKind string
		wantPath string
	}{
		{"empty -> mysql", "", "mysql", ""},
		{"explicit mysql", "mysql", "mysql", ""},
		{"file default path", "file", "sqlite", "godbsync_state.db"},
		{"file with custom path", "file=/tmp/state.db", "sqlite", "/tmp/state.db"},
		{"file= exactly 5 chars -> mysql fallback", "file=", "mysql", ""},
		{"unknown -> mysql fallback", "postgresql", "mysql", ""},
		{"partial match not fooled", "filex", "mysql", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotKind, gotPath := ParseSpec(c.input)
			if gotKind != c.wantKind {
				t.Errorf("kind: got %q, want %q", gotKind, c.wantKind)
			}
			if gotPath != c.wantPath {
				t.Errorf("path: got %q, want %q", gotPath, c.wantPath)
			}
		})
	}
}
