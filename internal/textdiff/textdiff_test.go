package textdiff

import "testing"

func TestUnified(t *testing.T) {
	if d := Unified("a\nb\n", "a\r\nb\r\n", "x", "y", 3); d != "" {
		t.Errorf("line endings only: %q", d)
	}
	a := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3,\n  \"d\": 4,\n  \"e\": 5,\n  \"f\": 6,\n  \"g\": 7\n}\n"
	b := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3,\n  \"d\": 4,\n  \"e\": 5,\n  \"f\": 6,\n  \"g\": 7,\n  \"h\": 8\n}\n"
	want := "--- old\n+++ new\n@@ -5,5 +5,6 @@\n   \"d\": 4,\n   \"e\": 5,\n   \"f\": 6,\n-  \"g\": 7\n+  \"g\": 7,\n+  \"h\": 8\n }\n"
	if d := Unified(a, b, "old", "new", 3); d != want {
		t.Errorf("got:\n%s\nwant:\n%s", d, want)
	}
	// A new file: all additions.
	if d := Unified("", "{\n}\n", "a", "b", 3); d != "--- a\n+++ b\n@@ -0,0 +1,2 @@\n+{\n+}\n" {
		t.Errorf("new file:\n%s", d)
	}
	// Two distant changes make two hunks.
	x := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	y := "1x\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12x\n"
	want = "--- a\n+++ b\n@@ -1,4 +1,4 @@\n-1\n+1x\n 2\n 3\n 4\n@@ -9,4 +9,4 @@\n 9\n 10\n 11\n-12\n+12x\n"
	if d := Unified(x, y, "a", "b", 3); d != want {
		t.Errorf("two hunks:\n%s\nwant:\n%s", d, want)
	}
}
