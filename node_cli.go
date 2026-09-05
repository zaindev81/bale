package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"bale/internal/nodeversion"
)

const nodeUsage = `bale - a Node.js version manager

Usage:
  bale list-remote [version]  List available versions for this platform
  bale install <version>     Install Node.js (e.g. 24, 24.10, v24.10.0, latest, lts)
  bale i <version>           Alias for install
  bale use <version>         Select an installed version (number or latest)
  bale list                  List installed versions; * marks the selected version
  bale ls                    Alias for list
  bale env                   Print PATH setup for sh, bash, or zsh
  bale pkg <command>         Manage npm packages (see bale pkg help)
  bale help                  Show this help

Setup (once per shell, or add to your shell startup file):
  eval "$(./bale env)"

Environment:
  BALE_HOME   Installation directory (default ~/.bale)

The selected version is shared by shells using the same BALE_HOME.
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, nodeUsage)
		return 0
	}
	if args[0] == "pkg" {
		return runPackages(args[1:], stdout, stderr)
	}
	cmd, rest := args[0], args[1:]
	if len(rest) == 1 && (rest[0] == "--help" || rest[0] == "-h") {
		fmt.Fprint(stdout, nodeUsage)
		return 0
	}
	valid := false
	switch cmd {
	case "install", "i", "use":
		valid = len(rest) == 1
	case "list-remote":
		valid = len(rest) <= 1
	case "list", "ls", "env":
		valid = len(rest) == 0
	}
	if !valid {
		fmt.Fprintf(stderr, "bale: invalid command or arguments: %s\n%s", strings.Join(args, " "), nodeUsage)
		return 2
	}
	m, err := nodeversion.New(os.Getenv("BALE_HOME"))
	if err != nil {
		fmt.Fprintf(stderr, "bale: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "env":
		// Single quoting also handles paths containing spaces, dollars, and backticks.
		bin := strings.ReplaceAll(m.Home+"/current/bin", "'", "'\"'\"'")
		fmt.Fprintf(stdout, "export PATH='%s':\"$PATH\"\nhash -r\n", bin)
	case "list-remote":
		selector := ""
		if len(rest) != 0 {
			selector = rest[0]
		}
		var releases []nodeversion.Release
		releases, err = m.Remote(ctx, selector)
		if err == nil {
			for _, release := range releases {
				fmt.Fprint(stdout, release.Version)
				if lts := release.LTSName(); lts != "" {
					fmt.Fprintf(stdout, " (LTS: %s)", lts)
				}
				fmt.Fprintln(stdout)
			}
		}
	case "install", "i":
		var version string
		fmt.Fprintf(stderr, "Resolving Node.js %s...\n", rest[0])
		version, err = m.Install(ctx, rest[0])
		if err == nil {
			fmt.Fprintf(stdout, "Installed Node.js %s. Run bale use %s to select it.\n", version, version)
		}
	case "use":
		var version string
		version, err = m.Use(rest[0])
		if err == nil {
			fmt.Fprintf(stdout, "Selected Node.js %s.\nIf PATH is not configured, run: eval \"$(./bale env)\"\n", version)
		}
	case "list", "ls":
		var versions []string
		versions, err = m.Installed()
		if err == nil {
			current := m.Current()
			if len(versions) == 0 {
				fmt.Fprintln(stdout, "No Node.js versions installed. Run bale list-remote, then bale install <version>.")
			}
			for _, v := range versions {
				mark := " "
				if v == current {
					mark = "*"
				}
				fmt.Fprintf(stdout, "%s %s\n", mark, v)
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "bale: %v\n", err)
		return 1
	}
	return 0
}
