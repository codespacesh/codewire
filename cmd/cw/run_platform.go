package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/codewiresh/codewire/internal/platform"
)

func runInWorkspace(wsName, sessionName string, command []string) error {
	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}

	coderBin := cfg.CoderBinary
	if coderBin == "" {
		home, _ := os.UserHomeDir()
		coderBin = filepath.Join(home, ".config", "cw", "bin", "coder")
	}

	if _, err := os.Stat(coderBin); err != nil {
		return fmt.Errorf("coder binary not found at %s (run 'cw setup')", coderBin)
	}

	// Build the remote cw run command
	cwArgs := []string{"run"}
	if sessionName != "" {
		cwArgs = append(cwArgs, sessionName)
	}
	cwArgs = append(cwArgs, "--")
	cwArgs = append(cwArgs, command...)

	// coder ssh <workspace> -- cw run [name] -- <command>
	sshArgs := []string{"ssh", wsName, "--"}
	sshArgs = append(sshArgs, "cw")
	sshArgs = append(sshArgs, cwArgs...)

	proc := exec.Command(coderBin, sshArgs...)
	proc.Stdin = os.Stdin
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr

	if err := proc.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}
