package server

import "net/http"

// requireKey puts a door in front of routes that were built without one.
//
// The live graph and the metrics come from CortexDB as plain handlers with no
// authentication of their own — right for a view on a loopback port the MCP
// server opens for one person, wrong on a listener other machines can reach.
// The first end-to-end run through this server found /graph/api/graph
// answering the whole graph to a caller with no key at all.
//
// A caller may present the key either way the rest of the server accepts it:
// a bearer header, which is what a scraper or a script sends, or the sign-in
// cookie, which is what a browser has. Clearance is not consulted — every
// route behind this is a read — but a key that is not in the policy is turned
// away, and so is no key.
func (s *Server) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.keys.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if _, code, _ := s.requestKey(r); code == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if s.signedIn(r) {
			next.ServeHTTP(w, r)
			return
		}
		httpError(w, http.StatusUnauthorized, "a key from the policy is required: send it as a bearer token, or sign in on the front page")
	})
}
