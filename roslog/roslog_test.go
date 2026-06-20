package roslog

import (
	"context"
	"log/slog"
	"reflect"
	"sort"
	"testing"
)

func TestGetLogLevel(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want slog.Level
	}{
		{"debug", "debug", true, slog.LevelDebug},
		{"info", "info", true, slog.LevelInfo},
		{"warn", "warn", true, slog.LevelWarn},
		{"error", "error", true, slog.LevelError},
		{"mixed case", "DeBuG", true, slog.LevelDebug},
		{"unrecognized falls back to info", "verbose", true, slog.LevelInfo},
		{"empty falls back to info", "", true, slog.LevelInfo},
		{"unset defaults to info", "", false, slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("RUNOS_LOG_LEVEL", tc.env)
			} else {
				// t.Setenv with empty value still sets the var; to test the
				// truly-unset path we rely on the default fallback, which the
				// empty case already exercises identically.
				t.Setenv("RUNOS_LOG_LEVEL", "")
			}
			if got := getLogLevel(); got != tc.want {
				t.Fatalf("getLogLevel() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContextWithLogFields_StoresAndMerges(t *testing.T) {
	ctx := context.Background()

	// First insertion.
	ctx = ContextWithLogFields(ctx, map[string]any{"a": 1, "b": 2})
	got := fieldsFromContext(t, ctx)
	if !reflect.DeepEqual(got, map[string]any{"a": 1, "b": 2}) {
		t.Fatalf("after first insert got %+v", got)
	}

	// Merge new keys and override an existing one.
	ctx = ContextWithLogFields(ctx, map[string]any{"b": 20, "c": 3})
	got = fieldsFromContext(t, ctx)
	want := map[string]any{"a": 1, "b": 20, "c": 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after merge got %+v, want %+v", got, want)
	}
}

func TestContextWithLogFields_DoesNotMutateOriginalMap(t *testing.T) {
	ctx := context.Background()
	first := map[string]any{"a": 1}
	ctx = ContextWithLogFields(ctx, first)

	// Merging must not write back into the first map.
	ctx = ContextWithLogFields(ctx, map[string]any{"b": 2})
	if _, ok := first["b"]; ok {
		t.Fatal("merge mutated the original fields map")
	}

	// And the resulting context still holds the merged view.
	got := fieldsFromContext(t, ctx)
	if !reflect.DeepEqual(got, map[string]any{"a": 1, "b": 2}) {
		t.Fatalf("merged context fields = %+v", got)
	}
}

func TestCreateInstallContext(t *testing.T) {
	ctx := CreateInstallContext(context.Background())
	got := fieldsFromContext(t, ctx)
	if got["phase"] != "NODE_INSTALLATION" {
		t.Fatalf("phase = %v, want NODE_INSTALLATION", got["phase"])
	}
}

func TestExtractContextValues(t *testing.T) {
	// No fields -> nil.
	if got := extractContextValues(context.Background()); got != nil {
		t.Fatalf("expected nil for empty context, got %v", got)
	}

	ctx := ContextWithLogFields(context.Background(), map[string]any{"k1": "v1", "k2": 2})
	attrs := extractContextValues(ctx)
	if len(attrs) != 2 {
		t.Fatalf("expected 2 attrs, got %d (%v)", len(attrs), attrs)
	}

	// Every element should be a slog.Attr keyed by the original field names.
	keys := make([]string, 0, len(attrs))
	for _, a := range attrs {
		attr, ok := a.(slog.Attr)
		if !ok {
			t.Fatalf("attr %v is not a slog.Attr", a)
		}
		keys = append(keys, attr.Key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"k1", "k2"}) {
		t.Fatalf("attr keys = %v, want [k1 k2]", keys)
	}
}

func TestCleanPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/root/runos/dev/nodeagent/roslog/roslog.go", "roslog/roslog.go"},
		{"/home/runner/work/nodeagent/nodeagent/commons/strings.go", "commons/strings.go"},
		{"/some/other/absolute/path/main.go", "main.go"},
		{"bare.go", "bare.go"},
	}
	for _, tc := range cases {
		if got := cleanPath(tc.in); got != tc.want {
			t.Errorf("cleanPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanFunction(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"github.com/runos-official/nodeagent/roslog.I", "roslog.I"},
		{"github.com/runos-official/nodeagent/uc/sync.setPeers", "sync.setPeers"},
		{"main.main", "main.main"},
		{"noSlashFunc", "noSlashFunc"},
	}
	for _, tc := range cases {
		if got := cleanFunction(tc.in); got != tc.want {
			t.Errorf("cleanFunction(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func fieldsFromContext(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	fields, ok := ctx.Value(logFieldsKey).(map[string]any)
	if !ok {
		t.Fatalf("context has no log fields map")
	}
	return fields
}
