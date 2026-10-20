// Command loopguard-hook is the program agents run for every hook event.
// It does only what `loopguard hook` does (LLD §10): forward the payload on
// stdin to the LoopGuard daemon, print its decision, start the daemon when
// it isn't running, and fail open. It is a separate, minimal binary
// because it runs on every tool call: no CLI framework, and none of the
// daemon's code, so it starts as fast as a Go program can.
//
//	loopguard-hook [--agent claude-code|codex]
//	loopguard-hook --version
//
// It starts the daemon from the loopguard executable in the same
// directory.
package main

import (
	"os"
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/shim"
)

// Set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	agent, showVersion := parseArgs(os.Args[1:])
	if showVersion {
		os.Stdout.WriteString("loopguard-hook " + version + "\n")
		return
	}
	os.Exit(shim.Run(shim.Options{Agent: agent, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}

// parseArgs reads --agent X (or --agent=X) and --version. Anything else is
// ignored, so a newer installed hook config never breaks an older binary.
func parseArgs(args []string) (agent string, version bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--version" || a == "-version":
			version = true
		case a == "--agent" || a == "-agent":
			if i+1 < len(args) {
				agent = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--agent="):
			agent = strings.TrimPrefix(a, "--agent=")
		}
	}
	return agent, version
}
