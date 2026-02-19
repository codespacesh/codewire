package node

import "testing"

func TestStripANSI(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"plain text", "hello world", "hello world"},
		{"CSI color", "\x1b[32mgreen\x1b[0m text", "green text"},
		{"CSI cursor move", "\x1b[2J\x1b[H", ""},
		{"OSC title BEL", "\x1b]0;title\x07rest", "rest"},
		{"OSC title ST", "\x1b]0;title\x1b\\rest", "rest"},
		{"nested codes", "\x1b[1;32mBold\x1b[0m normal", "Bold normal"},
		{"preserves newlines", "line1\nline2\n", "line1\nline2\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripANSI(tc.input)
			if got != tc.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
