package build

import "testing"

func TestExpandArgs(t *testing.T) {
	vars := map[string]string{"VER": "1.0", "EMPTY": ""}
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "v$VER", want: "v1.0"},
		{in: "v${VER}w", want: "v1.0w"},
		{in: "$UNKNOWN", want: ""},
		{in: "${UNKNOWN}", want: ""},
		{in: "${VER:-2.0}", want: "1.0"},
		{in: "${MISSING:-2.0}", want: "2.0"},
		{in: "${VER:-}", want: "1.0"},
		{in: "${MISSING:-}", want: ""},
		{in: "${EMPTY:-x}", want: "x"},
		{in: "${EMPTY-x}", want: ""},
		{in: "${MISSING-x}", want: "x"},
		{in: "${VER:+alt}", want: "alt"},
		{in: "${EMPTY:+alt}", want: ""},
		{in: "${MISSING:+alt}", want: ""},
		{in: "${MISSING+alt}", want: ""},
		{in: "${MISSING:-a b}", want: "a b"},
		{in: "costs $$5", want: "costs $5"},
		{in: "$5", want: "$5"},
		{in: "trailing $", want: "trailing $"},
		{in: "$VER$VER", want: "1.01.0"},
		{in: "${VER", wantErr: true},
		{in: "${}", wantErr: true},
		{in: "${9x}", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ExpandArgs(tt.in, vars)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ExpandArgs(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ExpandArgs(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ExpandArgs(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
