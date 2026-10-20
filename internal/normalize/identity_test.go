package normalize

import "testing"

func identityOf(cmd, cwd string) string {
	c := ParseCommand(cmd)
	return Identity(c.PrimaryWords(), c.Primary(), cwd)
}

// TestIdentityEquivalences has an example for each rule in identity.go.
func TestIdentityEquivalences(t *testing.T) {
	const cwd = `C:\src\shop`
	same := []struct{ rule, a, b string }{
		{"1 path and extension", `C:\Python312\python.exe foo.py`, "python foo.py"},
		{"1 case", "PYTEST tests", "pytest tests"},
		{"2 alias py", "py foo.py", "python foo.py"},
		{"2 alias python3", "python3 foo.py", "python foo.py"},
		{"2 alias python3.12", "python3.12 foo.py", "python foo.py"},
		{"3 -m", "python -m pytest tests", "pytest tests"},
		{"3 -m with flags", "python3 -u -X utf8 -m pytest tests", "pytest tests"},
		{"4 npx", "npx jest src/cart.test.ts", "jest src/cart.test.ts"},
		{"4 npx flags", "npx --yes -p jest@29 jest src/cart.test.ts", "jest src/cart.test.ts"},
		{"4 bunx", "bunx vitest run", "vitest run"},
		{"4 pnpm exec", "pnpm exec jest", "jest"},
		{"4 pnpm dlx", "pnpm dlx tsc -p .", "tsc -p ."},
		{"4 yarn exec", "yarn exec jest", "jest"},
		{"4 npm exec", "npm exec -- jest", "jest"},
		{"4 bun x", "bun x jest", "jest"},
		{"5 uv run", "uv run pytest -k add", "pytest -k add"},
		{"5 poetry run", "poetry run python -m pytest", "pytest"},
		{"6 npm run test", "npm run test", "npm test"},
		{"6 npm t", "npm t", "npm test"},
		{"6 yarn run test", "yarn run test", "yarn test"},
		{"7 whitespace", "go  test   ./pkg/a", "go test ./pkg/a"},
		{"7 flag order", "pytest -k add -x tests", "pytest tests -x -k add"},
		{"7 harmless flags", "pytest -q --tb=short -v --color=yes tests", "pytest tests"},
		{"7 harmless go flags", "go test -v -count=1 ./pkg/a", "go test ./pkg/a"},
		{"7 harmless jest flags", "jest --ci --runInBand --colors", "jest"},
		{"7 ./ and trailing slash", "go test ./pkg/a/", "go test pkg/a"},
		{"7 backslashes", `pytest tests\test_cart.py`, "pytest tests/test_cart.py"},
		{"7 absolute inside cwd", `pytest C:\src\shop\tests\test_cart.py`, "pytest tests/test_cart.py"},
		{"7 ./...", "go test ./...", "go test ..."},
		{"wrappers", "cd web && CI=1 timeout 60 npm test 2>&1 | tail -20", "npm test"},
		{"powershell", `Set-Location C:\src\shop; npm test 2>&1 | Select-Object -Last 40`, "npm test"},
		{"powershell call operator", `& "C:\Python312\python.exe" -m pytest tests -q`, "pytest tests"},
		{"bash -c", "bash -c 'pytest tests'", "pytest tests"},
	}
	for _, tt := range same {
		if a, b := identityOf(tt.a, cwd), identityOf(tt.b, cwd); a != b || a == "" {
			t.Errorf("rule %s: %q → %q, %q → %q; want equal", tt.rule, tt.a, a, tt.b, b)
		}
	}
	different := [][2]string{
		{"go test ./pkg/a", "go test ./pkg/b"},
		{"python foo.py", "python -c 'print(1)'"},
		{"python foo.py", "python bar.py"},
		{"pytest -k add", "pytest -k sub"},
		{"go test -run TestAdd ./...", "go test -run TestSub ./..."},
		{"pytest tests/test_a.py::test_x", "pytest tests/test_a.py::test_y"},
		{"npm run build", "npm build"},
		{"npm test", "npx jest"},
		{"jest", "jest src/cart.test.ts"},
		{"cargo test", "cargo test --release"},
	}
	for _, p := range different {
		if a, b := identityOf(p[0], cwd), identityOf(p[1], cwd); a == b {
			t.Errorf("%q and %q have the same identity %q", p[0], p[1], a)
		}
	}
}

func TestFallbackSegments(t *testing.T) {
	for line, want := range map[string]string{
		`cd x; npm test`:                          "npm test",
		`python -c "import x; print(1)"`:          "python",
		`npm test 2>&1 | Select-Object -Last 50`:  "npm test",
		`npm test > out.txt; Get-Content out.txt`: "get-content",
		`echo "unterminated && npm test`:          "echo",
		`go test ./... || Write-Host failed`:      "write-host failed",
	} {
		// Force the fallback with a construct bash rejects.
		c := fallbackCommand(line)
		if c.Primary() != want {
			t.Errorf("fallback %q: primary %q (all %q), want %q", line, c.Primary(), c.Families, want)
		}
	}
}
