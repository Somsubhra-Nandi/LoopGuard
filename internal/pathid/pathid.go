// Package pathid compares and displays file system paths. A path's
// identity ignores case where the file system does by default (Windows and
// macOS: C:\Users\me\Desktop\app and c:\users\me\desktop\app are one repo);
// the spelling is kept for display. On screen the user's home directory is
// shown as ~.
package pathid

import (
	"os"
	"path"
	"runtime"
	"strings"
)

// CaseInsensitive reports whether paths on this OS compare without case.
var CaseInsensitive = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// Key is a path's identity on this OS: FoldKey where paths are case-
// insensitive, else the cleaned path.
func Key(p string) string {
	if CaseInsensitive {
		return FoldKey(p)
	}
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

// FoldKey is a path's identity on a case-insensitive file system: forward
// slashes, cleaned, lower case (the drive letter included).
func FoldKey(p string) string {
	if p == "" {
		return ""
	}
	return strings.ToLower(slashed(p, true))
}

func slashed(p string, backslash bool) string {
	if backslash {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	return path.Clean(p)
}

// Same reports whether a and b are the same path on this OS.
func Same(a, b string) bool { return Key(a) == Key(b) }

// Tilde shows p with the user's home directory as ~ (p unchanged when it
// isn't inside it).
func Tilde(p string) string {
	h, _ := os.UserHomeDir()
	return TildeIn(p, h)
}

// TildeIn shows p with home as ~: home itself is "~", a path inside it
// "~/rest" (forward slashes). Home is matched as a path on this OS (case
// ignored on Windows and macOS).
func TildeIn(p, home string) string {
	if p == "" || home == "" {
		return p
	}
	k, hk := Key(p), Key(home)
	switch {
	case k == hk:
		return "~"
	case hk == "/" || !strings.HasPrefix(k, hk+"/"):
		return p
	}
	n := len(strings.Split(strings.Trim(hk, "/"), "/"))
	parts := strings.Split(strings.TrimPrefix(slashed(p, CaseInsensitive), "/"), "/")
	if len(parts) <= n {
		return p
	}
	return "~/" + strings.Join(parts[n:], "/")
}
