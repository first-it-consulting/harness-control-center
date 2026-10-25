package control

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Session lifetime and limits.
const (
	SessionTTL    = 12 * time.Hour
	maxSessions   = 64
	sessionHeader = "X-HCC-Session"
	ticketTTL     = time.Minute
	maxTickets    = 256
)

// auth guards the UI. The Control Center listens on loopback only, but
// any local process and any web page the user visits can talk to
// loopback, so:
//
//   - the Host header must name this server (no DNS rebinding),
//   - every API call needs a session, created only by opening the login
//     URL with the secret token printed at startup,
//   - the session is not a cookie (browsers send cookies to every port
//     of 127.0.0.1, so another local server could collect it): the login
//     redirect hands it to the page in the URL fragment, which is never
//     sent anywhere; the page keeps it in sessionStorage (this origin,
//     this port only) and sends it in a header that other sites cannot
//     set, which also rules out CSRF; changes also need a same-origin
//     Origin header,
//   - downloads, which cannot carry a header, use single-use tickets
//     that expire within a minute.
type auth struct {
	loginToken string
	now        func() time.Time

	mu       sync.Mutex
	hosts    []string // allowed Host headers
	sessions map[string]time.Time
	tickets  map[string]ticket
}

type ticket struct {
	path    string // the API path it opens
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
	return &auth{loginToken: randomToken(), now: time.Now, sessions: map[string]time.Time{}, tickets: map[string]ticket{}}
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
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) >= maxSessions { // drop the oldest
		var oldest string
		for k, exp := range a.sessions {
			if oldest == "" || exp.Before(a.sessions[oldest]) {
				oldest = k
			}
		}
		delete(a.sessions, oldest)
	}
	id = randomToken()
	a.sessions[id] = now.Add(SessionTTL)
	return id, true
}

// session reports whether the request carries a live session.
func (a *auth) session(r *http.Request) bool {
	id := r.Header.Get(sessionHeader)
	if id == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[id]
	return ok && !a.now().After(exp)
}

func (a *auth) logout(r *http.Request) {
	a.mu.Lock()
	delete(a.sessions, r.Header.Get(sessionHeader))
	a.mu.Unlock()
}

// newTicket returns a single-use ticket for GET path.
func (a *auth) newTicket(path string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, t := range a.tickets {
		if now.After(t.expires) {
			delete(a.tickets, k)
		}
	}
	if len(a.tickets) >= maxTickets {
		clear(a.tickets)
	}
	id := randomToken()
	a.tickets[id] = ticket{path: path, expires: now.Add(ticketTTL)}
	return id
}

// redeem consumes a ticket and returns the path it opens.
func (a *auth) redeem(id string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tickets[id]
	delete(a.tickets, id)
	if !ok || a.now().After(t.expires) {
		return "", false
	}
	return t.path, true
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
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !a.session(r) {
				writeError(w, http.StatusUnauthorized, errString("not signed in: open the link that \"hcc control serve\" printed"))
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != "http://"+r.Host {
				writeError(w, http.StatusForbidden, errString("cross-origin request refused"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type errString string

func (e errString) Error() string { return string(e) }
