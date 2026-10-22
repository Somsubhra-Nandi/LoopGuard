package dashboard

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuth(t *testing.T) {
	f := newFixture(t)
	f.d.auth = newAuth(rand.Reader, f.d.o.Now) // real tokens
	anon := &client{t: t, d: f.d}

	// No cookie: 401 on every page and API route; the stylesheet is public.
	for _, p := range []string{"/ui/", "/ui/sessions", "/ui/sessions/" + sidShop, "/ui/costs", "/ui/settings", "/ui/api/overview", "/ui/nope"} {
		if r := anon.get(p); r.Code != http.StatusUnauthorized || !strings.Contains(r.Body.String(), "loopguard dashboard") {
			t.Errorf("GET %s without a cookie: %d", p, r.Code)
		}
	}
	if r := anon.get("/ui/static/style.css"); r.Code != http.StatusOK || !strings.Contains(r.Header().Get("Content-Type"), "text/css") {
		t.Errorf("stylesheet: %d %s", r.Code, r.Header().Get("Content-Type"))
	}
	// A made-up cookie is no better.
	fake := &client{t: t, d: f.d, cookie: &http.Cookie{Name: cookieName, Value: "guess"}}
	if r := fake.get("/ui/"); r.Code != http.StatusUnauthorized {
		t.Errorf("made-up cookie: %d", r.Code)
	}

	// Bad Host: 403 whatever else (DNS rebinding), even when signed in.
	c := f.login()
	for _, host := range []string{"evil.example:47821", "127.0.0.1:1234", "127.0.0.1", "localhost", "127.0.0.1.nip.io:47821", "[::1]:47821"} {
		req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
		req.Host = host
		req.AddCookie(c.cookie)
		rec := httptest.NewRecorder()
		f.d.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: %d", host, rec.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:47821", "localhost:47821", "LOCALHOST:47821"} {
		req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
		req.Host = host
		req.AddCookie(c.cookie)
		rec := httptest.NewRecorder()
		f.d.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: %d", host, rec.Code)
		}
	}

	// The login link: once, within 2 minutes; the cookie is HttpOnly,
	// SameSite=Strict, and scoped to the dashboard.
	link, exp, err := f.d.NewLoginLink()
	if err != nil || !strings.HasPrefix(link, "http://127.0.0.1:47821/ui/login?code=") || exp.Sub(at) != LoginTTL {
		t.Fatalf("link %q %v %v", link, exp, err)
	}
	path := strings.TrimPrefix(link, "http://127.0.0.1:47821")
	r := anon.get(path)
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/ui/" {
		t.Fatalf("first use: %d", r.Code)
	}
	ck := r.Result().Cookies()[0]
	if ck.Name != cookieName || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/ui" || ck.Value == "" {
		t.Errorf("cookie %+v", ck)
	}
	if r := anon.get(path); r.Code != http.StatusForbidden {
		t.Errorf("reused link: %d", r.Code)
	}
	if r := anon.get("/ui/login?code=nope"); r.Code != http.StatusForbidden {
		t.Errorf("bad code: %d", r.Code)
	}
	if r := anon.get("/ui/login"); r.Code != http.StatusForbidden {
		t.Errorf("no code: %d", r.Code)
	}
	link, _, _ = f.d.NewLoginLink()
	f.setClock(at.Add(LoginTTL + time.Second))
	if r := anon.get(strings.TrimPrefix(link, "http://127.0.0.1:47821")); r.Code != http.StatusForbidden {
		t.Errorf("expired link: %d", r.Code)
	}
	f.setClock(at)

	// Headers: a strict CSP without inline scripts or other origins, no
	// framing, and never any CORS header.
	r = c.get("/ui/")
	csp := r.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "style-src 'self'", "frame-ancestors 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe") || strings.Contains(csp, "http") || r.Header().Get("X-Frame-Options") != "DENY" ||
		r.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", r.Header())
	}
	if strings.Contains(r.Body.String(), "<script") || strings.Contains(r.Body.String(), " style=") {
		t.Error("a page has a script or an inline style")
	}
	pre := c.do(http.MethodOptions, "/ui/", nil, map[string]string{"Origin": "http://evil.example", "Access-Control-Request-Method": "POST"})
	for _, rec := range []*httptest.ResponseRecorder{r, pre} {
		for k := range rec.Header() {
			if strings.HasPrefix(k, "Access-Control") {
				t.Errorf("CORS header %s", k)
			}
		}
	}

	// State-changing requests: POST only, with the session's CSRF token,
	// from the dashboard's own origin.
	form := func(tok string) url.Values {
		return url.Values{"csrf": {tok}, "action": {actPauseSession}, "target": {sidShop}, "for": {"2h"}, "back": {"/ui/"}}
	}
	tok := c.csrfToken()
	cases := []struct {
		name string
		form url.Values
		hdr  map[string]string
		want int
	}{
		{"no token", form(""), nil, http.StatusForbidden},
		{"wrong token", form("x" + tok[1:]), nil, http.StatusForbidden},
		{"other origin", form(tok), map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"other port", form(tok), map[string]string{"Origin": "http://127.0.0.1:9999"}, http.StatusForbidden},
		{"cross-site fetch", form(tok), map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"good", form(tok), map[string]string{"Origin": "http://127.0.0.1:47821", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
	}
	for _, tc := range cases {
		for _, p := range []string{"/ui/confirm", "/ui/action"} {
			if r := c.do(http.MethodPost, p, tc.form, tc.hdr); r.Code != tc.want {
				t.Errorf("%s %s: %d, want %d", tc.name, p, r.Code, tc.want)
			}
		}
	}
	if r := anon.do(http.MethodPost, "/ui/action", form(tok), nil); r.Code != http.StatusUnauthorized {
		t.Errorf("POST without a cookie: %d", r.Code)
	}
	// A GET never acts (it finds no page).
	reloads := f.be.reloads
	if r := c.get("/ui/action?" + form(tok).Encode()); r.Code != http.StatusNotFound || f.be.reloads != reloads {
		t.Errorf("GET /ui/action: %d", r.Code)
	}

	// Signing out ends the session.
	if r := c.do(http.MethodPost, "/ui/logout", url.Values{"csrf": {tok}}, nil); r.Code != http.StatusOK {
		t.Fatalf("logout: %d", r.Code)
	}
	if r := c.get("/ui/"); r.Code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", r.Code)
	}

	// Sessions expire.
	c = f.login()
	f.setClock(at.Add(SessionTTL + time.Minute))
	if r := c.get("/ui/"); r.Code != http.StatusUnauthorized {
		t.Errorf("after %v: %d", SessionTTL, r.Code)
	}
}
