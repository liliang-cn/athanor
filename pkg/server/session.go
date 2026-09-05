package server

import (
	"net/http"
	"strings"

	"github.com/liliang-cn/alchemy/pkg/gateway"
)

// One sign-in, two browser UIs.
//
// Athanor serves its own front page and alchemy's review UI on one listener,
// and until this file existed a person signed in twice with the same key: once
// into the front page's cookie, once into the review UI's. Two forms for one
// credential is the kind of seam a buyer notices in the first five minutes,
// and it was not a design decision — it was two projects each having solved
// the same problem before they shared a port.
//
// # What /ui/ actually checks, which is the design input
//
// Read from alchemy's pkg/gateway/viewsession.go and view.go:
//
//   - The cookie is named alchemy_view, scoped to Path=/ui/, HttpOnly,
//     SameSite=Strict, Secure under TLS, MaxAge 8h.
//   - Its value is *not* the token. It is a random 32-byte ticket, and the
//     gateway holds `ticket -> token` in a process-local map (sessions) with
//     no file behind it. A restart signs everybody out. viewauth_test.go
//     asserts, by name, that a cookie whose value *is* the token is refused —
//     so Athanor cannot make its own cookie satisfy alchemy's check by
//     spelling it differently. That closes option (b)-by-forgery outright.
//   - viewer.credential(r) looks in the Authorization header **first** and
//     only then in the cookie. viewer.authorize(r) takes whichever it found,
//     puts it in gRPC metadata as `authorization: Bearer <token>`, and calls
//     GetJob with an empty request — a probe that reaches the interceptor and
//     never the store. Anything but Unauthenticated means the service was
//     willing to talk to this caller. The gateway never judges a token itself.
//
// That last point is the whole opening. alchemy's gateway dials Athanor's own
// gRPC front door (server.go), so the credential it probes with is checked by
// Athanor's one policy (auth.go): a key from the key file is exactly what
// alchemy's authorize() will accept, because Athanor's interceptor is what
// answers. There is nothing to translate. A bearer header is already a
// first-class credential at /ui/ — it just was not reaching there from a
// browser, because a browser cannot set one.
//
// # What this does
//
// Option (a), the preferred one: a middleware in front of /ui/ turns Athanor's
// cookie into the header alchemy already accepts. No ticket is minted, no
// second session exists, and alchemy is not modified or even aware.
//
// Three things keep that from being a hole:
//
//   - The header is only ever *added*. A request that names its own credential
//     keeps it, so curl behaves at /ui/ exactly as it does at /v1.
//   - The cookie is looked up in the policy before it becomes a header. A
//     cookie holding a key nobody issued produces no header at all, and /ui/
//     answers its own 401 — the same answer it gives a stranger.
//   - Promoting a cookie to a header is the classic way to hand somebody
//     CSRF, and the thing that stops it here is that athanor_key is
//     SameSite=Strict: another origin cannot make a browser spend it, so a
//     cross-site POST to /ui/jobs/{id}/decisions arrives with no cookie and
//     therefore no header. That property is load-bearing now, not cosmetic.
//
// And one thing it removes: a signed-out browser navigating to /ui/ used to be
// shown alchemy's sign-in form, which is the second door in person. It is now
// redirected to the one form there is. Only a browser is redirected — a
// program with no key still gets 401 and the Bearer challenge alchemy's own
// auth table asserts.

// alchemyViewCookie is the ticket alchemy's viewer mints for a browser that
// took its own door.
//
// Athanor never mints one — the middleware below means a browser signed in
// here needs no ticket — but somebody who POSTed to /ui/session by hand has
// one, and sign-out has to end that too. The name is a literal because
// alchemy does not export it; see the report accompanying this change for the
// one hook that would remove the duplication.
const alchemyViewCookie = "alchemy_view"

// openKeyToken is what stands in for a credential when there is no key policy
// at all. Athanor with no key file is open by construction — requireKey passes
// everything through and the front page treats every visitor as signed in —
// but alchemy's viewer refuses a request carrying no credential of any shape
// before it ever asks the service. So the middleware presents a token; the
// interceptor that receives it (auth.go's authorizeAlchemy) returns before
// looking at it when the policy is disabled. Without this, "no key file" would
// mean "every door open except the review queue", which is a worse story than
// either alternative.
const openKeyToken = "athanor-open"

// oneSignIn carries Athanor's sign-in into alchemy's review UI.
func (s *Server) oneSignIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			next.ServeHTTP(w, r)
			return
		}
		if key, ok := s.browserKey(r); ok {
			// Cloned rather than mutated: the header belongs to this hop, and
			// r is the caller's.
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+key)
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && wantsHTML(r) && !hasCookie(r, alchemyViewCookie) {
			// A browser with no session of either kind. Sending it to the one
			// sign-in form is the difference between one door and two; a
			// browser that already holds an alchemy ticket is left alone, so
			// that door still works for anyone who used it.
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// browserKey is the credential Athanor's own cookie stands for, if the policy
// still names it.
func (s *Server) browserKey(r *http.Request) (string, bool) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if _, ok := s.keys.Lookup(c.Value); ok {
			return c.Value, true
		}
	}
	if !s.keys.Enabled() {
		return openKeyToken, true
	}
	return "", false
}

// clearAlchemySession tells a browser to drop alchemy's ticket.
//
// The deletion has to name alchemy's path as well as its name, because a
// Set-Cookie at the wrong path deletes nothing. What this cannot do is make
// the gateway forget the ticket server-side: alchemy scopes the cookie to
// /ui/, so it is not sent to /signout and Athanor never learns its value.
// That matters only for a browser that took alchemy's own door — the flow this
// file establishes mints no ticket to leave behind.
func clearAlchemySession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: alchemyViewCookie, Value: "", Path: gateway.ViewPrefix, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
}

// hasCookie reports whether a named cookie arrived with a value.
func hasCookie(r *http.Request, name string) bool {
	c, err := r.Cookie(name)
	return err == nil && c.Value != ""
}

// wantsHTML reports whether this looks like a browser navigating rather than a
// program calling. It is alchemy's own predicate by the same name, and it is
// spelled again here for the reason alchemy spells bearerHeader again: it
// decides nothing about authorization, only about which of two equally
// refusing answers reads better to whoever asked.
func wantsHTML(r *http.Request) bool {
	for _, accept := range r.Header.Values("Accept") {
		if strings.Contains(accept, "text/html") {
			return true
		}
	}
	return false
}
