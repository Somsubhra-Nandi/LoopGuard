# Transcript fixtures

`anonymized/` holds three real Claude Code sessions, exported with `loopguard replay --export-anonymized` (see `internal/replay/export.go` for exactly what is kept and dropped). They are the sessions with the most loop-like activity in the author's private set: the longest same-family failure runs and the most rewrites of one file in a turn. None of them contains a loop by the M1 detectors' definitions, so `golden.json` pins **no detections** on them. That makes them a false-positive regression test on real-shaped data.

Positive detection through the transcript reader is covered by the synthetic transcripts in `internal/replay/replaytest`.

- **Before adding an export**, scan it for names, paths, hosts, and secrets. Real transcripts live only in `testdata/private/`, which is gitignored.
- **After a detector change**, run `go test ./internal/replay -run TestGolden -update` and review the diff of `golden.json`.
