package tests

import (
	"testing"

	"github.com/codespacesh/codewire/internal/config"
	"github.com/codespacesh/codewire/internal/fleet"
)

func TestValidateNodeNameValid(t *testing.T) {
	valid := []string{"my-node", "node_1", "gpu-box", "a"}
	for _, name := range valid {
		if err := config.ValidateNodeName(name); err != nil {
			t.Errorf("ValidateNodeName(%q) should pass, got error: %v", name, err)
		}
	}
}

func TestValidateNodeNameInvalid(t *testing.T) {
	invalid := []string{"", "my.node", "my node", "my*node", "my>node"}
	for _, name := range invalid {
		if err := config.ValidateNodeName(name); err == nil {
			t.Errorf("ValidateNodeName(%q) should fail, got nil error", name)
		}
	}
}

func TestParseFleetTargetValid(t *testing.T) {
	nodeName, sessionID, err := fleet.ParseFleetTarget("gpu-box:42")
	if err != nil {
		t.Fatalf("ParseFleetTarget should pass, got error: %v", err)
	}
	if nodeName != "gpu-box" {
		t.Errorf("expected nodeName 'gpu-box', got %q", nodeName)
	}
	if sessionID != 42 {
		t.Errorf("expected sessionID 42, got %d", sessionID)
	}
}

func TestParseFleetTargetInvalid(t *testing.T) {
	invalid := []string{"no-colon", "node:abc"}
	for _, target := range invalid {
		_, _, err := fleet.ParseFleetTarget(target)
		if err == nil {
			t.Errorf("ParseFleetTarget(%q) should fail, got nil error", target)
		}
	}
}
