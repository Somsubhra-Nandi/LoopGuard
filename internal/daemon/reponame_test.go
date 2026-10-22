package daemon

import (
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/pathid"
)

// TestRepoNames: on a case-insensitive file system, every spelling of a
// repo (desktop/Desktop, C:/c:) is recorded as the one stored first.
func TestRepoNames(t *testing.T) {
	r := repoNames{key: pathid.FoldKey}
	r.load(map[string]string{pathid.FoldKey(`C:\Users\me\Desktop\loopguard`): `C:\Users\me\Desktop\loopguard`})
	for _, p := range []string{`C:\Users\me\desktop\loopguard`, `c:\Users\me\Desktop\loopguard`, `c:/users/me/desktop/loopguard/`} {
		if got := r.name(p); got != `C:\Users\me\Desktop\loopguard` {
			t.Errorf("name(%q) = %q", p, got)
		}
	}
	if got := r.name(`C:\Users\me\desktop\other`); got != `C:\Users\me\desktop\other` {
		t.Errorf("a new repo keeps its spelling: %q", got)
	}
	if got := r.name(`C:\USERS\ME\DESKTOP\OTHER`); got != `C:\Users\me\desktop\other` {
		t.Errorf("then its other spellings map to it: %q", got)
	}
	if r.name("") != "" {
		t.Error("no repo")
	}
}
