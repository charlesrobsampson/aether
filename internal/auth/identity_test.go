package auth

import "testing"

func TestValidIdentity(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "phone", value: "+18016739997", want: true},
		{name: "short phone", value: "+1", want: true},
		{name: "uuid", value: "2aea5c40-0f8e-4ad7-8099-880b50064454", want: true},
		{name: "uppercase uuid", value: "2AEA5C40-0F8E-4AD7-8099-880B50064454", want: true},
		{name: "empty", value: "", want: false},
		{name: "bare digits", value: "18016739997", want: false},
		{name: "path traversal", value: "../admin", want: false},
		{name: "phone with letters", value: "+1801abc9997", want: false},
		{name: "truncated uuid", value: "2aea5c40-0f8e-4ad7-8099", want: false},
		{name: "uuid with bad hex", value: "2aea5c40-0f8e-4ad7-8099-880b5006445z", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidIdentity(tc.value); got != tc.want {
				t.Fatalf("ValidIdentity(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
