# Transcript fixtures

`anonymized/` holds three real Claude Code sessions, exported with `loopguard replay --export-anonymized` (see `internal/replay/export.go` for exactly what is kept and dropped). They are the sessions with the most loop-like activity in the author's private set: the longest same-family failure runs and the most rewrites of one file in a turn. None of them contains a loop by the M1 detectors' definitions, so `golden.json` pins **no detections** on them. That makes them a false-positive regression test on real-shaped data.

Since M4 they also carry usage (token counts, model, hashed message ids), compaction boundaries, and each tool result's original length (`chars`), so `golden.json` pins their API-equivalent cost and per-call estimates too. They were re-exported with the M4 exporter and merged with their earlier tool results (content and `is_error`), so detection on them is unchanged; a fresh export would differ in `is_error` on a few results, because the soft-failure rules changed since (LLD §18 decision 1, M4-A decision 122).

Positive detection through the transcript reader is covered by the synthetic transcripts in `internal/replay/replaytest`.

- **Before adding an export**, scan it for names, paths, hosts, and secrets. Real transcripts live only in `testdata/private/`, which is gitignored.
- **After a detector change**, run `go test ./internal/replay -run TestGolden -update` and review the diff of `golden.json`.
