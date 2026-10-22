package dashboard

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Login and session limits.
const (
	LoginTTL    = 2 * time.Minute // a login link's life (single use)
	SessionTTL  = 12 * time.Hour  // a signed-in browser's life
	maxCodes    = 16              // outstanding login links
	maxSessions = 32
	cookieName  = "loopguard_dashboard"
	maxForm     = 16 << 10
)

// uiSession is a signed-in browser.
type uiSession struct {
	CSRF    string
	expires time.Time
	key     string
}

// authState holds the login codes and sessions, in memory only (a daemon
// restart signs everyone out). Only hashes of the secrets are kept.
type authState struct {
	rand io.Reader
	now  func() time.Time

	mu       sync.Mutex
	codes    map[string]time.Time  // hash → expiry
	sessions map[string]*uiSession // hash of the cookie → session
}

func newAuth(r io.Reader, now func() time.Time) *authState {
	return &authState{rand: r, now: now, codes: map[string]time.Time{}, sessions: map[string]*uiSession{}}
}

func (a *authState) token() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(a.rand, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return string(h[:])
}

func (a *authState) prune(now time.Time) {
	for h, exp := range a.codes {
		if !now.Before(exp) {
			delete(a.codes, h)
		}
	}
	for h, s := range a.sessions {
		if !now.Before(s.expires) {
			delete(a.sessions, h)
		}
	}
}

// newCode makes a login code valid for LoginTTL.
func (a *authState) newCode() (string, time.Time, error) {
	code, err := a.token()
	if err != nil {
		return "", time.Time{}, err
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prune(now)
	if len(a.codes) >= maxCodes {
		return "", time.Time{}, errors.New("too many unused login links; wait two minutes")
	}
	exp := now.Add(LoginTTL)
	a.codes[hash(code)] = exp
	return code, exp, nil
}

// redeem uses up a login code and starts a session; it returns the cookie
// value. A code works once, within LoginTTL.
func (a *authState) redeem(code string) (string, *uiSession, bool) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prune(now)
	h := hash(code)
	if _, ok := a.codes[h]; !ok || code == "" {
		return "", nil, false
	}
	delete(a.codes, h)
	id, err1 := a.token()
	csrf, err2 := a.token()
	if err1 != nil || err2 != nil {
		return "", nil, false
	}
	if len(a.sessions) >= maxSessions { // drop the oldest
		var oldest string
		for k, s := range a.sessions {
			if oldest == "" || s.expires.Before(a.sessions[oldest].expires) {
				oldest = k
			}
		}
		delete(a.sessions, oldest)
	}
	s := &uiSession{CSRF: csrf, expires: now.Add(SessionTTL), key: hash(id)}
	a.sessions[s.key] = s
	return id, s, true
}

func (a *authState) lookup(cookie string) (*uiSession, bool) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[hash(cookie)]
	if !ok || !now.Before(s.expires) {
		return nil, false
	}
	return s, true
}

func (a *authState) end(s *uiSession) {
	a.mu.Lock()
	delete(a.sessions, s.key)
	a.mu.Unlock()
}

// login redeems a one-time link: it sets the session cookie (HttpOnly,
// SameSite=Strict, the dashboard's path only) and goes to the overview.
func (d *Dashboard) login(w http.ResponseWriter, r *http.Request) {
	id, _, ok := d.auth.redeem(r.URL.Query().Get("code"))
	if !ok {
		d.errorPage(w, r, http.StatusForbidden, "This login link doesn't work",
			"Each link works once, for 2 minutes. Run loopguard dashboard for a new one.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/ui", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(SessionTTL / time.Second)})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// authed requires a signed-in browser: 401 without a valid cookie.
func (d *Dashboard) authed(h func(w http.ResponseWriter, r *http.Request, s *uiSession)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		var s *uiSession
		ok := err == nil
		if ok {
			s, ok = d.auth.lookup(c.Value)
		}
		if !ok {
			d.errorPage(w, r, http.StatusUnauthorized, "Not signed in",
				"Run loopguard dashboard: it opens a one-time login link (it works once, for 2 minutes).")
			return
		}
		h(w, r, s)
	})
}

// csrf guards a state-changing POST: the form's token must be the
// session's, and a browser's Origin and Sec-Fetch-Site must say the
// request comes from the dashboard itself. 403 otherwise.
func (d *Dashboard) csrf(h func(w http.ResponseWriter, r *http.Request, s *uiSession)) func(http.ResponseWriter, *http.Request, *uiSession) {
	return func(w http.ResponseWriter, r *http.Request, s *uiSession) {
		r.Body = http.MaxBytesReader(w, r.Body, maxForm)
		err := r.ParseForm()
		tok := r.PostForm.Get("csrf")
		bad := err != nil || tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.CSRF)) != 1
		if o := r.Header.Get("Origin"); o != "" && !d.sameOrigin(o) {
			bad = true
		}
		if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" {
			bad = true
		}
		if bad {
			d.errorPage(w, r, http.StatusForbidden, "Request refused",
				"The form's security token is missing or doesn't match. Reload the page and try again.")
			return
		}
		h(w, r, s)
	}
}

func (d *Dashboard) sameOrigin(o string) bool {
	u, err := url.Parse(o)
	return err == nil && u.Scheme == "http" && u.Path == "" && d.goodHost(u.Host)
}

func (d *Dashboard) logout(w http.ResponseWriter, r *http.Request, s *uiSession) {
	d.auth.end(s)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/ui", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	d.errorPage(w, r, http.StatusOK, "Signed out", "Run loopguard dashboard to sign in again.")
}
