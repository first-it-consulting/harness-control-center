package control

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Session lifetime and limits.
const (
	SessionTTL  = 12 * time.Hour
	maxSessions = 64
	cookieName  = "hcc_session"
	csrfHeader  = "X-HCC-CSRF"
)

// auth guards the UI. The Control Center listens on loopback only, but
// any local process and any web page the user visits can talk to
// loopback, so:
//
//   - the Host header must name this server (no DNS rebinding),
//   - every API call needs a session, created only by opening the login
//     URL with the secret token printed at startup (other local users
//     and web pages cannot know it),
//   - the session cookie is HttpOnly and SameSite=Strict, and every
//     change also needs the session's CSRF token in a header and a
//     same-origin Origin header.
type auth struct {
	loginToken string
	now        func() time.Time

	mu       sync.Mutex
	hosts    []string // allowed Host headers
	sessions map[string]session
}

type session struct {
	csrf    string
	expires time.Time
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

func newAuth() *auth {
	return &auth{loginToken: randomToken(), now: time.Now, sessions: map[string]session{}}
}

func (a *auth) setHosts(hosts []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hosts = slices.Clone(hosts)
}

func (a *auth) hostAllowed(host string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.hosts, host)
}

// login exchanges the login token for a new session.
func (a *auth) login(token string) (id string, ok bool) {
	if subtle.ConstantTimeCompare([]byte(token), []byte(a.loginToken)) != 1 {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, k)
		}
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
	id = randomToken()
	a.sessions[id] = session{csrf: randomToken(), expires: now.Add(SessionTTL)}
	return id, true
}

// session returns the request's live session.
func (a *auth) session(r *http.Request) (session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || a.now().After(s.expires) {
		return session{}, false
	}
	return s, true
}

func (a *auth) logout(r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
}

// securityHeaders are sent with every response. The UI loads nothing
// from elsewhere and runs no inline script.
func securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; "+
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cache-Control", "no-store")
}

// guard applies the host check and security headers to every request,
// and the session and CSRF checks to the API.
func (a *auth) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header())
		if !a.hostAllowed(r.Host) {
			http.Error(w, "unexpected Host header", http.StatusMisdirectedRequest)
			return
		}
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			s, ok := a.session(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, errString("not signed in: open the link that \"hcc control serve\" printed"))
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if r.Header.Get("Origin") != "http://"+r.Host {
					writeError(w, http.StatusForbidden, errString("cross-origin request refused"))
					return
				}
				if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(s.csrf)) != 1 {
					writeError(w, http.StatusForbidden, errString("missing or wrong CSRF token"))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

type errString string

func (e errString) Error() string { return string(e) }
