# LoopGuard

A circuit breaker for AI coding agents: it detects when Claude Code or Codex is stuck in a loop, nudges it out, and halts it if needed, while tracking tokens, cost, and wasted time.

> **Status: early development.** Nothing works yet beyond the CLI skeleton. Don't install it in your agent's settings yet.

- Build plan and milestones: [plan.md](plan.md)
- Design docs: [docs/](docs/): [HLD](docs/HLD.md) · [LLD](docs/LLD.md) · [TDD](docs/TDD.md)
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md)

## Build from source

```sh
make build            # → bin/loopguard
./bin/loopguard version
```

Requires Go (see `go.mod` for the version) and `make`.

## License

[Apache-2.0](LICENSE)
