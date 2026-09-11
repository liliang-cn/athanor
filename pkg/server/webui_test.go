package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// The interface reaches every route with a cookie, so every route must accept
// one.
//
// This is the defect that appeared the moment the server-rendered pages were
// removed: each JSON handler read Authorization directly, a browser cannot set
// that header, and so the product's own screens were locked out of the product
// with a 401 that said "missing bearer token". The fix was one function; this
// test is what stops the next handler from reading the header again.
func TestEveryJSONRouteAcceptsTheSessionCookie(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	for _, path := range []string{
		"/api/session",
		"/api/shelf",
		"/athanor/decisions?limit=5",
		"/athanor/ontologies",
		"/athanor/livedb/plans",
		"/athanor/livedb/follows",
	} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: cookieName, Value: "op-secret"})
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				t.Fatalf("%s refused the session cookie; the interface cannot reach it", path)
			}
		})
	}
}

// A bearer header still works and still wins, so a program behaves the same
// whatever a browser happened to leave in the jar.
func TestABearerHeaderOutranksTheCookie(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	req, err := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+"/api/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ro-secret")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "op-secret"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who struct {
		Actor    string `json:"actor"`
		CanWrite bool   `json:"can_write"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	if who.Actor != "reader" || who.CanWrite {
		t.Fatalf("the header did not win: %+v", who)
	}
}

// The secret goes in and does not come back out.
func TestTheSessionNeverReturnsTheKey(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	body := strings.NewReader(`{"key":` + strconv.Quote("op-secret") + `}`)
	req, err := http.NewRequest(http.MethodPost, "http://"+h.httpAddr+"/api/session", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw strings.Builder
	if _, err := raw.WriteString(readAll(t, resp)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw.String(), "op-secret") {
		t.Fatalf("the session answer carries the key back: %s", raw.String())
	}
}

// A key nobody issued is refused in the same words as a malformed one, so the
// endpoint is not an oracle for which ids exist.
func TestAnUnknownKeyIsRefusedIndistinguishably(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	seen := map[string]int{}
	for _, key := range []string{"not-a-key", "", "op-secre"} {
		body := strings.NewReader(`{"key":` + strconv.Quote(key) + `}`)
		req, err := http.NewRequest(http.MethodPost, "http://"+h.httpAddr+"/api/session", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		seen[strconv.Itoa(resp.StatusCode)+" "+readAll(t, resp)]++
		resp.Body.Close()
	}
	if len(seen) != 1 {
		t.Fatalf("three bad keys got %d different answers: %v", len(seen), seen)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// index.html must not be cached, and the hashed assets must be.
//
// Every build renames the bundle, which is the whole point of the hash: a
// browser may keep an asset forever because a changed asset has a different
// name. index.html is the one file that keeps its name, so a browser that
// reuses yesterday's copy asks for a bundle this deploy no longer has and
// renders nothing — a blank product, after a deploy that went fine. Go's file
// server sets no Cache-Control of its own and leaves it to the browser's
// heuristic, which caches. So the handler has to say so, on the path that
// serves a file that exists as well as on the fallback.
func TestTheIndexIsNeverCachedAndTheAssetsAlwaysAre(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html>")},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc123.css": {Data: []byte("body{}")},
	}
	h := webAssets(dist)
	for _, tc := range []struct {
		path string
		want string
		why  string
	}{
		{"/", "no-store", "the entry page names this build's bundle and nothing else"},
		{"/index.html", "no-store", "the same file reached by its own name"},
		{"/decisions/abc", "no-store", "a deep link the router answers with the entry page"},
		{"/assets/index-abc123.js", "public, max-age=31536000, immutable", "a hashed name never changes meaning"},
		{"/assets/index-abc123.css", "public, max-age=31536000, immutable", "likewise"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Fatalf("%s carries Cache-Control %q, want %q — %s", tc.path, got, tc.want, tc.why)
			}
		})
	}
}
