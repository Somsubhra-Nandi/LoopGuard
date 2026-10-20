# Synthetic Claude Code hook payloads

These payloads are **synthetic**: written by hand from the Claude Code hooks
reference (code.claude.com/docs/en/hooks, as summarized in LLD §3.1), not
captured from a real session. They pin the adapter's field mapping until
real payloads are captured.

To capture real ones, run the daemon with `LOOPGUARD_CAPTURE=1` (payloads
land redacted in `~/.loopguard/capture/claude-code/`), review them, and copy
them into `testdata/hooks/claude-code/<claude --version>/`. The golden test
picks up every `*.json` in the subdirectories here; run
`go test ./internal/adapter/claudecode -update` to write the expected
`*.golden` files, then review the diff.
