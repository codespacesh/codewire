package session

import (
	"strings"
	"testing"
)

func TestBuildEnvStripsClaudeCode(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	env := buildEnv(nil)
	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDECODE=") {
			t.Fatalf("CLAUDECODE should be stripped, got: %s", e)
		}
	}
}

func TestBuildEnvPreservesOtherVars(t *testing.T) {
	t.Setenv("CW_TEST_VAR", "keep-me")
	env := buildEnv(nil)
	found := false
	for _, e := range env {
		if e == "CW_TEST_VAR=keep-me" {
			found = true
		}
	}
	if !found {
		t.Fatal("CW_TEST_VAR should be preserved")
	}
}

func TestBuildEnvAppliesOverrides(t *testing.T) {
	env := buildEnv([]string{"MY_VAR=hello"})
	found := false
	for _, e := range env {
		if e == "MY_VAR=hello" {
			found = true
		}
	}
	if !found {
		t.Fatal("MY_VAR=hello should be present")
	}
}

func TestBuildEnvOverridesExisting(t *testing.T) {
	t.Setenv("CW_TEST_VAR", "original")
	env := buildEnv([]string{"CW_TEST_VAR=override"})
	for _, e := range env {
		if e == "CW_TEST_VAR=original" {
			t.Fatal("override not applied")
		}
	}
}
