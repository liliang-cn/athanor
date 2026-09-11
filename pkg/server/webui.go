package server

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
)

// Serving the product's interface, which this package does not draw.
//
// Athanor is a product, so its UI is a frontend project of its own — Vite,
// React, Tailwind, shadcn, under web/ — and Go's job here is two things and no
// more: answer JSON, and hand the browser the built assets. There is no
// html/template in this repository any more. An earlier version of these
// screens was server-rendered with a hand-written stylesheet, on the reasoning
// that CortexDB and alchemy both server-render their own pages and so a build
// step could be avoided. That reasoning holds for a library's diagnostic page
// and does not survive contact with a product: the first thing a buyer sees
// was the ugliest thing in the repository.
//
// # The session is a cookie because a browser cannot send a header
//
// Sign-in is now POST /api/session with a JSON body, and the answer is a
// cookie plus the key's identity as JSON. The cookie is the credential itself,
// HttpOnly and SameSite=Strict — nothing here invents a session store to
// protect a secret the key file already holds in plain text — and it is what
// session.go promotes into an Authorization header for alchemy's /ui/, which
// is still that project's own page.
//
// SameSite=Strict is load-bearing rather than decorative: the middleware in
// session.go turns this cookie into a bearer header, and a cookie that another
// origin could make a browser spend would be CSRF against every write route.

// cookieName is the session cookie. The value is the key itself: nothing here
// invents a session store to protect a secret the key file already holds in
// plain text, and session.go promotes it into the Authorization header that
// alchemy's /ui/ already accepts.
const cookieName = "athanor_key"

// sessionResponse is what the frontend needs to know about who it is: the key
// id to show, and whether that key may write. Never the secret — it went in,
// it does not come back out.
type sessionResponse struct {
	SignedIn  bool   `json:"signed_in"`
	Actor     string `json:"actor,omitempty"`
	Clearance string `json:"clearance,omitempty"`
	CanWrite  bool   `json:"can_write"`
	Describe  string `json:"describe,omitempty"`
	// Open reports that this deployment has no key policy at all, so the
	// frontend shows no sign-in form rather than one nothing would accept.
	Open bool `json:"open"`
}

func (s *Server) sessionFor(key authz.Key, signedIn bool) sessionResponse {
	return sessionResponse{
		SignedIn:  signedIn,
		Actor:     key.ID,
		Clearance: string(key.Clearance),
		CanWrite:  key.AuthorizeOperation("athanor.session", authz.Method{Access: authz.Write}) == nil,
		Describe:  s.describe,
		Open:      !s.keys.Enabled(),
	}
}

// handleSession is the whole of sign-in, sign-out and "who am I".
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !s.keys.Enabled() {
			writeJSON(w, http.StatusOK, s.sessionFor(authz.Key{ID: openKeyID, Clearance: authz.ReadWrite}, true))
			return
		}
		key, code, _ := s.requestKey(r)
		if code != 0 {
			writeJSON(w, http.StatusOK, sessionResponse{Describe: s.describe})
			return
		}
		writeJSON(w, http.StatusOK, s.sessionFor(key, true))

	case http.MethodPost:
		var body struct {
			Key string `json:"key"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		secret := strings.TrimSpace(body.Key)
		key, ok := s.keys.Lookup(secret)
		if !ok && s.keys.Enabled() {
			// One message for a key that is not in the policy and a key that
			// is malformed: the difference is what a caller would use to tell
			// a real id from a guess.
			httpError(w, http.StatusUnauthorized, "that key is not in the policy")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: secret, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		})
		writeJSON(w, http.StatusOK, s.sessionFor(key, true))

	case http.MethodDelete:
		// One form signed the person in, so one call signs them out of
		// everything it opened — a review UI still open after sign-out is
		// worse than two sign-in forms ever were.
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		})
		clearAlchemySession(w, r)
		writeJSON(w, http.StatusOK, sessionResponse{Describe: s.describe, Open: !s.keys.Enabled()})

	default:
		httpError(w, http.StatusMethodNotAllowed, "GET to read the session, POST to open one, DELETE to end it")
	}
}

// cookieAuthorization renders the session cookie as the header every other
// route already understands, so one credential is resolved by one path.
func cookieAuthorization(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		return h
	}
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return "Bearer " + c.Value
	}
	return ""
}

// signedIn reports whether a browser carries a key the policy knows. gate.go
// uses it to let a signed-in browser reach the pages CortexDB and alchemy
// serve on this listener without a bearer header it cannot set.
func (s *Server) signedIn(r *http.Request) bool {
	if !s.keys.Enabled() {
		return true
	}
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	_, ok := s.keys.Lookup(c.Value)
	return ok
}

// handleShelf is the one read the frontend's first screen needs and the API
// did not have: the contract tally and what wants a person, in one answer, so
// a page load is one request rather than a waterfall.
func (s *Server) handleShelf(w http.ResponseWriter, r *http.Request) {
	key, code, msg := s.requestKey(r)
	if code != 0 {
		httpError(w, code, msg)
		return
	}
	if err := key.AuthorizeOperation("athanor.shelf", authz.Method{Access: authz.Read}); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return
	}
	out := map[string]any{}
	// A section whose query failed says so rather than rendering as empty: a
	// page that drops one of its questions silently tells a reader the brain
	// holds nothing of that kind, which is a different and worse claim.
	if tally, err := s.db.ContractTally(r.Context()); err != nil {
		out["tally_error"] = err.Error()
	} else {
		out["tally"] = tally
	}
	if att, err := s.db.NeedsAttention(r.Context(), 20); err != nil {
		out["attention_error"] = err.Error()
	} else {
		out["attention"] = att
	}
	writeJSON(w, http.StatusOK, out)
}

// webAssets serves the built frontend.
//
// Any path the bundle does not have is answered with index.html rather than
// 404, because the router is in the browser: a person who reloads on
// /decisions/xyz must get the app, not a not-found. Only /api, /athanor,
// /brain, /graph, /v1 and /ui are routed before this, and they are registered
// on the mux ahead of "/", so a client-side route can never shadow them.
func webAssets(dist fs.FS) http.Handler {
	files := http.FileServer(http.FS(dist))
	index, indexErr := fs.ReadFile(dist, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" {
			clean = "index.html"
		}
		if f, err := dist.Open(clean); err == nil {
			_ = f.Close()
			// Hashed asset names are immutable; index.html is not.
			if strings.HasPrefix(clean, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		if indexErr != nil {
			httpError(w, http.StatusNotFound, "this build carries no interface; the API is at /athanor and /brain")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(index)
	})
}
