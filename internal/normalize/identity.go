package normalize

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// Command equivalences (LLD §4.3, §18 M3-B). This file holds every rule
// that makes two spellings of a command the same command; identity_test.go
// has an example per rule. The rules apply to families (so a loop doesn't
// restart when the agent rewords the command) and to command identities
// (so a block can't be dodged by rewording).
//
//  1. The command name is lower-cased, with its directory and a Windows
//     extension removed (commandName): C:\Python312\python.exe → python.
//  2. Aliases: py, python3, python3.12, … → python.
//  3. python [flags] -m X … → X …  (python -m pytest → pytest).
//  4. Exec wrappers run their argument: npx X, bunx X, pnpm exec|dlx X,
//     yarn exec|dlx X, npm exec X, bun x X → X (their own flags dropped).
//  5. Env runners: uv|poetry|pipenv|hatch|pdm run X → X.
//  6. Script runners' built-in scripts: npm|yarn|pnpm|bun run test → … test
//     (also start, stop, restart; npm t → npm test). Other scripts keep
//     "run": npm run build ≠ npm build.
//  7. Identity only: flags in harmlessFlags (verbosity, color, report
//     format, fail-fast) are dropped, path arguments are cleaned (./a/ →
//     a, backslashes → slashes, inside cwd → relative), and the remaining
//     arguments are sorted, so flag order doesn't matter.
//
// Shell spacing and quoting differences are already gone after parsing.

// commandAliases map interpreter names to one name.
var commandAliases = map[string]string{"py": "python", "python3": "python", "python2": "python", "pip3": "pip", "nodejs": "node"}

var pythonVersionRe = regexp.MustCompile(`^python[23](\.\d+)?$`)

// execRunners run their first non-flag argument as the command.
var execRunners = map[string]bool{"npx": true, "bunx": true}

// execSubcommands: runner → subcommands that run their argument.
var execSubcommands = map[string]map[string]bool{
	"pnpm": {"exec": true, "dlx": true},
	"yarn": {"exec": true, "dlx": true},
	"npm":  {"exec": true},
	"bun":  {"x": true},
}

// runnerValueFlags take a value as the next word (npx -p pkg jest).
var runnerValueFlags = map[string]bool{"-p": true, "--package": true, "-c": true, "--call": true}

// builtinScripts are the scripts npm-style runners also accept without
// "run", so "npm run test" and "npm test" are the same command.
var builtinScripts = map[string]bool{"test": true, "start": true, "stop": true, "restart": true}

// harmlessFlags change how a command reports, not what it runs. They are
// left out of command identities. A trailing "=" matches any value.
var harmlessFlags = map[string]bool{
	// verbosity
	"-v": true, "-vv": true, "-vvv": true, "--verbose": true, "-q": true, "-qq": true, "--quiet": true,
	"-s": true, "--silent": true, "--no-header": true, "--disable-warnings": true,
	// color
	"--color": true, "--color=": true, "--colors": true, "--no-color": true, "--no-colors": true, "--colour=": true,
	// report format and fail-fast
	"--tb=": true, "-ra": true, "-rA": true, "-rf": true, "-x": true, "--exitfirst": true, "--maxfail=": true,
	"--ci": true, "--runInBand": true, "-i": true, "--watchAll=false": true, "--no-watch": true, "--no-coverage": true,
	"-count=1": true, "-failfast": true, "-json": true,
	"--nocapture": true, "--show-output": true, "--": true,
}

func harmless(flag string) bool {
	if harmlessFlags[flag] {
		return true
	}
	if i := strings.IndexByte(flag, '='); i > 0 {
		return harmlessFlags[flag[:i+1]]
	}
	return false
}

// canonicalWords applies rules 1–6 to a command's words, returning them
// with the canonical command name first.
func canonicalWords(words []string) []string {
	for depth := 0; depth < maxDepth && len(words) > 0; depth++ {
		name := commandName(words[0])
		if a, ok := commandAliases[name]; ok {
			name = a
		} else if pythonVersionRe.MatchString(name) {
			name = "python"
		}
		args := words[1:]
		if name == "python" {
			if i := pythonModuleArg(args); i >= 0 && i+1 < len(args) {
				words = args[i+1:]
				continue
			}
		}
		if envRunners[name] && len(args) >= 2 && args[0] == "run" {
			words = args[1:]
			continue
		}
		if execRunners[name] {
			words = skipRunnerFlags(args)
			continue
		}
		if subs := execSubcommands[name]; subs != nil && len(args) >= 2 && subs[args[0]] {
			words = skipRunnerFlags(args[1:])
			continue
		}
		if scriptRunners[name] && len(args) >= 2 && (args[0] == "run" || args[0] == "run-script") && builtinScripts[strings.ToLower(args[1])] {
			args = args[1:]
		}
		if name == "npm" && len(args) > 0 && args[0] == "t" {
			args = append([]string{"test"}, args[1:]...)
		}
		return append([]string{name}, args...)
	}
	return words
}

// pythonModuleArg returns the index of "-m" among python's leading flags,
// or -1 (python -u -X utf8 -m pytest).
func pythonModuleArg(args []string) int {
	for i := 0; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch args[i] {
		case "-m":
			return i
		case "-c":
			return -1
		case "-X", "-W":
			i++
		}
	}
	return -1
}

func skipRunnerFlags(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if runnerValueFlags[args[0]] && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return args
}

// Identity is a command's identity (LLD §7.1): its canonical name plus its
// arguments (subcommand included) without harmless flags, paths cleaned,
// sorted. family only says whether there is a command at all. Blocks match
// identities, so `go test ./pkg/a` and `go test ./pkg/b` are different
// commands, while `pytest -q tests` and `python3 -m pytest tests -v` are
// the same. words must already be canonical (Command.PrimaryWords).
func Identity(words []string, family, cwd string) string {
	if len(words) == 0 || family == "" {
		return ""
	}
	base := ""
	if cwd != "" {
		base = NormalizePath(".", cwd)
	}
	// Everything after the name is sorted, the subcommand too: the family's
	// "first word-like argument" rule would make `pytest tests -k a` and
	// `pytest -k a tests` differ.
	var toks []string
	for _, w := range words[1:] {
		w = strings.Trim(w, `"'`)
		switch {
		case w == "":
		case strings.HasPrefix(w, "-"):
			if !harmless(w) {
				toks = append(toks, w)
			}
		case looksLikePath(w):
			toks = append(toks, cleanArgPath(w, base))
		default:
			toks = append(toks, w)
		}
	}
	sort.Strings(toks)
	name := commandName(words[0])
	if len(toks) == 0 {
		return name
	}
	return name + " " + strings.Join(toks, " ")
}

var fileNameRe = regexp.MustCompile(`^[\w.@-]+\.[A-Za-z][A-Za-z0-9]{0,5}(::.*)?$`)

func looksLikePath(w string) bool {
	return strings.ContainsAny(w, `/\`) || w == "." || fileNameRe.MatchString(w)
}

// cleanArgPath makes path arguments comparable: slashes, no "./" or
// trailing slash, relative to the working directory when inside it.
// pytest node ids (file.py::test) keep their "::" part.
func cleanArgPath(w, base string) string {
	w = strings.ReplaceAll(w, `\`, "/")
	if isAbsPath(w) && base != "" {
		p := NormalizePath(w, "")
		if strings.EqualFold(p, base) {
			return "."
		}
		if len(p) > len(base) && p[len(base)] == '/' && strings.EqualFold(p[:len(base)], base) {
			return p[len(base)+1:]
		}
		return p
	}
	if strings.HasSuffix(w, "/...") {
		if dir := path.Clean(strings.TrimSuffix(w, "/...")); dir != "." {
			return dir + "/..."
		}
		return "..."
	}
	return path.Clean(w)
}

// fallbackSegments splits a command line that isn't valid bash
// (PowerShell, cmd, a truncated heredoc) into the words of each command:
// quote-aware, at unquoted ; && || and |, keeping only the producer of a
// pipeline, and dropping redirections and PowerShell's call operators
// (& and .).
func fallbackSegments(line string) [][]string {
	var segs [][]string
	var cur []string
	var tok strings.Builder
	inTok, inPipe := false, false
	var quote byte
	flushTok := func() {
		if inTok {
			cur = append(cur, tok.String())
			tok.Reset()
			inTok = false
		}
	}
	flushSeg := func(pipe bool) {
		flushTok()
		if !inPipe {
			if seg := cleanSegment(cur); len(seg) > 0 {
				segs = append(segs, seg)
			}
		}
		cur = nil
		inPipe = pipe
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0:
			// Quotes stay in the word: a quoted first argument is a value,
			// not a subcommand (commandName and Identity drop them).
			tok.WriteByte(ch)
			if ch == quote {
				quote = 0
			}
		case ch == '"' || ch == '\'':
			quote, inTok = ch, true
			tok.WriteByte(ch)
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			flushTok()
		case ch == ';':
			flushSeg(false)
		case ch == '&' && i+1 < len(line) && line[i+1] == '&', ch == '|' && i+1 < len(line) && line[i+1] == '|':
			i++
			flushSeg(false)
		case ch == '|':
			flushSeg(true) // the rest of the pipeline is skipped
		default:
			tok.WriteByte(ch)
			inTok = true
		}
	}
	flushSeg(false)
	return segs
}

// cleanSegment drops redirections ("2>&1", "> out.txt", "2>$null") and a
// leading call operator from one fallback command.
func cleanSegment(words []string) []string {
	out := words[:0:0]
	for i := 0; i < len(words); i++ {
		w := words[i]
		if j := strings.IndexByte(w, '>'); j >= 0 && (j == 0 || isRedirPrefix(w[:j])) {
			if strings.TrimLeft(w[j:], ">&") == "" && !strings.HasSuffix(w, "&1") && !strings.HasSuffix(w, "&2") {
				i++ // the target is the next word
			}
			continue
		}
		out = append(out, w)
	}
	if len(out) > 0 && (out[0] == "&" || out[0] == ".") {
		out = out[1:]
	}
	return out
}

func isRedirPrefix(s string) bool {
	return s == "*" || (len(s) == 1 && isDigit(s[0]))
}
