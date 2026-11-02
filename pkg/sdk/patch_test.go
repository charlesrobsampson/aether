package sdk

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApplyPatch(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		ops     []Operation
		want    string // empty means the patch must fail
		wantErr string // substring the error must contain
	}{
		{
			name: "add object member",
			doc:  `{"a":1}`,
			ops:  []Operation{{Op: "add", Path: "/b", Value: 2}},
			want: `{"a":1,"b":2}`,
		},
		{
			name: "add replaces an existing object member",
			doc:  `{"a":1}`,
			ops:  []Operation{{Op: "add", Path: "/a", Value: 2}},
			want: `{"a":2}`,
		},
		{
			name: "add inserts an array element",
			doc:  `{"a":["x","y"]}`,
			ops:  []Operation{{Op: "add", Path: "/a/1", Value: "m"}},
			want: `{"a":["x","m","y"]}`,
		},
		{
			name: "add appends with a dash",
			doc:  `{"a":["x"]}`,
			ops:  []Operation{{Op: "add", Path: "/a/-", Value: "y"}},
			want: `{"a":["x","y"]}`,
		},
		{
			name: "add null",
			doc:  `{}`,
			ops:  []Operation{{Op: "add", Path: "/a", Value: nil}},
			want: `{"a":null}`,
		},
		{
			name: "replace object member",
			doc:  `{"a":1,"b":2}`,
			ops:  []Operation{{Op: "replace", Path: "/b", Value: 3}},
			want: `{"a":1,"b":3}`,
		},
		{
			name: "replace array element",
			doc:  `{"a":["x","y"]}`,
			ops:  []Operation{{Op: "replace", Path: "/a/0", Value: "z"}},
			want: `{"a":["z","y"]}`,
		},
		{
			name: "remove object member",
			doc:  `{"a":1,"b":2}`,
			ops:  []Operation{{Op: "remove", Path: "/a"}},
			want: `{"b":2}`,
		},
		{
			name: "remove array element",
			doc:  `{"a":["x","y","z"]}`,
			ops:  []Operation{{Op: "remove", Path: "/a/1"}},
			want: `{"a":["x","z"]}`,
		},
		{
			name: "test passes",
			doc:  `{"a":{"b":[1,2]},"c":"x"}`,
			ops:  []Operation{{Op: "test", Path: "/a", Value: map[string]any{"b": []any{1, 2}}}},
			want: `{"a":{"b":[1,2]},"c":"x"}`,
		},
		{
			name:    "test fails on value",
			doc:     `{"a":1}`,
			ops:     []Operation{{Op: "test", Path: "/a", Value: 2}},
			wantErr: "test failed",
		},
		{
			name:    "test fails on missing path",
			doc:     `{"a":1}`,
			ops:     []Operation{{Op: "test", Path: "/b", Value: 1}},
			wantErr: "not found",
		},
		{
			name:    "replace missing path",
			doc:     `{"a":1}`,
			ops:     []Operation{{Op: "replace", Path: "/b", Value: 2}},
			wantErr: "not found",
		},
		{
			name:    "remove missing path",
			doc:     `{"a":1}`,
			ops:     []Operation{{Op: "remove", Path: "/b"}},
			wantErr: "not found",
		},
		{
			name:    "remove array element out of range",
			doc:     `{"a":["x"]}`,
			ops:     []Operation{{Op: "remove", Path: "/a/1"}},
			wantErr: "out of range",
		},
		{
			name:    "add through a missing parent",
			doc:     `{"a":{}}`,
			ops:     []Operation{{Op: "add", Path: "/b/c", Value: 1}},
			wantErr: "not found",
		},
		{
			name:    "invalid pointer without a leading slash",
			doc:     `{"a":1}`,
			ops:     []Operation{{Op: "replace", Path: "a", Value: 2}},
			wantErr: "invalid JSON pointer",
		},
		{
			name:    "invalid pointer escape",
			doc:     `{"a":1}`,
			ops:     []Operation{{Op: "replace", Path: "/a/~2", Value: 2}},
			wantErr: "invalid JSON pointer",
		},
		{
			name:    "unsupported operation",
			doc:     `{"a":1}`,
			ops:     []Operation{{Op: "move", Path: "/a", Value: 2}},
			wantErr: "unsupported op",
		},
		{
			name:    "invalid document",
			doc:     `{`,
			ops:     []Operation{{Op: "add", Path: "/a", Value: 1}},
			wantErr: "invalid patch document",
		},
		{
			name: "replace the whole document through the empty pointer",
			doc:  `{"a":1}`,
			ops:  []Operation{{Op: "replace", Path: "", Value: map[string]any{"b": 2}}},
			want: `{"b":2}`,
		},
		{
			name: "escaped pointer tokens",
			doc:  `{"a/b":{"m~n":1}}`,
			ops:  []Operation{{Op: "replace", Path: "/a~1b/m~0n", Value: 2}},
			want: `{"a/b":{"m~n":2}}`,
		},
		{
			name: "multiple operations apply in order",
			doc:  `{"a":1}`,
			ops: []Operation{
				{Op: "add", Path: "/b", Value: 2},
				{Op: "replace", Path: "/a", Value: 3},
				{Op: "remove", Path: "/b"},
			},
			want: `{"a":3}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply([]byte(tt.doc), tt.ops)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Apply() = %s, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Apply() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if !jsonEqual(t, got, []byte(tt.want)) {
				t.Fatalf("Apply() = %s, want %s", got, tt.want)
			}
		})
	}
}

// jsonEqual compares two JSON documents semantically so key order and
// whitespace do not matter.
func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()

	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("unmarshal Apply output %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("unmarshal expected output %q: %v", want, err)
	}
	gotJSON, err := json.Marshal(gotValue)
	if err != nil {
		t.Fatalf("marshal Apply output: %v", err)
	}
	wantJSON, err := json.Marshal(wantValue)
	if err != nil {
		t.Fatalf("marshal expected output: %v", err)
	}
	return string(gotJSON) == string(wantJSON)
}
