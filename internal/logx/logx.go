// Package logx sets up logging for each subcommand. The detached app has no
// stderr anyone reads, so everything goes to a file under the log directory,
// and Go runtime crashes (panics, fatal errors) are written there too.
//
// Copied from an earlier project of the author's.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
)

// Setup opens <dir>/<name>.log for appending, makes it the default slog
// destination and the runtime crash output. When alsoStderr is set, logs are
// mirrored to stderr (the shim's stderr is captured by Claude Code).
// The returned file stays open for the life of the process.
func Setup(dir, name string, alsoStderr bool) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		fmt.Fprintf(f, "logx: SetCrashOutput: %v\n", err)
	}
	var w io.Writer = f
	if alsoStderr {
		w = io.MultiWriter(f, os.Stderr)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("start", "cmd", name, "pid", os.Getpid(), "args", os.Args[1:])
	return f, nil
}
