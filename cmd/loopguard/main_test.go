package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// When set, the test binary behaves like the real loopguard binary, so tests
// can check real process exit codes and stdout.
const runMainEnv = "LOOPGUARD_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
	}
	os.Exit(m.Run())
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func hookInputs(t *testing.T) map[string][]byte {
	return map[string][]byte{
		"random bytes": randomBytes(t, 64*1024),
		"empty":        {},
		"5 MB":         randomBytes(t, 5*1024*1024),
	}
}

func TestHookProcessExitsZeroWithEmptyStdout(t *testing.T) {
	for name, input := range hookInputs(t) {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "hook", "--agent", "claude-code")
			cmd.Env = append(os.Environ(), runMainEnv+"=1")
			cmd.Stdin = bytes.NewReader(input)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			if err := cmd.Run(); err != nil {
				t.Fatalf("hook process failed: %v (stderr: %q)", err, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestHookReadsAllOfStdin(t *testing.T) {
	for name, input := range hookInputs(t) {
		t.Run(name, func(t *testing.T) {
			in := &countingReader{r: bytes.NewReader(input)}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"hook", "--agent", "codex"}, in, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if in.n != len(input) {
				t.Errorf("read %d bytes of stdin, want %d", in.n, len(input))
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("stdout = %q, stderr = %q, want both empty", stdout.String(), stderr.String())
			}
		})
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("boom") }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("stdin closed") }

func TestHookFailsOpen(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin io.Reader
	}{
		{"panic while reading stdin", []string{"hook", "--agent", "claude-code"}, panicReader{}},
		{"stdin read error", []string{"hook", "--agent", "claude-code"}, errReader{}},
		{"unknown flag", []string{"hook", "--no-such-flag"}, strings.NewReader("{}")},
		{"missing flag value", []string{"hook", "--agent"}, strings.NewReader("{}")},
		{"unknown agent", []string{"hook", "--agent", "some-future-agent"}, strings.NewReader("{}")},
		{"no agent flag", []string{"hook"}, strings.NewReader("{}")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, tc.stdin, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("stdout = %q, stderr = %q, want both empty", stdout.String(), stderr.String())
			}
		})
	}
}

func TestStubSubcommands(t *testing.T) {
	cases := []struct {
		args      []string
		milestone string
	}{
		{[]string{"daemon"}, "M2"},
		{[]string{"init"}, "M2"},
		{[]string{"uninstall"}, "M2"},
		{[]string{"doctor"}, "M2"},
		{[]string{"status"}, "M2"},
		{[]string{"report"}, "M4"},
		{[]string{"report", "abc123"}, "M4"},
		{[]string{"pause"}, "M3"},
		{[]string{"pause", "abc123"}, "M3"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
			}
			want := "not implemented yet (milestone " + tc.milestone + ")\n"
			if stdout.String() != want {
				t.Errorf("stdout = %q, want %q", stdout.String(), want)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	oldVersion, oldCommit, oldDate := version, commit, date
	t.Cleanup(func() { version, commit, date = oldVersion, oldCommit, oldDate })
	version, commit, date = "v1.2.3", "abc1234", "2026-10-19T00:00:00Z"

	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	for _, want := range []string{"v1.2.3", "abc1234", "2026-10-19T00:00:00Z"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("version output %q is missing %q", stdout.String(), want)
		}
	}
}

func TestRootAndHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"hook", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Errorf("run(%q) exit code = %d, want 0 (stderr: %q)", args, code, stderr.String())
		}
	}
}

func TestUnknownCommandFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"no-such-command"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("stderr = %q, want an unknown command error", stderr.String())
	}
}
