package transit

import "testing"

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"strings", []any{"logseq_db_Demo", "/tmp/x/db.sqlite"}, []any{"logseq_db_Demo", "/tmp/x/db.sqlite"}},
		{"bools", []any{true, false}, []any{true, false}},
		{"nil", []any{nil}, []any{nil}},
		{"numbers", []any{float64(1.5), int64(-3)}, []any{float64(1.5), int64(-3)}},
		{"nested", []any{map[string]any{"path": "/tmp/y", "force": true}},
			[]any{map[string]any{"path": "/tmp/y", "force": true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(enc)
			if err != nil {
				t.Fatalf("Decode(%q): %v", enc, err)
			}
			assertEqual(t, tc.want, got)
		})
	}
}

func TestEncodeRejectsTransitTokenLeadingStrings(t *testing.T) {
	for _, s := range []string{"~kw", "^cache", "`tick"} {
		if _, err := Encode([]any{s}); err == nil {
			t.Errorf("Encode should reject string starting with %q", s[0])
		}
	}
}

func assertEqual(t *testing.T, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			t.Fatalf("want %#v got %#v", want, got)
		}
		for i := range w {
			assertEqual(t, w[i], g[i])
		}
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			t.Fatalf("want %#v got %#v", want, got)
		}
		for k := range w {
			assertEqual(t, w[k], g[k])
		}
	case float64:
		g, ok := got.(float64)
		if !ok || g != w {
			t.Fatalf("want %#v got %#v", want, got)
		}
	case int64:
		g, ok := got.(int64)
		if !ok || g != w {
			t.Fatalf("want %#v got %#v", want, got)
		}
	default:
		if got != want {
			t.Fatalf("want %#v got %#v", want, got)
		}
	}
}
