package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"
)

// ContentKeys name the fields that carry the user's prompts, code and
// tool output, in hook payloads and in their .golden files. Every string
// below one of them, at any depth, is content.
var ContentKeys = map[string]bool{
	"prompt": true, "tool_input": true, "tool_response": true, "error": true,
	"last_assistant_message": true, "message": true, "content": true,
	"input": true, "output": true, // .golden names for tool_input and the response
}

// keptKeys are the fields whose string values StructureOnly keeps: enum-like
// values and the (already anonymized) ids and paths the adapter maps. Any
// other string, including in fields Claude Code adds later, becomes a
// placeholder.
var keptKeys = map[string]bool{
	"hook_event_name": true, "tool_name": true, "permission_mode": true, "source": true,
	"reason": true, "model": true, "id": true, "display_name": true, "level": true,
	"notification_type": true, "trigger": true, "matcher": true, "agent_type": true,
	"session_id": true, "prompt_id": true, "tool_use_id": true, "agent_id": true,
	"cwd": true, "transcript_path": true, "scratchpad_dir": true,
}

// StructureOnly returns v with every key, the nesting and every value's
// type kept, but each string replaced by "<str:N>" (N its length in
// characters), except the strings of keptKeys outside ContentKeys.
func StructureOnly(v any) any {
	return structure(v, "", false)
}

func structure(v any, key string, content bool) any {
	switch t := v.(type) {
	case string:
		if !content && keptKeys[key] {
			return t
		}
		return fmt.Sprintf("<str:%d>", utf8.RuneCountInString(t))
	case []any:
		for i := range t {
			t[i] = structure(t[i], key, content)
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = structure(val, k, content || ContentKeys[k])
		}
		return t
	}
	return v
}

// MaxContentString is the longest string CheckNoContent allows in a
// content field: room for a hand-written synthetic value or a placeholder,
// not for a real prompt or tool output.
const MaxContentString = 40

// CheckNoContent returns a description of each string in a ContentKeys
// field of the JSON document b that is longer than MaxContentString
// characters. It is the safety net for committed payloads.
func CheckNoContent(b []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var bad []string
	var walk func(v any, path string, content bool)
	walk = func(v any, path string, content bool) {
		switch t := v.(type) {
		case string:
			if n := utf8.RuneCountInString(t); content && n > MaxContentString {
				bad = append(bad, fmt.Sprintf("%s: %d-character string", path, n))
			}
		case []any:
			for i, c := range t {
				walk(c, fmt.Sprintf("%s[%d]", path, i), content)
			}
		case map[string]any:
			for k, c := range t {
				walk(c, path+"."+k, content || ContentKeys[k])
			}
		}
	}
	walk(v, "", false)
	sort.Strings(bad)
	return bad, nil
}
