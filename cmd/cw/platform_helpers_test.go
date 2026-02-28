package main

import "testing"

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Acme Corp", "acme-corp"},
		{"My Cool Project", "my-cool-project"},
		{"  hello  world  ", "hello-world"},
		{"foo--bar", "foo-bar"},
		{"UPPER CASE", "upper-case"},
		{"special!@#chars", "special-chars"},
		{"-leading-trailing-", "leading-trailing"},
		{"a", "a"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := slugify(tt.input)
			if got != tt.want {
				t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
