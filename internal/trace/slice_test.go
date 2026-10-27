package trace

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const testdata = "../../testdata"

var update = flag.Bool("update", false, "rewrite testdata/golden from BuildState")

// TestBuildStateGolden pins BuildState's exact output for every fixture: what
// the model reads must not change by accident. After an intended change, run
// `make golden` and review the diff.
func TestBuildStateGolden(t *testing.T) {
	traces, err := filepath.Glob(filepath.Join(testdata, "traces", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	goldens, err := filepath.Glob(filepath.Join(testdata, "golden", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) == 0 {
		t.Fatal("no trace fixtures found")
	}
	if len(traces) != len(goldens) {
		t.Fatalf("%d traces but %d golden files; run `make golden`", len(traces), len(goldens))
	}

	for _, path := range traces {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			tr, err := Decode(f)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			golden := filepath.Join(testdata, "golden", name+".txt")
			if *update {
				if err := os.WriteFile(golden, []byte(BuildState(tr)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v; run `make golden`", err)
			}
			if got := string(BuildState(tr)); got != string(want) {
				t.Errorf("BuildState differs from testdata/golden at byte %d\n--- got\n%s\n--- want\n%s",
					firstDiff(got, string(want)), got, want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"日本語です", 3, "日本語"},
		{"a😀b😀c", 2, "a😀"},
		{"éé", 5, "éé"},
	}
	for _, tt := range tests {
		got := truncateRunes(tt.in, tt.n)
		if got != tt.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncateRunes(%q, %d) split a code point", tt.in, tt.n)
		}
	}
}

func TestCompact(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"query": "capital of France", "limit": 3}`, `{"query":"capital of France","limit":3}`},
		{`{"b": 1, "a": 2}`, `{"b":1,"a":2}`}, // key order kept
		{`[1, [2, {"x": null}], true]`, `[1,[2,{"x":null}],true]`},
		{`1.5e3`, `1.5e3`},                   // numbers as written
		{`"caf\u00e9 \n"`, `"caf\u00e9 \n"`}, // escapes as written
	}
	for _, tt := range tests {
		if got := compact([]byte(tt.in)); got != tt.want {
			t.Errorf("compact(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func firstDiff(a, b string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
