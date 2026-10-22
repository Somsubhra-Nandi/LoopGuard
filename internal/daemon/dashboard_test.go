package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestDashboardThroughTheDaemon: the login link comes only from the token
// route, signs one browser in once, and the pages and actions work on the
// daemon's port.
func TestDashboardThroughTheDaemon(t *testing.T) {
	d := startDaemon(t, homeWithConfig(t, "mode: guard\n"), nil)
	d.event(payload("SessionStart", "dash-1", `"source":"startup"`))
	failingRun(d, "dash-1", 1)

	// The link: POST with the API token only.
	if code, _ := d.do("POST", "/v1/dashboard/login", nil, map[string]string{"Authorization": "Bearer nope"}); code != http.StatusUnauthorized {
		t.Errorf("login link with a bad token: %d", code)
	}
	code, b := d.do("POST", "/v1/dashboard/login", nil, nil)
	var link LoginLink
	if err := json.Unmarshal(b, &link); err != nil || code != http.StatusOK {
		t.Fatalf("login link: %d %s", code, b)
	}
	if !strings.HasPrefix(link.URL, fmt.Sprintf("http://127.0.0.1:%d/ui/login?code=", d.info.Port)) || time.Until(link.ExpiresAt) > 2*time.Minute {
		t.Errorf("link %+v", link)
	}

	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{Proxy: nil}}
	get := func(u string, ck *http.Cookie) (int, string) {
		req, _ := http.NewRequest("GET", u, nil)
		if ck != nil {
			req.AddCookie(ck)
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := get(d.url("/ui/"), nil); code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d", code)
	}
	// The bearer token doesn't open the dashboard.
	if code, _ := d.do("GET", "/ui/", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("API token on /ui/: %d", code)
	}
	req, _ := http.NewRequest("GET", link.URL, nil)
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		ck = c
	}
	if resp.StatusCode != http.StatusSeeOther || ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("login: %d %+v", resp.StatusCode, ck)
	}
	if code, _ := get(link.URL, nil); code != http.StatusForbidden {
		t.Errorf("link used twice: %d", code)
	}
	code, body := get(d.url("/ui/"), ck)
	if code != http.StatusOK || !strings.Contains(body, "Last 30 days") {
		t.Fatalf("overview: %d", code)
	}
	code, body = get(d.url("/ui/sessions/dash-1"), ck)
	if code != http.StatusOK || !strings.Contains(body, "Stop its turn") || !strings.Contains(body, "guard") {
		t.Fatalf("session page: %d\n%s", code, body)
	}

	// DNS rebinding: another Host, on the daemon's real socket.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", d.info.Port))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /ui/ HTTP/1.1\r\nHost: evil.example:%d\r\nCookie: %s=%s\r\nConnection: close\r\n\r\n", d.info.Port, ck.Name, ck.Value)
	status, _ := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if !strings.Contains(status, "403") {
		t.Errorf("rebinding Host: %q", status)
	}

	// Pause the session from the dashboard: the daemon applies it at once.
	tok := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no CSRF token on the page")
	}
	post := func(path string, form url.Values) (int, string) {
		form.Set("csrf", tok[1])
		req, _ := http.NewRequest("POST", d.url(path), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", d.info.Port))
		req.AddCookie(ck)
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post("/ui/action", url.Values{"action": {"pause-session"}, "target": {"dash-1"}, "for": {"30m"}}); code != 200 || !strings.Contains(body, "Paused session") {
		t.Fatalf("pause: %d %s", code, body)
	}
	_, pb := d.do("GET", "/v1/pauses", nil, nil)
	if !strings.Contains(string(pb), `"dash-1"`) {
		t.Errorf("the daemon didn't pick up the pause: %s", pb)
	}
	// Paused, the session observes, so a stop is refused.
	failingRun(d, "dash-1", 2)
	if code, body := post("/ui/action", url.Values{"action": {"stop"}, "target": {"dash-1"}}); code != http.StatusBadRequest || !strings.Contains(body, "observes only") {
		t.Errorf("stop of a paused session: %d %s", code, body)
	}
	if code, body := post("/ui/action", url.Values{"action": {"resume-session"}, "target": {"dash-1"}}); code != 200 || !strings.Contains(body, "Resumed") {
		t.Fatalf("resume: %d %s", code, body)
	}
	failingRun(d, "dash-1", 3)
	if code, body := post("/ui/action", url.Values{"action": {"stop"}, "target": {"dash-1"}}); code != 200 || !strings.Contains(body, "next tool call") {
		t.Errorf("stop: %d %s", code, body)
	}
	pre := d.event(payload("PreToolUse", "dash-1", `"tool_name":"Bash","tool_use_id":"toolu_9","tool_input":{"command":"ls"}`))
	if !strings.Contains(pre.Stdout, "Stopped by the user") {
		t.Errorf("the next tool call wasn't stopped: %s", pre.Stdout)
	}
}
