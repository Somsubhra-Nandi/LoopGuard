# Synthetic Claude Code hook payloads

These payloads are **synthetic**: written by hand from the Claude Code hooks
reference (code.claude.com/docs/en/hooks, as summarized in LLD §3.1), not
captured from a real session. They pin the adapter's field mapping.

Real payloads are never committed. `loopguard capture export` writes them,
anonymized, to `testdata/private/hooks/claude-code/<claude --version>/`
(git-ignored). What goes next to this directory, one directory per Claude
Code version, comes from `loopguard capture export --structure-only`: the
real payloads' keys, nesting and value types, with content strings replaced
by `"<str:N>"` placeholders (see docs/capturing-payloads.md).

The golden test picks up every `*.json` below `testdata/hooks/claude-code/`
and, when it exists, `testdata/private/hooks/claude-code/`; run
`go test ./internal/adapter/claudecode -update` to rewrite the expected
`*.golden` files, then review the diff. `make check-testdata` (run in CI)
fails if a file here has a string longer than 40 characters in a content
field (`prompt`, `tool_input`, `tool_response`, `error`, ...), so keep
hand-written content short.
