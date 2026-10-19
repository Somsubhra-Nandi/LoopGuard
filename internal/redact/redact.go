package redact

import (
	"regexp"
	"strings"
)

// Placeholder returns the text that replaces a secret of the given type.
func Placeholder(typ string) string { return "<REDACTED:" + typ + ">" }

// rule is one secret pattern. The regexp only runs when the text contains
// one of the prefilter substrings, which keeps redaction cheap on the
// common case of text without secrets. If the regexp has a capture group
// named "keep", that part of the match is kept and the rest replaced (for
// "password=..." the key stays, the value goes).
type rule struct {
	typ       string
	prefilter []string
	// fold matches the prefilter against the lower-cased text.
	fold bool
	// need, if set, must also appear in the text.
	need string
	// check, if set, replaces the substring prefilter.
	check func(s string) bool
	re    *regexp.Regexp
	// skip, if set, keeps matches whose value (the part after "keep") is
	// not a secret.
	skip func(value string) bool
}

// codeRefRe matches values that refer to a secret rather than contain one:
// dotted identifiers (process.env.API_KEY, settings.token), shell and
// template variables ($TOKEN, ${TOKEN}, {{ token }}), and placeholders.
var codeRefRe = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+(?:\(\))?|\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|\{\{.*|<[^>]*>?|\*+|x{4,}|null|none|true|false|undefined)$`)

func isCodeRef(v string) bool {
	return codeRefRe.MatchString(strings.ToLower(v)) || codeRefRe.MatchString(v)
}

// Rules in order: specific token formats first, generic key=value last, so
// a known token inside a key=value pair gets its specific type.
var rules = []rule{
	mk("private_key", []string{"PRIVATE KEY"},
		`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`, nil),
	mk("anthropic_key", []string{"sk-ant-"}, `\bsk-ant-[A-Za-z0-9_-]{16,}`, nil),
	mk("openai_key", []string{"sk-"}, `\bsk-(?:proj-|svcacct-|None-)?[A-Za-z0-9_-]{20,}`, nil),
	mk("aws_access_key", []string{"AKIA", "ASIA"}, `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, nil),
	mk("github_token", []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"},
		`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,})`, nil),
	mk("slack_token", []string{"xox"}, `\bxox[abposr]-[A-Za-z0-9-]{10,}`, nil),
	mk("google_api_key", []string{"AIza"}, `\bAIza[0-9A-Za-z_-]{35}`, nil),
	mk("jwt", []string{"eyJ"}, `\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`, nil),
	mk("bearer", []string{"earer"}, `(?P<keep>(?i:bearer)\s+)[A-Za-z0-9._~+/=-]{16,}`, nil),
	needing("@", mk("url_credentials", []string{"://"}, `(?P<keep>\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@'"]+:)[^\s@/'"]+@`, nil)),
	// .env style: KEY=value where KEY names a secret.
	checked(hasSecretAssignment, mk("env_secret", nil,
		`(?m)(?P<keep>^[ \t]*(?:export[ \t]+)?[A-Z0-9_]*(?:SECRET|TOKEN|KEY|PASS)[A-Z0-9_]*[ \t]*=[ \t]*)[^\s'"]+`+
			`|(?m)(?P<keep2>^[ \t]*(?:export[ \t]+)?[A-Z0-9_]*(?:SECRET|TOKEN|KEY|PASS)[A-Z0-9_]*[ \t]*=[ \t]*["'])[^"'\n]+`,
		isCodeRef)),
	// password=..., token: ..., "secret": "..." and friends.
	folded(mk("secret", []string{"password", "passwd", "pwd", "secret", "token", "api_key", "api-key", "apikey"},
		`(?P<keep>(?i:\b(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret))["']?\s*[:=]\s*["']?)[^\s"',;&}]{4,}`,
		isCodeRef)),
}

func needing(sub string, r rule) rule { r.need = sub; return r }

func checked(f func(string) bool, r rule) rule { r.check = f; return r }

// hasSecretAssignment reports whether s has an "=" directly after an
// upper-case identifier that names a secret (DB_PASSWORD=, API_KEY =).
func hasSecretAssignment(s string) bool {
	for i := strings.IndexByte(s, '='); i >= 0; {
		j := i
		for j > 0 && (s[j-1] == ' ' || s[j-1] == '\t') {
			j--
		}
		k := j
		for k > 0 && (s[k-1] >= 'A' && s[k-1] <= 'Z' || s[k-1] >= '0' && s[k-1] <= '9' || s[k-1] == '_') {
			k--
		}
		if id := s[k:j]; strings.Contains(id, "SECRET") || strings.Contains(id, "TOKEN") || strings.Contains(id, "KEY") || strings.Contains(id, "PASS") {
			return true
		}
		next := strings.IndexByte(s[i+1:], '=')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return false
}

func folded(r rule) rule { r.fold = true; return r }

func mk(typ string, prefilter []string, pattern string, skip func(string) bool) rule {
	return rule{typ: typ, prefilter: prefilter, re: regexp.MustCompile(pattern), skip: skip}
}

// String replaces secrets in s with <REDACTED:type> placeholders (LLD §13).
// Text without secrets is returned unchanged.
func String(s string) string {
	lower := ""
	for i := range rules {
		r := &rules[i]
		if r.need != "" && !strings.Contains(s, r.need) {
			continue
		}
		hay := s
		if r.fold {
			if lower == "" {
				lower = strings.ToLower(s)
			}
			hay = lower
		}
		if r.check != nil {
			if !r.check(s) {
				continue
			}
		} else if !containsAny(hay, r.prefilter) {
			continue
		}
		s = replace(s, r)
		lower = ""
	}
	return s
}

// Lines redacts each line.
func Lines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = String(l)
	}
	return out
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func replace(s string, r *rule) string {
	ph := Placeholder(r.typ)
	names := r.re.SubexpNames()
	return r.re.ReplaceAllStringFunc(s, func(m string) string {
		sub := r.re.FindStringSubmatch(m)
		for i, name := range names {
			if strings.HasPrefix(name, "keep") && i < len(sub) && sub[i] != "" {
				if r.skip != nil && r.skip(m[len(sub[i]):]) {
					return m
				}
				return sub[i] + ph
			}
		}
		if r.skip != nil && r.skip(m) {
			return m
		}
		return ph
	})
}
