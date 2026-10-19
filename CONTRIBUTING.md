# Contributing to LoopGuard

Thanks for helping. LoopGuard is in early development, so open an issue before starting anything large. Check [plan.md](plan.md) and the [design docs](docs/) first.

## Build and test

You need Go (version in `go.mod`), `make`, and a C compiler for the race detector (`go test -race` needs cgo). On Windows, run `make` from Git Bash.

```sh
make fmt     # gofmt -s -w .
make lint    # go vet + staticcheck
make test    # go test -race ./...
make build   # bin/loopguard, with version info from git
```

CI runs `fmt-check`, `vet`, `staticcheck`, `test`, and `build` on Linux, macOS, and Windows. Run `make all` before you push.

## Ground rules

- **Fail-open, always.** `loopguard hook` must never break the agent: on any error it exits 0 with no output.
- Keep dependencies minimal; discuss new ones in an issue first.
- Add tests with every change. Detector changes must not lower precision on the eval corpus.
- Never commit raw captured payloads or transcripts. Anonymize them first (see [docs/capturing-payloads.md](docs/capturing-payloads.md)).

## Commit style

Use [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): summary`, in the imperative and at most 72 characters, e.g. `feat(detect): add repeated-failure detector` or `fix(shim): exit 0 when the daemon times out`. Common types: `feat`, `fix`, `test`, `docs`, `refactor`, `build`, `ci`, `chore`. Keep each commit focused on one change.
