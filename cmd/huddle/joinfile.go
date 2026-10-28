package main

// The last room joined, so the app opened from Finder rejoins it. Stage 1
// has no join screen: the first join comes from the command line
// (open Huddle.app --args app -server URL -room NAME), and later opens
// reuse it. Stage 3's sign-in and meetings list replace this.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type lastJoin struct {
	Server string `json:"server"`
	Room   string `json:"room"`
	Name   string `json:"name"`
}

func joinPath(data string) string { return filepath.Join(data, "last-join.json") }

func readLastJoin(data string) (lastJoin, bool) {
	b, err := os.ReadFile(joinPath(data))
	if err != nil {
		return lastJoin{}, false
	}
	var j lastJoin
	if json.Unmarshal(b, &j) != nil || j.Server == "" || j.Room == "" {
		return lastJoin{}, false
	}
	return j, true
}

func writeLastJoin(data string, j lastJoin) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := joinPath(data) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, joinPath(data))
}
