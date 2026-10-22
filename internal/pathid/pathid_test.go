package pathid

import "testing"

func TestFoldKey(t *testing.T) {
	same := [][2]string{
		{`C:\Users\me\desktop\loopguard`, `C:\Users\me\Desktop\loopguard`},
		{`C:\Users\me\app`, `c:\users\me\app`},
		{`c:/Users/me/app/`, `C:\Users\me\app`},
		{`/Users/me/Code/App`, `/users/me/code/app`},
	}
	for _, p := range same {
		if FoldKey(p[0]) != FoldKey(p[1]) {
			t.Errorf("FoldKey(%q) = %q, FoldKey(%q) = %q; want equal", p[0], FoldKey(p[0]), p[1], FoldKey(p[1]))
		}
	}
	if FoldKey(`C:\a\app`) == FoldKey(`C:\a\app2`) || FoldKey("") != "" {
		t.Error("FoldKey merges different paths")
	}
}

func TestTildeIn(t *testing.T) {
	home := "/home/me"
	if CaseInsensitive {
		home = `C:\Users\Me Too`
	}
	cases := map[string]string{
		home:                       "~",
		home + "/.claude/projects": "~/.claude/projects",
		"/elsewhere/x":             "/elsewhere/x",
		home + "2/x":               home + "2/x",
		"":                         "",
	}
	if CaseInsensitive {
		cases[`c:\users\me too\.claude\projects\C--x`] = "~/.claude/projects/C--x"
		cases[`C:\Users\Me Too\Desktop\app`] = "~/Desktop/app"
	}
	for p, want := range cases {
		if got := TildeIn(p, home); got != want {
			t.Errorf("TildeIn(%q, %q) = %q, want %q", p, home, got, want)
		}
	}
	if got := TildeIn("/x", ""); got != "/x" {
		t.Errorf("no home: %q", got)
	}
}
