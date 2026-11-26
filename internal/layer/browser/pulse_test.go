package browser

import "testing"

func TestModuleIDFromLoadOutput(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"536871142", "536871142"},    // bare ID, the real format
		{"  42\n", "42"},              // surrounding whitespace
		{"", "0"},                     // empty -> no-op fallback
		{"not a number", "0"},         // garbage -> no-op fallback
		{"42: module-null-sink", "0"}, // list-format is not load output
	}
	for _, tc := range cases {
		if got := moduleIDFromLoadOutput(tc.in); got != tc.want {
			t.Errorf("moduleIDFromLoadOutput(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
