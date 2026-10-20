# Synthetic Claude Code hook payloads

These payloads are **synthetic**: written by hand from the Claude Code hooks
reference (code.claude.com/docs/en/hooks, as summarized in LLD §3.1), not
captured from a real session. They pin the adapter's field mapping until
real payloads are captured.

Real ones go next to this directory, one directory per Claude Code version:
run `loopguard capture on`, use Claude Code, `loopguard capture off`, then
`loopguard capture export` from the repo root (see
docs/capturing-payloads.md). It writes anonymized payloads and their
`*.golden` files to `testdata/hooks/claude-code/<claude --version>/`.

The golden test picks up every `*.json` below `testdata/hooks/claude-code/`;
run `go test ./internal/adapter/claudecode -update` to rewrite the expected
`*.golden` files, then review the diff.
