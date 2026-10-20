package shim

import (
	"encoding/json"
	"strings"
	"testing"
)

type jsonReply struct {
	Stdout string `json:"stdout"`
	Exit   int    `json:"exit"`
}

func TestDecodeReply(t *testing.T) {
	ok := map[string]jsonReply{
		`{"stdout":"","exit":0}`:  {},
		`{}`:                      {},
		` { "exit" : 3 } `:        {Exit: 3},
		`{"stdout":"x","exit":3}`: {"x", 3},
		`{"stdout":"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\"}}","exit":0}`: {`{"hookSpecificOutput":{"hookEventName":"PreToolUse"}}`, 0},
		`{"stdout":"a\nb\té 😀\/\\"}`:                                                        {"a\nb\té 😀/\\", 0},
		`{"stdout":"lone \ud800 surrogate"}`:                                                {"lone � surrogate", 0},
		`{"stdout":"x","stdout":null,"exit":null}`:                                          {"x", 0},
		`{"STDOUT":"x","Exit":-1}`:                                                          {"x", -1},
		`{"other":{"a":[1,2.5e-3,true,false,null,"s"]},"stdout":"y"}`:                       {"y", 0},
		"{\"stdout\":\"bad \xff byte\"}":                                                    {"bad � byte", 0},
	}
	// \u escapes, a surrogate pair among them ('#' stands for a backslash).
	esc := strings.NewReplacer("#", `\`, "'", `"`).Replace(`{'stdout':'#u00e9#u2028#ud83d#ude00#n#u0041'}`)
	ok[esc] = jsonReply{Stdout: string([]rune{0xe9, 0x2028, 0x1f600, '\n', 'A'})}
	for in, want := range ok {
		stdout, exit, err := decodeReply([]byte(in))
		if err != nil || stdout != want.Stdout || exit != want.Exit {
			t.Errorf("decodeReply(%s) = %q, %d, %v; want %q, %d", in, stdout, exit, err, want.Stdout, want.Exit)
		}
		var std jsonReply
		if err := json.Unmarshal([]byte(in), &std); err != nil || std != want {
			t.Errorf("test case %s: encoding/json says %+v, %v", in, std, err)
		}
	}
	for _, in := range []string{
		``, `null`, `[]`, `{`, `{"stdout":7}`, `{"exit":"0"}`, `{"exit":1.5}`, `{"exit":1e2}`, `{"exit":01}`,
		`{"stdout":"x",}`, `{"stdout":"x" "exit":0}`, `{"stdout":"unterminated}`, "{\"stdout\":\"ctl\x01\"}",
		`{"stdout":"\x"}`, `{"stdout":"\u12"}`, `{"exit":99999999999999999999}`, `{"a":tru}`, `{"a":-}`,
		"\x00\xffnot json", strings.Repeat("[", 100),
		`{"a":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}`,
	} {
		if stdout, exit, err := decodeReply([]byte(in)); err == nil {
			t.Errorf("decodeReply(%q) = %q, %d; want an error", in, stdout, exit)
		}
	}
}

// FuzzDecodeReply checks decodeReply against encoding/json: whatever it
// accepts, encoding/json decodes to the same values.
func FuzzDecodeReply(f *testing.F) {
	for _, s := range []string{`{"stdout":"x","exit":3}`, `{"stdout":"😀"}`, `{"a":[{"b":null}],"exit":-0}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		stdout, exit, err := decodeReply(b)
		if err != nil {
			return
		}
		var std jsonReply
		if err := json.Unmarshal(b, &std); err != nil {
			// encoding/json rejects trailing data; decodeReply stops at the
			// object's end, as json.Decoder did in the old shim.
			if d := json.NewDecoder(strings.NewReader(string(b))); d.Decode(&std) != nil {
				t.Fatalf("decodeReply(%q) = %q, %d; encoding/json: %v", b, stdout, exit, err)
			}
		}
		if std.Stdout != stdout || std.Exit != exit {
			t.Fatalf("decodeReply(%q) = %q, %d; encoding/json = %q, %d", b, stdout, exit, std.Stdout, std.Exit)
		}
	})
}

// FuzzDaemonReplies round-trips what the daemon sends: any stdout encoded
// by encoding/json decodes back unchanged.
func FuzzDaemonReplies(f *testing.F) {
	f.Add("", 0)
	f.Add(`{"continue":false,"stopReason":"x <y> &  "}`, 0)
	f.Fuzz(func(t *testing.T, stdout string, exit int) {
		b, err := json.Marshal(jsonReply{stdout, exit})
		if err != nil {
			t.Skip()
		}
		var want jsonReply
		_ = json.Unmarshal(b, &want) // invalid UTF-8 became U+FFFD
		got, code, err := decodeReply(b)
		if err != nil || got != want.Stdout || code != want.Exit {
			t.Fatalf("decodeReply(%s) = %q, %d, %v; want %q, %d", b, got, code, err, want.Stdout, want.Exit)
		}
	})
}
