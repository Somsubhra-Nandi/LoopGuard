package redact

import (
	"strings"
	"testing"
)

// All secrets below are fake: well-formed but random.
func TestString(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"aws access key", "aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "aws_access_key_id = <REDACTED:aws_access_key>"},
		{"aws session key", "key ASIAZZZZ5EXAMPLE1234 used", "key <REDACTED:aws_access_key> used"},
		{"anthropic", "ANTHROPIC_API_KEY used sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx_yz-0123456789 here", "ANTHROPIC_API_KEY used <REDACTED:anthropic_key> here"},
		{"openai", "export OPENAI=sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz012345", "export OPENAI=<REDACTED:openai_key>"},
		{"github classic", "token ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 ok", "token <REDACTED:github_token> ok"},
		{"github fine-grained", "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz", "<REDACTED:github_token>"},
		{"slack", "SLACK=xoxb-123456789012-abcdefghijkl", "SLACK=<REDACTED:slack_token>"},
		{"google", "key=AIzaSyA-abcdefghijklmnopqrstuvwxyz01234", "key=<REDACTED:google_api_key>"},
		{"env var reference kept", "export API_TOKEN=$VAULT_TOKEN", "export API_TOKEN=$VAULT_TOKEN"},
		{"jwt", "Cookie: session=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U;",
			"Cookie: session=<REDACTED:jwt>;"},
		{"bearer", `curl -H "Authorization: Bearer abc123def456ghi789jkl" https://api.x`, `curl -H "Authorization: Bearer <REDACTED:bearer>" https://api.x`},
		{"url credentials", "git clone https://alice:hunter2pass@github.com/x/y.git", "git clone https://alice:<REDACTED:url_credentials>github.com/x/y.git"},
		{"password kv", "connecting with password=hunter2!x", "connecting with password=<REDACTED:secret>"},
		{"json secret", `{"client_secret": "s3cr3t-value-123"}`, `{"client_secret": "<REDACTED:secret>"}`},
		{"token colon", "token: abcd1234efgh", "token: <REDACTED:secret>"},
		{"env line", "DB_PASSWORD=correct-horse\nPORT=8080", "DB_PASSWORD=<REDACTED:env_secret>\nPORT=8080"},
		{"env export quoted", `export STRIPE_SECRET_KEY="sk_live_abc 123"`, `export STRIPE_SECRET_KEY="<REDACTED:env_secret>"`},
		{"private key", "x\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\nabc\n-----END RSA PRIVATE KEY-----\ny", "x\n<REDACTED:private_key>\ny"},
		{"truncated private key", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAA", "<REDACTED:private_key>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := String(tt.in); got != tt.want {
				t.Errorf("String(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStringLeavesNormalTextAlone(t *testing.T) {
	for _, s := range []string{
		"KeyError: 'user'",
		"--- FAIL: TestToken (0.00s)",
		"password field is required",
		"tokens: 1234 input, 56 output",
		"const apiKey = process.env.API_KEY;",
		"see https://example.com/docs",
		"sk-learn is a library",
		"PORT=8080",
		"",
	} {
		if got := String(s); got != s {
			t.Errorf("String(%q) = %q, want unchanged", s, got)
		}
	}
}

func TestNoSecretSurvives(t *testing.T) {
	secrets := []string{
		"AKIAIOSFODNN7EXAMPLE", "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx", "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"hunter2!x", "correct-horse",
	}
	text := "AKIAIOSFODNN7EXAMPLE sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx\nghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 password=hunter2!x\nDB_PASS=correct-horse\n"
	out := String(text)
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("secret %q survived: %q", s, out)
		}
	}
	if got := Lines([]string{"password=hunter2!x", "ok"}); got[0] != "password=<REDACTED:secret>" || got[1] != "ok" {
		t.Errorf("Lines = %q", got)
	}
}
