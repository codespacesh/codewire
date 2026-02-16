package terminal

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

func TerminalSize() (cols, rows uint16, err error) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0, err
	}
	return uint16(w), uint16(h), nil
}

// ResizeSignal returns a channel that fires on SIGWINCH and a cleanup function.
func ResizeSignal() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch); close(ch) }
}
