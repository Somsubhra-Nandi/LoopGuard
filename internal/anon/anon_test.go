package anon

import (
	"reflect"
	"testing"
)

// Every test here runs Windows-style and Unix-style inputs on every OS:
// the paths come from data, so the results must not depend on where the
// tests run.

func TestBase(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\Jane Doe`:          "Jane Doe",
		`C:\Users\Jane Doe\`:         "Jane Doe",
		"C:/Users/Jane Doe/proj":     "proj",
		"/home/jane/proj/":           "proj",
		`/c/Users/jane\mixed\seps`:   "seps",
		"proj":                       "proj",
		`\\server\share\team proj`:   "team proj",
		"/Users/jane/Library/x.json": "x.json",
	} {
		if got := Base(in); got != want {
			t.Errorf("Base(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHomeUsers(t *testing.T) {
	for in, want := range map[string][]string{
		`cd C:\Users\Jane Q-Doe\desktop\shop`:           {"Jane Q-Doe"},
		`C:/Users/Jane Q-Doe/x`:                         {"Jane Q-Doe"},
		`ls /c/Users/jdoe/src`:                          {"jdoe"},
		`/mnt/c/Users/jdoe/src`:                         {"jdoe"},
		`TEMP=C:\Users\JANEQ-~1\AppData\Local\Temp`:     {"JANEQ-~1"},
		`cat /home/bobby/.bashrc and /Users/alice/x`:    {"bobby", "alice"},
		`"cwd":"/home/carol"`:                           {"carol"},
		`/usr/local/bin and src/users/list.ts`:          nil,
		`see https://example.com/home/page for details`: nil,
	} {
		if got := HomeUsers(in); !reflect.DeepEqual(got, want) {
			t.Errorf("HomeUsers(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortName(t *testing.T) {
	for in, want := range map[string]string{
		"Jane Doe":   "JANEDO",
		"Jane Q-Doe": "JANEQ-",
		"jane":       "",
		"ADMINI~1":   "",
		"first.last": "FIRSTL",
		"Zoë Smith":  "",
	} {
		if got := ShortName(in); got != want {
			t.Errorf("ShortName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHomeReplacer(t *testing.T) {
	type tc struct{ in, want string }
	styles := []struct {
		home  string
		cases []tc
	}{
		{`C:\Users\Jane Q-Doe`, []tc{
			{`C:\Users\Jane Q-Doe\desktop\shop`, `<HOME>\desktop\shop`},
			{`c:/users/jane q-doe/desktop/shop`, `<HOME>/desktop/shop`},
			{`/c/Users/Jane Q-Doe/src`, `<HOME>/src`},
			{`/mnt/c/Users/Jane Q-Doe/src`, `<HOME>/src`},
			{`C:\Users\JANEQ-~1\AppData\Local\Temp\x`, `<HOME>\AppData\Local\Temp\x`},
			{`projects\C--Users-Jane-Q-Doe-desktop-shop\s.jsonl`, `projects\<HOME>-desktop-shop\s.jsonl`},
			{`C:\Users\Jane Q-Doer\x`, `C:\Users\Jane Q-Doer\x`}, // a different user
			{`C:\Users\Jane Q-Doe`, `<HOME>`},
		}},
		// The same home written with forward slashes gives the same result.
		{`C:/Users/Jane Q-Doe/`, []tc{
			{`C:\Users\Jane Q-Doe\desktop`, `<HOME>\desktop`},
			{`/c/Users/Jane Q-Doe/desktop`, `<HOME>/desktop`},
		}},
		{"/home/jane", []tc{
			{"/home/jane/src/shop", "<HOME>/src/shop"},
			{`\home\jane\src`, `<HOME>\src`},
			{"/HOME/JANE/x", "<HOME>/x"},
			{"projects/-home-jane-src-shop/s.jsonl", "projects/<HOME>-src-shop/s.jsonl"},
			{"/home/janet/x", "/home/janet/x"},
		}},
		{"/Users/jane", []tc{
			{"/Users/jane/Library/x", "<HOME>/Library/x"},
			{"/Users/jane", "<HOME>"},
		}},
	}
	for _, st := range styles {
		r := NewHomeReplacer(st.home)
		for _, c := range st.cases {
			if got := r.Replace(c.in, "<HOME>"); got != c.want {
				t.Errorf("home %q: Replace(%q) = %q, want %q", st.home, c.in, got, c.want)
			}
		}
	}
	if got := NewHomeReplacer("").Replace("/home/x", "<HOME>"); got != "/home/x" {
		t.Errorf("empty home replaced something: %q", got)
	}
}

func TestReplaceWord(t *testing.T) {
	for _, tt := range [][3]string{
		{"loopguard and loopguard-hook", "loopguard", "project and project-hook"},
		{"LoopGuardX loopguard2 xloopguard", "loopguard", "LoopGuardX loopguard2 xloopguard"},
		{`/a/LOOPGUARD/b and C:\a\LoopGuard\b`, "loopguard", `/a/project/b and C:\a\project\b`},
	} {
		if got := ReplaceWord(tt[0], tt[1], "project"); got != tt[2] {
			t.Errorf("ReplaceWord(%q, %q) = %q, want %q", tt[0], tt[1], got, tt[2])
		}
	}
}
