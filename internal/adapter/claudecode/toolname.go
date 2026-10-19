package claudecode

import (
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// ToolName maps a Claude Code tool name to LoopGuard's normalized name
// (LLD §3.1): Bash/PowerShell → bash, Edit/MultiEdit/NotebookEdit → edit,
// Write → write, Read → read, Grep → grep, Glob → glob,
// mcp__srv__tool → mcp:srv:tool, anything else → other:<name>.
func ToolName(name string) string {
	switch name {
	case "Bash", "PowerShell":
		return event.ToolBash
	case "Edit", "MultiEdit", "NotebookEdit":
		return event.ToolEdit
	case "Write":
		return event.ToolWrite
	case "Read":
		return event.ToolRead
	case "Grep":
		return event.ToolGrep
	case "Glob":
		return event.ToolGlob
	}
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		if srv, tool, ok := strings.Cut(rest, "__"); ok {
			return "mcp:" + srv + ":" + tool
		}
	}
	return "other:" + name
}
