package terminal

import (
	"os"

	"golang.org/x/term"
)

type RawModeGuard struct {
	fd       int
	oldState *term.State
}

func EnableRawMode() (*RawModeGuard, error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return &RawModeGuard{fd: fd, oldState: oldState}, nil
}

func (g *RawModeGuard) Restore() {
	term.Restore(g.fd, g.oldState)
}
