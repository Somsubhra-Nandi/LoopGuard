# Test-runner outputs

Failing outputs used by the error-signature tests (`internal/normalize`) and the scripted sessions (`testdata/sessions`).

| Dir | Source | How |
|---|---|---|
| `gotest/` | **Real**: go 1.27.2 | `go test -count=1 ./...` on a tiny `calc` package, editing the code/test between runs |
| `tsc/` | **Real**: tsc 7.0.2 | `tsc -p .` (non-TTY, so no `--pretty`) on a one-file project |
| `jest/` | **Real**: jest 29.7.0 | `npx jest --ci` on a two-file project; keeps jest's ANSI-colored code frames |
| `pytest/` | Hand-written | Python was not installed when these were made; modeled on pytest 8 output |
| `cargo/` | Hand-written | Rust was not installed when these were made; modeled on cargo 1.8x output |

Naming: `*_1`/`*_2` are reruns of the same failure, `*_shifted` is the same failure after an edit moved the failing line, and `*_other*`/`*_fail` with another name are different failures. `tsc/type_fail_moved_same_msg.txt` is the same message at another location, which the signature deliberately treats as the same error.

Replace the hand-written files with real captures when the toolchains are available, and keep the same names so the tests still apply.
