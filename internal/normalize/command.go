package normalize

import (
	"bytes"
	"regexp"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"
)

// Command is a parsed bash command line.
type Command struct {
	// Canonical is the command re-printed by the shell printer and run
	// through Text, so formatting differences ("npm  test") disappear. It
	// is the text fingerprinted for bash actions.
	Canonical string
	// Commands holds the words of each real command, in order (LLD §4.3:
	// the first real command of each pipeline, every element of a list).
	Commands [][]string
	// Families is the family of each entry in Commands.
	Families []string
}

// Primary returns the family of the last real command, which is usually
// the one whose result matters, or "" if there is none.
func (c Command) Primary() string {
	if len(c.Families) == 0 {
		return ""
	}
	return c.Families[len(c.Families)-1]
}

// PrimaryWords returns the words of the last real command.
func (c Command) PrimaryWords() []string {
	if len(c.Commands) == 0 {
		return nil
	}
	return c.Commands[len(c.Commands)-1]
}

// skipCommands are not "real" commands: they set up the shell rather than
// produce the result that matters.
var skipCommands = map[string]bool{
	"cd": true, "pushd": true, "popd": true, "export": true, "source": true, ".": true,
	"set": true, "unset": true, "alias": true, "true": true, ":": true, "local": true,
	"declare": true, "typeset": true, "readonly": true, "shopt": true, "trap": true,
}

// wrapperCommands run the rest of their arguments as the real command.
var wrapperCommands = map[string]bool{
	"time": true, "env": true, "command": true, "exec": true, "nohup": true,
	"sudo": true, "nice": true, "timeout": true, "xargs": true,
}

// scriptRunners take a script name after "run" that is part of the family
// ("npm run build" and "npm run test" are different families).
var scriptRunners = map[string]bool{"npm": true, "yarn": true, "pnpm": true, "bun": true}

// envRunners run another command after "run"; the family is that of the
// inner command ("uv run pytest" → "pytest").
var envRunners = map[string]bool{"uv": true, "poetry": true, "pipenv": true, "hatch": true, "pdm": true}

var subcommandRe = regexp.MustCompile(`^[a-z][a-z0-9:_-]*$`)

// parsers pools shell parsers, which are not safe for concurrent use.
var parsers = sync.Pool{New: func() any { return syntax.NewParser(syntax.Variant(syntax.LangBash)) }}

func parseShell(src string) (*syntax.File, error) {
	p := parsers.Get().(*syntax.Parser)
	defer parsers.Put(p)
	return p.Parse(strings.NewReader(src), "")
}

// ParseCommand parses a bash command line. It never fails: when the line
// is not valid bash (PowerShell, a truncated heredoc), it falls back to
// splitting on whitespace.
func ParseCommand(cmd string) Command {
	if len(cmd) > MaxInput {
		cmd = cmd[:MaxInput]
	}
	f, err := parseShell(cmd)
	if err != nil {
		return fallbackCommand(cmd)
	}
	var out Command
	var buf bytes.Buffer
	if err := syntax.NewPrinter(syntax.SingleLine(true)).Print(&buf, f); err != nil {
		return fallbackCommand(cmd)
	}
	out.Canonical = Text(buf.String())
	collectStmts(f.Stmts, &out, 0)
	return out
}

func fallbackCommand(cmd string) Command {
	c := Command{Canonical: Text(cmd)}
	words := strings.Fields(c.Canonical)
	if len(words) > 0 {
		c.add(words, 0)
	}
	return c
}

// maxDepth bounds recursion into `bash -c '…'` and nested blocks.
const maxDepth = 4

func collectStmts(stmts []*syntax.Stmt, out *Command, depth int) {
	for _, st := range stmts {
		collectStmt(st, out, depth)
	}
}

func collectStmt(st *syntax.Stmt, out *Command, depth int) {
	if st == nil || depth > maxDepth {
		return
	}
	switch c := st.Cmd.(type) {
	case *syntax.CallExpr:
		words := make([]string, 0, len(c.Args))
		for _, w := range c.Args {
			words = append(words, wordString(w))
		}
		if len(words) > 0 {
			out.add(words, depth)
		}
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.Pipe, syntax.PipeAll:
			collectStmt(c.X, out, depth) // the producer is what matters
		default:
			collectStmt(c.X, out, depth)
			collectStmt(c.Y, out, depth)
		}
	case *syntax.Block:
		collectStmts(c.Stmts, out, depth)
	case *syntax.Subshell:
		collectStmts(c.Stmts, out, depth)
	case *syntax.TimeClause:
		collectStmt(c.Stmt, out, depth)
	case *syntax.IfClause:
		for ic := c; ic != nil; ic = ic.Else {
			collectStmts(ic.Then, out, depth)
		}
	case *syntax.WhileClause:
		collectStmts(c.Do, out, depth)
	case *syntax.ForClause:
		collectStmts(c.Do, out, depth)
	}
}

// wordString renders a word as the literal text it would most likely expand
// to; parameter expansions and substitutions keep their source form.
func wordString(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if l, ok := q.(*syntax.Lit); ok {
					b.WriteString(l.Value)
				} else {
					printNode(&b, q)
				}
			}
		default:
			printNode(&b, p)
		}
	}
	return b.String()
}

func printNode(b *strings.Builder, n syntax.Node) {
	var buf bytes.Buffer
	if syntax.NewPrinter().Print(&buf, n) == nil {
		b.Write(buf.Bytes())
	}
}

// add records one real command, unwrapping wrappers and skipping setup
// commands. Shell interpreters with -c are parsed recursively.
func (c *Command) add(words []string, depth int) {
	words = unwrap(words)
	if len(words) == 0 {
		return
	}
	name := commandName(words[0])
	if skipCommands[name] {
		return
	}
	if (name == "bash" || name == "sh" || name == "zsh") && len(words) >= 3 && words[1] == "-c" && depth < maxDepth {
		if f, err := parseShell(words[2]); err == nil {
			collectStmts(f.Stmts, c, depth+1)
			return
		}
	}
	c.Commands = append(c.Commands, words)
	c.Families = append(c.Families, family(words))
}

// unwrap strips wrappers (`time`, `env X=Y`, `timeout 30`, `sudo -u x`) and
// returns the words of the wrapped command.
func unwrap(words []string) []string {
	for len(words) > 0 && wrapperCommands[commandName(words[0])] {
		name := commandName(words[0])
		words = words[1:]
		for len(words) > 0 {
			w := words[0]
			switch {
			case strings.HasPrefix(w, "-"):
				words = words[1:]
				if name == "sudo" && (w == "-u" || w == "-g") && len(words) > 0 {
					words = words[1:]
				}
				continue
			case name == "env" && strings.Contains(w, "="):
				words = words[1:]
				continue
			case name == "timeout" && len(w) > 0 && isDigit(w[0]):
				words = words[1:]
				continue
			case name == "nice" && len(w) > 0 && isDigit(w[0]):
				words = words[1:]
				continue
			}
			break
		}
	}
	return words
}

// commandName lower-cases the command word and strips its directory and a
// Windows executable extension: "C:\Python\python.exe" → "python".
func commandName(w string) string {
	w = strings.ToLower(w)
	if i := strings.LastIndexAny(w, `/\`); i >= 0 {
		w = w[i+1:]
	}
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if strings.HasSuffix(w, ext) {
			return w[:len(w)-len(ext)]
		}
	}
	return w
}

// family returns the command family: the command name plus its first
// argument when that argument looks like a subcommand (LLD §4.3).
func family(words []string) string {
	name := commandName(words[0])
	args := words[1:]
	switch {
	case (strings.HasPrefix(name, "python") || name == "py") && len(args) >= 2 && args[0] == "-m":
		return family(args[1:])
	case envRunners[name] && len(args) >= 2 && args[0] == "run":
		return family(args[1:])
	case scriptRunners[name] && len(args) >= 2 && args[0] == "run" && subcommandRe.MatchString(strings.ToLower(args[1])):
		return name + " run " + strings.ToLower(args[1])
	}
	if len(args) > 0 {
		if sub := strings.ToLower(args[0]); subcommandRe.MatchString(sub) {
			return name + " " + sub
		}
	}
	return name
}

// PollMatcher decides whether a command is a polling command, whose
// repeats are expected (LLD §4.3).
type PollMatcher struct {
	prefixes [][]string
	patterns []*regexp.Regexp
}

// NewPollMatcher builds a matcher from polling family prefixes (e.g.
// "gh run watch") and regular expressions over the normalized command.
// Invalid patterns are skipped.
func NewPollMatcher(families, patterns []string) *PollMatcher {
	m := &PollMatcher{}
	for _, f := range families {
		if ws := strings.Fields(strings.ToLower(f)); len(ws) > 0 {
			m.prefixes = append(m.prefixes, ws)
		}
	}
	for _, p := range patterns {
		if re, err := regexp.Compile(p); err == nil {
			m.patterns = append(m.patterns, re)
		}
	}
	return m
}

// IsPolling reports whether the primary command of c is a polling
// command: its non-flag words start with a polling prefix, or its text
// matches a polling pattern.
func (m *PollMatcher) IsPolling(c Command) bool {
	words := c.PrimaryWords()
	if len(words) == 0 {
		return false
	}
	nonFlag := make([]string, 0, len(words))
	nonFlag = append(nonFlag, commandName(words[0]))
	for _, w := range words[1:] {
		if !strings.HasPrefix(w, "-") {
			nonFlag = append(nonFlag, strings.ToLower(w))
		}
	}
	for _, p := range m.prefixes {
		if hasPrefix(nonFlag, p) {
			return true
		}
	}
	if len(m.patterns) > 0 {
		line := strings.Join(append([]string{commandName(words[0])}, words[1:]...), " ")
		for _, re := range m.patterns {
			if re.MatchString(line) {
				return true
			}
		}
	}
	return false
}

func hasPrefix(words, prefix []string) bool {
	if len(words) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if words[i] != p {
			return false
		}
	}
	return true
}
