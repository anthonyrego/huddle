//go:build unix

package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

// ReserveStdout hands the process's real stdout to the caller and points fd 1
// at stderr. Anything that prints to stdout afterwards (a stray fmt.Println,
// SDL, copied code) lands on stderr instead of corrupting a JSON-RPC stream.
// Call it first thing in any subcommand whose stdout is a protocol.
func ReserveStdout() (*os.File, error) {
	fd, err := unix.Dup(1)
	if err != nil {
		return nil, err
	}
	if err := unix.Dup2(2, 1); err != nil {
		unix.Close(fd)
		return nil, err
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "protocol-stdout"), nil
}
