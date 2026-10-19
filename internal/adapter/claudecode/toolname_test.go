package claudecode

import "testing"

func TestToolName(t *testing.T) {
	for in, want := range map[string]string{
		"Bash":                           "bash",
		"PowerShell":                     "bash",
		"Edit":                           "edit",
		"MultiEdit":                      "edit",
		"NotebookEdit":                   "edit",
		"Write":                          "write",
		"Read":                           "read",
		"Grep":                           "grep",
		"Glob":                           "glob",
		"mcp__github__create_issue":      "mcp:github:create_issue",
		"mcp__claude_ai_Docs__read_page": "mcp:claude_ai_Docs:read_page",
		"mcp__broken":                    "other:mcp__broken",
		"WebFetch":                       "other:WebFetch",
		"TodoWrite":                      "other:TodoWrite",
	} {
		if got := ToolName(in); got != want {
			t.Errorf("ToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
