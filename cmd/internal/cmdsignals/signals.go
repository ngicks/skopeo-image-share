package cmdsignals

import (
	"os"
	"syscall"
)

// ExitSignals are the signals that should cancel top-level CLI execution.
var ExitSignals = [...]os.Signal{
	os.Interrupt,
	syscall.SIGTERM,
}
