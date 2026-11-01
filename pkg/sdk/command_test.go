package sdk

import (
	"maps"
	"testing"
)

func TestMatchCommand(t *testing.T) {
	tests := []struct {
		name  string
		spec  CommandSpec
		input string
		want  map[string]string
		ok    bool
	}{
		{
			name:  "relative reminder",
			spec:  CommandSpec{Name: "remind", Patterns: []string{"remind me <msg> in <num> <unit>"}},
			input: "remind me call bob in 5 minutes",
			want:  map[string]string{"msg": "call bob", "num": "5", "unit": "minutes"},
			ok:    true,
		},
		{
			name:  "absolute reminder",
			spec:  CommandSpec{Name: "remind", Patterns: []string{"remind <msg> at <when>"}},
			input: "remind take pills at 2026-11-01 07:30",
			want:  map[string]string{"msg": "take pills", "when": "2026-11-01 07:30"},
			ok:    true,
		},
		{
			name:  "multi-word arg is lazy",
			spec:  CommandSpec{Name: "remind", Patterns: []string{"remind me <msg> in <num> <unit>"}},
			input: "remind me buy milk and eggs in 2 hours",
			want:  map[string]string{"msg": "buy milk and eggs", "num": "2", "unit": "hours"},
			ok:    true,
		},
		{
			name:  "declaration order wins",
			spec:  CommandSpec{Name: "remind", Patterns: []string{"remind <msg>", "remind <msg> at <when>"}},
			input: "remind call bob at noon",
			want:  map[string]string{"msg": "call bob at noon"},
			ok:    true,
		},
		{
			name:  "no match",
			spec:  CommandSpec{Name: "remind", Patterns: []string{"remind me <msg>"}},
			input: "fly FPS",
			ok:    false,
		},
		{
			name:  "trigger is part of the pattern",
			spec:  CommandSpec{Name: "fly", Patterns: []string{"fly <station>"}},
			input: "FPS",
			ok:    false,
		},
		{
			name:  "trigger matches when included",
			spec:  CommandSpec{Name: "fly", Patterns: []string{"fly <station>"}},
			input: "fly FPS",
			want:  map[string]string{"station": "FPS"},
			ok:    true,
		},
		{
			name:  "whitespace runs match",
			spec:  CommandSpec{Name: "remind", Patterns: []string{"remind me <msg> in <num> <unit>"}},
			input: "remind   me call bob in\t5\nminutes",
			want:  map[string]string{"msg": "call bob", "num": "5", "unit": "minutes"},
			ok:    true,
		},
		{
			name:  "pattern is anchored",
			spec:  CommandSpec{Name: "test", Patterns: []string{"test"}},
			input: "test extra",
			ok:    false,
		},
		{
			name:  "regex metacharacters are literal",
			spec:  CommandSpec{Name: "math", Patterns: []string{"what is 2+2?"}},
			input: "what is 2+2?",
			want:  map[string]string{},
			ok:    true,
		},
		{
			name:  "invalid arg name never matches",
			spec:  CommandSpec{Name: "bad", Patterns: []string{"bad <1x>"}},
			input: "bad value",
			ok:    false,
		},
		{
			name:  "empty pattern list",
			spec:  CommandSpec{Name: "empty"},
			input: "anything",
			ok:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MatchCommand(tt.input, tt.spec)
			if ok != tt.ok {
				t.Fatalf("MatchCommand(%q) ok = %v, want %v (args %v)", tt.input, ok, tt.ok, got)
			}
			if !maps.Equal(got, tt.want) {
				t.Fatalf("MatchCommand(%q) args = %v, want %v", tt.input, got, tt.want)
			}
		})
	}

	t.Run("regexes are compiled once per spec", func(t *testing.T) {
		patterns := []string{"fly <station>"}
		first := compileSpec(patterns)
		second := compileSpec(patterns)
		if first != second {
			t.Fatal("compileSpec returned a different compiled spec for identical patterns")
		}
	})
}
