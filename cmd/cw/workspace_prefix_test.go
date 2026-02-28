package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestIsKnownCommand(t *testing.T) {
	root := &cobra.Command{Use: "cw"}
	root.AddCommand(&cobra.Command{Use: "run", Aliases: []string{}})
	root.AddCommand(&cobra.Command{Use: "list"})
	root.AddCommand(&cobra.Command{Use: "stop", Aliases: []string{"halt"}})
	root.AddCommand(&cobra.Command{Use: "orgs"})
	root.AddCommand(&cobra.Command{Use: "launch"})

	tests := []struct {
		name string
		want bool
	}{
		{"run", true},
		{"list", true},
		{"stop", true},
		{"halt", true},  // alias
		{"orgs", true},
		{"launch", true},
		{"api", false},        // workspace name
		{"my-project", false}, // workspace name
		{"help", true},        // built-in
		{"version", true},     // built-in
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isKnownCommand(root, tt.name)
			if got != tt.want {
				t.Errorf("isKnownCommand(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
