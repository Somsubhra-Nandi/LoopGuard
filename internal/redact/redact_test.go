package redact

import (
	"strings"
	"testing"
)

// Fake secrets: well-formed but random. Each is assembled from pieces so the
// full token never appears in source, which keeps secret scanners quiet.
var (
	fakeAWSKey       = "AK" + "IA" + "IOSFODNN7EXAMPLE"
	fakeAWSSession   = "AS" + "IA" + "ZZZZ5EXAMPLE1234"
	fakeAnthropicKey = "sk-" + "ant-" + "api03-AbCdEfGhIjKlMnOpQrStUvWx_yz-0123456789"
	fakeOpenAIKey    = "sk-" + "proj-" + "AbCdEfGhIjKlMnOpQrStUvWxYz012345"
	fakeGitHubToken  = "gh" + "p_" + "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	fakeGitHubPAT    = "github" + "_pat_" + "11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz"
	fakeSlackToken   = "xo" + "xb-" + "123456789012-abcdefghijkl"
	fakeGoogleKey    = "AI" + "za" + "SyA-abcdefghijklmnopqrstuvwxyz01234"
	fakeJWT          = "ey" + "JhbGciOiJIUzI1NiJ9" + "." + "ey" + "JzdWIiOiIxMjM0NTY3ODkwIn0" + "." + "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	fakeStripeKey    = "sk" + "_live_" + "abc 123"
)

// pemBegin and pemEnd return fake PEM private-key header and footer lines.
func pemBegin(kind string) string { return "-----BEGIN " + kind + " PRIVATE" + " KEY-----" }
func pemEnd(kind string) string   { return "-----END " + kind + " PRIVATE" + " KEY-----" }

func TestString(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"aws access key", "aws_access_key_id = " + fakeAWSKey, "aws_access_key_id = <REDACTED:aws_access_key>"},
		{"aws session key", "key " + fakeAWSSession + " used", "key <REDACTED:aws_access_key> used"},
		{"anthropic", "ANTHROPIC_API_KEY used " + fakeAnthropicKey + " here", "ANTHROPIC_API_KEY used <REDACTED:anthropic_key> here"},
		{"openai", "export OPENAI=" + fakeOpenAIKey, "export OPENAI=<REDACTED:openai_key>"},
		{"github classic", "token " + fakeGitHubToken + " ok", "token <REDACTED:github_token> ok"},
		{"github fine-grained", fakeGitHubPAT, "<REDACTED:github_token>"},
		{"slack", "SLACK=" + fakeSlackToken, "SLACK=<REDACTED:slack_token>"},
		{"google", "key=" + fakeGoogleKey, "key=<REDACTED:google_api_key>"},
		{"env var reference kept", "export API_TOKEN=$VAULT_TOKEN", "export API_TOKEN=$VAULT_TOKEN"},
		{"jwt", "Cookie: session=" + fakeJWT + ";", "Cookie: session=<REDACTED:jwt>;"},
		{"bearer", `curl -H "Authorization: Bearer abc123def456ghi789jkl" https://api.x`, `curl -H "Authorization: Bearer <REDACTED:bearer>" https://api.x`},
		{"url credentials", "git clone https://alice:hunter2pass@github.com/x/y.git", "git clone https://alice:<REDACTED:url_credentials>github.com/x/y.git"},
		{"password kv", "connecting with password=hunter2!x", "connecting with password=<REDACTED:secret>"},
		{"json secret", `{"client_secret": "s3cr3t-value-123"}`, `{"client_secret": "<REDACTED:secret>"}`},
		{"token colon", "token: abcd1234efgh", "token: <REDACTED:secret>"},
		{"env line", "DB_PASSWORD=correct-horse\nPORT=8080", "DB_PASSWORD=<REDACTED:env_secret>\nPORT=8080"},
		{"env export quoted", `export STRIPE_SECRET_KEY="` + fakeStripeKey + `"`, `export STRIPE_SECRET_KEY="<REDACTED:env_secret>"`},
		{"private key", "x\n" + pemBegin("RSA") + "\nMIIEpAIBAAKCAQEA\nabc\n" + pemEnd("RSA") + "\ny", "x\n<REDACTED:private_key>\ny"},
		{"truncated private key", pemBegin("OPENSSH") + "\nb3BlbnNzaC1rZXktdjEAAAA", "<REDACTED:private_key>"},
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
	secrets := []string{fakeAWSKey, fakeAnthropicKey, fakeGitHubToken, "hunter2!x", "correct-horse"}
	text := fakeAWSKey + " " + fakeAnthropicKey + "\n" + fakeGitHubToken + " password=hunter2!x\nDB_PASS=correct-horse\n"
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
