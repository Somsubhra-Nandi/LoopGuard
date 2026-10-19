package normalize

import (
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		// ANSI and carriage returns
		{"ansi color", "\x1b[31mFAIL\x1b[0m src/a.test.ts", "FAIL src/a.test.ts"},
		{"ansi cursor", "\x1b[2K\x1b[1Gdone", "done"},
		{"osc title", "\x1b]0;npm test\x07ok", "ok"},
		{"osc st", "\x1b]8;;http://x\x1b\\link", "link"},
		{"progress overwrite", "Downloading 10%\rDownloading 55%\rDownloading 100%", "Downloading 100%"},
		{"crlf", "line one\r\nline two\r\n", "line one\nline two"},

		// timestamps
		{"iso utc", "at 2026-10-19T15:06:21Z failed", "at <TS> failed"},
		{"iso millis offset", "2026-10-19T15:06:21.123+05:30 boom", "<TS> boom"},
		{"iso space", "2026-10-19 15:06:21,456 ERROR x", "<TS> ERROR x"},
		{"iso date", "built 2026-10-19", "built <TS>"},
		{"clock", "[15:06:21] Starting", "[<TS>] Starting"},
		{"clock millis", "15:06:21.123 INFO", "<TS> INFO"},
		{"rfc1123", "Date: Mon, 19 Oct 2026 15:06:21 GMT", "Date: <TS>"},
		{"ctime", "Mon Oct 19 15:06:21 2026 started", "<TS> started"},
		{"not a clock: line:col", "a.go:12:34: undefined", "a.go:12:34: undefined"},

		// durations
		{"go dur", "--- FAIL: TestAdd (0.00s)", "--- FAIL: TestAdd (<DUR>)"},
		{"ms", "took 450ms", "took <DUR>"},
		{"compound", "finished in 3m2s", "finished in <DUR>"},
		{"compound h", "uptime 1h2m3.5s", "uptime <DUR>"},
		{"spaced s", "Time:        2.07 s", "Time: <DUR>"},
		{"spaced word", "ran for 12 seconds", "ran for <DUR>"},
		{"jest estimated", "Time: 1.196 s, estimated 2 s", "Time: <DUR>, estimated <DUR>"},
		{"micro", "op 1.5µs", "op <DUR>"},
		{"pytest", "1 failed, 4 passed in 0.12s", "1 failed, 4 passed in <DUR>"},
		{"not a dur: plural", "2 suites, 3 tests", "2 suites, 3 tests"},
		{"not a dur: inside word", "test2s ok", "test2s ok"},

		// IDs
		{"uuid", "request 3f2504e0-4f89-11d3-9a0c-0305e82c3301 failed", "request <ID> failed"},
		{"ulid", "id 01HF3Z9K2M8V7Q4W5X6Y7Z8A9B done", "id <ID> done"},
		{"git sha", "HEAD is now at 1a2a6d4 docs", "HEAD is now at <ID> docs"},
		{"long sha", "commit 836bd0b1f2e3d4c5b6a7980716253443", "commit <ID>"},
		{"hex addr", "panic at 0x7ffd5e8c1a20", "panic at <ID>"},
		{"word not sha", "the file was defaced", "the file was defaced"},
		{"short hex kept", "code abc123 ok", "code abc123 ok"},

		// temp paths
		{"tmp", "open /tmp/pytest-of-dev/pytest-41/test_add0/in.txt: no such file", "open <TMP>: no such file"},
		{"var tmp", "wrote /var/tmp/build-1234/out", "wrote <TMP>"},
		{"tmp line col", "/tmp/go-build123/x.go:12: undefined", "<TMP>:12: undefined"},
		{"macos", "at /var/folders/xy/abc123_def/T/jest_rs/cache", "at <TMP>"},
		{"macos private", "at /private/var/folders/xy/T/tmp.abc", "at <TMP>"},
		{"windows backslash", `copy C:\Users\dev\AppData\Local\Temp\go-build4129354589\b001\x.exe done`, "copy <TMP> done"},
		{"windows forward", "WindowsPath('C:/Users/dev/AppData/Local/Temp/pytest-of-dev/pytest-41/test_add0')", "WindowsPath('<TMP>')"},
		{"windows space user", `in C:\Users\Jane Doe\AppData\Local\Temp\claude\x.txt ok`, "in <TMP> ok"},
		{"windows 8.3", `C:\Users\JANEDO~1\AppData\Local\Temp\go-link-1996151474\go.o`, "<TMP>"},
		{"windows temp", `C:\Windows\Temp\setup.log`, "<TMP>"},
		{"percent temp", `%TEMP%\build\a.obj`, "<TMP>"},
		{"tmpdir var", "$TMPDIR/x", "<TMP>"},
		{"not tmp: word", "/tmpfiles/x and tmp_path", "/tmpfiles/x and tmp_path"},

		// numbers
		{"pid", "killed pid 42", "killed pid <N>"},
		{"pid eq", "process=7 exited", "process=<N> exited"},
		{"port", "listening on localhost:54321", "listening on localhost:<N>"},
		{"port phrase", "port 80 in use", "port <N> in use"},
		{"big number", "wrote 123456 bytes", "wrote <N> bytes"},
		{"small numbers kept", "test_x.py:12: assert 4 == 3", "test_x.py:12: assert 4 == 3"},
		{"inside word kept", "foo_1234 bar", "foo_1234 bar"},

		// whitespace
		{"collapse", "  a \t  b   c  ", "a b c"},
		{"empty lines dropped", "a\n\n   \nb", "a\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.in); got != tt.want {
				t.Errorf("Text(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTextTruncates(t *testing.T) {
	in := strings.Repeat("x", MaxInput+100)
	if got := len(Text(in)); got != MaxInput {
		t.Errorf("len = %d, want %d", got, MaxInput)
	}
}

func TestTextIdempotent(t *testing.T) {
	in := "2026-10-19T15:06:21Z /tmp/x 0x1f 450ms 12345 \x1b[1mbold\x1b[0m"
	once := Text(in)
	if twice := Text(once); twice != once {
		t.Errorf("not idempotent:\n%q\n%q", once, twice)
	}
}

func TestMaskLocations(t *testing.T) {
	tests := []struct{ in, want string }{
		{"calc_test.go:7: Add(1, 2) = 4, want 3", "calc_test.go:<LN>: Add(1, 2) = 4, want 3"},
		{".\\calc.go:3:37: undefined: c", ".\\calc.go:<LN>: undefined: c"},
		{"src/a.ts(4,9): error TS2322", "src/a.ts(<LN>): error TS2322"},
		{`File "app/main.py", line 3, in <module>`, `File "app/main.py", line <LN>, in <module>`},
		{"> 14 | expect(total([])).toBe(0);", "> <LN> | expect(total([])).toBe(0);"},
		{"6 | });", "<LN> | });"},
		{"at Object.toBe (cart.test.js:14:23)", "at Object.toBe (cart.test.js:<LN>)"},
		{"Expected: 7", "Expected: 7"},
	}
	for _, tt := range tests {
		if got := MaskLocations(tt.in); got != tt.want {
			t.Errorf("MaskLocations(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
