// Package config resolves where huddle keeps its data and logs.
//
// HUDDLE_DATA_DIR overrides the data directory. Dev builds and each worktree
// set their own so development never migrates the database the installed app is
// using. When it is set, logs go under it too.
//
// Copied from an earlier project of the author's.
package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// Paths are the directories huddle writes to.
type Paths struct {
	Data string // SQLite database, font index cache
	Logs string
}

// Resolve returns the paths for this process, honoring HUDDLE_DATA_DIR.
func Resolve() (Paths, error) {
	if d := os.Getenv("HUDDLE_DATA_DIR"); d != "" {
		abs, err := filepath.Abs(d)
		if err != nil {
			return Paths{}, err
		}
		return Paths{Data: abs, Logs: filepath.Join(abs, "logs")}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	switch runtime.GOOS {
	case "darwin":
		return Paths{
			Data: filepath.Join(home, "Library", "Application Support", "Huddle"),
			Logs: filepath.Join(home, "Library", "Logs", "Huddle"),
		}, nil
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		return Paths{Data: filepath.Join(base, "Huddle"), Logs: filepath.Join(base, "Huddle", "logs")}, nil
	default:
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			state = filepath.Join(home, ".local", "state")
		}
		return Paths{Data: filepath.Join(data, "huddle"), Logs: filepath.Join(state, "huddle", "logs")}, nil
	}
}

// Ensure creates the directories.
func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.Data, 0o755); err != nil {
		return err
	}
	return os.MkdirAll(p.Logs, 0o755)
}
