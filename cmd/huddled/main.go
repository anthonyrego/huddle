// Command huddled is the huddle server: the API, the meeting WebSocket and (in
// stage 1) the whiteboard agent. One binary, several subcommands; each lives in
// its own file and registers itself in init.
//
// Copied from an earlier project of the author's.
package main

import (
	"fmt"
	"os"
	"sort"
)

type command struct {
	name    string
	summary string
	run     func(args []string) error
}

var commands = map[string]*command{}

func register(c *command) {
	if _, dup := commands[c.name]; dup {
		panic("duplicate subcommand " + c.name)
	}
	commands[c.name] = c
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name, args := os.Args[1], os.Args[2:]
	if name == "help" || name == "-h" || name == "--help" {
		usage()
		return
	}
	c := commands[name]
	if c == nil {
		fmt.Fprintf(os.Stderr, "huddled: unknown subcommand %q\n\n", name)
		usage()
		os.Exit(2)
	}
	if err := c.run(args); err != nil {
		if err != errQuiet {
			fmt.Fprintf(os.Stderr, "huddled %s: %v\n", name, err)
		}
		os.Exit(1)
	}
}

// errQuiet fails a command that has already said why.
var errQuiet = fmt.Errorf("failed")

func usage() {
	fmt.Fprintln(os.Stderr, "usage: huddled <subcommand> [flags]\n\nsubcommands:")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-15s %s\n", n, commands[n].summary)
	}
}
