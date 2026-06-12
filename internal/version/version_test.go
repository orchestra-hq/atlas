package version

import "testing"

func TestString(t *testing.T) {
	restore := func(v, c, d string) {
		Version, Commit, Date = v, c, d
	}
	defer restore(Version, Commit, Date)

	tests := []struct {
		name                  string
		version, commit, date string
		want                  string
	}{
		{"dev build", "dev", "", "", "dev"},
		{"stamped release", "0.1.0", "c019bb3", "2026-06-12", "0.1.0 (c019bb3, 2026-06-12)"},
		{"commit without date", "0.1.0", "c019bb3", "", "0.1.0 (c019bb3)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore(tt.version, tt.commit, tt.date)
			if got := String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
