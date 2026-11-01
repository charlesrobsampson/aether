package sdk

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConvertArgs(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}

	spec := CommandSpec{
		Name:     "remind",
		Patterns: []string{"remind me <msg> in <num> <unit>"},
		Args: []ArgDef{
			{Name: "msg", Type: "string"},
			{Name: "num", Type: "int"},
			{Name: "unit", Type: "enum", Enum: []string{"seconds", "minutes", "hours", "days"}},
			{Name: "when", Type: "datetime"},
		},
	}

	t.Run("converts every supported type", func(t *testing.T) {
		raw := map[string]string{
			"msg":  "  call bob  ",
			"num":  "5",
			"unit": "minutes",
			"when": "2026-06-15 07:30",
		}
		want := map[string]any{
			"msg":  "call bob",
			"num":  5,
			"unit": "minutes",
			"when": time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC),
		}

		got, err := ConvertArgs(spec, raw, chicago)
		if err != nil {
			t.Fatalf("ConvertArgs: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ConvertArgs = %#v, want %#v", got, want)
		}
	})

	t.Run("rejects enum outside the declared set", func(t *testing.T) {
		raw := map[string]string{
			"msg":  "call bob",
			"num":  "5",
			"unit": "fortnights",
			"when": "2026-06-15 07:30",
		}
		_, err := ConvertArgs(spec, raw, chicago)
		if err == nil {
			t.Fatal("ConvertArgs accepted an enum value outside the declared set")
		}
		if !strings.Contains(err.Error(), "unit") {
			t.Fatalf("error = %q, want it to name the offending argument", err)
		}
	})

	t.Run("rejects a non-integer int", func(t *testing.T) {
		raw := map[string]string{
			"msg":  "call bob",
			"num":  "five",
			"unit": "minutes",
			"when": "2026-06-15 07:30",
		}
		if _, err := ConvertArgs(spec, raw, chicago); err == nil {
			t.Fatal("ConvertArgs accepted a non-integer int argument")
		}
	})

	t.Run("rejects a missing argument", func(t *testing.T) {
		raw := map[string]string{
			"msg":  "call bob",
			"unit": "minutes",
			"when": "2026-06-15 07:30",
		}
		_, err := ConvertArgs(spec, raw, chicago)
		if err == nil {
			t.Fatal("ConvertArgs accepted a missing argument")
		}
		if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "num") {
			t.Fatalf("error = %q, want it to report the missing argument", err)
		}
	})

	t.Run("rejects an unknown argument type", func(t *testing.T) {
		bad := CommandSpec{Args: []ArgDef{{Name: "x", Type: "float"}}}
		if _, err := ConvertArgs(bad, map[string]string{"x": "1.5"}, chicago); err == nil {
			t.Fatal("ConvertArgs accepted an unknown argument type")
		}
	})

	t.Run("rejects a nil location", func(t *testing.T) {
		if _, err := ConvertArgs(spec, map[string]string{
			"msg": "call bob", "num": "5", "unit": "minutes", "when": "2026-06-15 07:30",
		}, nil); err == nil {
			t.Fatal("ConvertArgs accepted a nil location")
		}
	})

	t.Run("empty spec returns an empty map", func(t *testing.T) {
		got, err := ConvertArgs(CommandSpec{}, nil, chicago)
		if err != nil {
			t.Fatalf("ConvertArgs: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ConvertArgs = %#v, want an empty map", got)
		}
	})
}
