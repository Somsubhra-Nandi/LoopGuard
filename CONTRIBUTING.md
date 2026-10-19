# Contributing to LoopGuard

Thanks for helping. LoopGuard is in early development, so open an issue before starting anything large. Check [plan.md](plan.md) and the [design docs](docs/) first.

## Build and test

You need Go (version in `go.mod`) and `make`. `make test` runs the race detector on Linux and macOS, which needs cgo and a C compiler. On Windows it runs without `-race` by default (`make test RACE=1` turns it on), and you run `make` from Git Bash.

```sh
make fmt     # gofmt -s -w .
make lint    # go vet + staticcheck
make test    # go test ./... (-race on Linux/macOS)
make build   # bin/loopguard, with version info from git
make eval    # precision/recall on your labeled transcripts (needs LOOPGUARD_EVAL_DIR)
```

Without `make` (e.g. in PowerShell), the equivalents are `gofmt -s -l .`, `go vet ./...`, `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...`, `go test ./...`, and `go build ./cmd/loopguard`.

CI runs `fmt-check`, `vet`, `staticcheck`, `test`, and `build` on Linux, macOS, and Windows. Run `make all` before you push.

Real transcripts go in `testdata/private/` (gitignored) and never in commits. To turn one into a fixture, use `loopguard replay --export-anonymized`, scan the result, and see `testdata/transcripts/README.md`.

## Ground rules

- **Fail-open, always.** `loopguard hook` must never break the agent: on any error it exits 0 with no output.
- Keep dependencies minimal; discuss new ones in an issue first.
- Add tests with every change. Detector changes must not lower precision on the eval corpus.
- Never commit raw captured payloads or transcripts. Anonymize them first (see [docs/capturing-payloads.md](docs/capturing-payloads.md)).

## Commit style

Use [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): summary`, in the imperative and at most 72 characters, e.g. `feat(detect): add repeated-failure detector` or `fix(shim): exit 0 when the daemon times out`. Common types: `feat`, `fix`, `test`, `docs`, `refactor`, `build`, `ci`, `chore`. Keep each commit focused on one change.
