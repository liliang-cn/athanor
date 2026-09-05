package server

import (
	"net/http"
	"testing"
)

func TestTheGraphAndTheMetricsAreBehindTheSameDoorAsEverythingElse(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	for _, path := range []string{"/graph/", "/graph/api/graph", "/metrics", "/debug/vars"} {
		resp, err := h.http(http.MethodGet, path, "", "")
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with no key answered %d; the first live run served the whole graph this way", path, resp.StatusCode)
		}
		resp, _ = h.http(http.MethodGet, path, "ro-secret", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s with a read-only key answered %d", path, resp.StatusCode)
		}
	}
}

func TestTheSignInCookieOpensTheGraphForABrowser(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+"/graph/api/graph", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "ro-secret"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a signed-in browser was refused the graph: %d", resp.StatusCode)
	}
	req.Header.Del("Cookie")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "not-a-key"})
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a cookie holding a key not in the policy opened the graph: %d", resp.StatusCode)
	}
}

func TestHealthzStaysOpenAndSaysNothing(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	resp, _ := h.http(http.MethodGet, "/healthz", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}
