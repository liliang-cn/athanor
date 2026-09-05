package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// A person signs in once. Both browser UIs open.
//
// The seam these tests hold shut is the one a buyer finds in the first five
// minutes: two sign-in forms for one key. Everything here is written from the
// browser's side — a cookie jar, a form post, a navigation — because that is
// the only side on which the two doors were ever distinguishable.

func (h *harness) browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{Jar: jar}
}

// stopAtRedirect keeps the jar and stops at the first response, so a test can
// read the Set-Cookie headers a 303 carries instead of the page it points at.
func stopAtRedirect(c *http.Client) *http.Client {
	d := *c
	d.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &d
}

func (h *harness) signIn(t *testing.T, c *http.Client, key string) *http.Response {
	t.Helper()
	resp, err := stopAtRedirect(c).PostForm("http://"+h.httpAddr+"/signin", url.Values{"key": {key}})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) signOut(t *testing.T, c *http.Client) *http.Response {
	t.Helper()
	resp, err := stopAtRedirect(c).PostForm("http://"+h.httpAddr+"/signout", nil)
	if err != nil {
		t.Fatalf("sign out: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// navigate is a browser following a link: it accepts HTML and carries whatever
// the jar holds.
func (h *harness) navigate(t *testing.T, c *http.Client, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+path, nil)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := stopAtRedirect(c).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// The whole point: one key, typed once, and the review queue opens.
func TestOneSignInOpensTheFrontPageAndTheReviewUI(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	b := h.browser(t)

	resp := h.signIn(t, b, "op-secret")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in answered %d, want 303", resp.StatusCode)
	}
	set := cookieNamed(resp.Cookies(), cookieName)
	if set == nil || set.Value != "op-secret" {
		t.Fatalf("sign in set no %s cookie", cookieName)
	}
	if !set.HttpOnly || set.SameSite != http.SameSiteStrictMode {
		t.Errorf("the cookie is weaker than it was: HttpOnly=%v SameSite=%v", set.HttpOnly, set.SameSite)
	}

	ui, body := h.navigate(t, b, "/ui/")
	if ui.StatusCode != http.StatusOK {
		t.Fatalf("the review UI answered %d after one sign-in; that is the second sign-in this exists to remove", ui.StatusCode)
	}
	if strings.Contains(body, "sign in") {
		t.Errorf("the review UI served its own sign-in form to a browser that had already signed in")
	}

	// And the routes the cookie already opened stay open.
	graph, _ := h.navigate(t, b, "/graph/api/graph")
	if graph.StatusCode != http.StatusOK {
		t.Errorf("the graph answered %d to a signed-in browser", graph.StatusCode)
	}
}

// A read-only key is a person too, and the review queue is a read.
func TestAReadOnlyKeyAlsoOpensTheReviewUIFromOneSignIn(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	b := h.browser(t)
	h.signIn(t, b, "ro-secret")
	ui, _ := h.navigate(t, b, "/ui/")
	if ui.StatusCode != http.StatusOK {
		t.Fatalf("the review UI answered %d to a read-only key", ui.StatusCode)
	}
}

// Signing out ends both, and the browser is left holding neither cookie.
func TestSigningOutEndsBothSessions(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	b := h.browser(t)
	h.signIn(t, b, "op-secret")

	// Take alchemy's own door as well, so there is a second session to end.
	req, _ := http.NewRequest(http.MethodPost, "http://"+h.httpAddr+"/ui/session", nil)
	req.Header.Set("Authorization", "Bearer op-secret")
	minted, err := b.Do(req)
	if err != nil {
		t.Fatalf("alchemy sign in: %v", err)
	}
	minted.Body.Close()
	if minted.StatusCode != http.StatusNoContent {
		t.Fatalf("alchemy sign in answered %d, want 204", minted.StatusCode)
	}

	out := h.signOut(t, b)
	for _, want := range []struct{ name, path string }{
		{cookieName, "/"},
		{alchemyViewCookie, "/ui/"},
	} {
		c := cookieNamed(out.Cookies(), want.name)
		if c == nil {
			t.Fatalf("sign out did not clear %s; a session it does not end is a session that outlives it", want.name)
		}
		if c.Value != "" || c.MaxAge >= 0 {
			t.Errorf("sign out set %s to %q MaxAge=%d, want an expiry", want.name, c.Value, c.MaxAge)
		}
		if c.Path != want.path {
			t.Errorf("sign out cleared %s at path %q, want %q; a deletion at the wrong path deletes nothing",
				want.name, c.Path, want.path)
		}
	}

	ui, _ := h.navigate(t, b, "/ui/")
	if ui.StatusCode == http.StatusOK {
		t.Errorf("the review UI stayed open after sign out")
	}
	graph, _ := h.navigate(t, b, "/graph/api/graph")
	if graph.StatusCode != http.StatusUnauthorized {
		t.Errorf("the graph answered %d after sign out, want 401", graph.StatusCode)
	}
}

// A key that is not in the policy leaves no trace at all.
func TestAKeyThatIsNotInThePolicySetsNoCookieAndOpensNothing(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	b := h.browser(t)

	resp := h.signIn(t, b, "not-a-key")
	if c := cookieNamed(resp.Cookies(), cookieName); c != nil {
		t.Fatalf("a key not in the policy set a cookie: %q", c.Value)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatalf("a refused sign-in set %d cookies", len(resp.Cookies()))
	}

	ui, _ := h.navigate(t, b, "/ui/")
	if ui.StatusCode == http.StatusOK {
		t.Errorf("the review UI opened for a key not in the policy")
	}
	graph, _ := h.navigate(t, b, "/graph/api/graph")
	if graph.StatusCode != http.StatusUnauthorized {
		t.Errorf("the graph answered %d for a key not in the policy, want 401", graph.StatusCode)
	}
}

// A cookie holding a key the policy does not name is no better than none.
func TestAForgedCookieDoesNotOpenTheReviewUI(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+"/ui/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "not-a-key"})
	resp, err := stopAtRedirect(&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a cookie holding a key not in the policy opened the review UI")
	}
}

// A browser with no session is sent to the one sign-in form there is; a
// program is still refused with the 401 every route answers.
func TestABrowserWithNoSessionIsSentToTheOneSignInForm(t *testing.T) {
	h := newHarness(t, fakeRunner{})

	resp, _ := h.navigate(t, h.browser(t), "/ui/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a signed-out browser at /ui/ got %d, want a redirect to the one sign-in form", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("redirected to %q, want %q", got, "/")
	}

	plain, err := h.http(http.MethodGet, "/ui/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	plain.Body.Close()
	if plain.StatusCode != http.StatusUnauthorized {
		t.Errorf("a program with no key got %d at /ui/, want 401", plain.StatusCode)
	}
	if !strings.Contains(plain.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("the 401 lost its Bearer challenge: %q", plain.Header.Get("WWW-Authenticate"))
	}
}

// The header still wins, so curl behaves at /ui/ exactly as it does at /v1.
func TestABearerHeaderStillReachesTheReviewUIWithoutACookie(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	resp, err := h.http(http.MethodGet, "/ui/", "ro-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a bearer header at /ui/ answered %d", resp.StatusCode)
	}
}

// The cookie is carried, not trusted over a header the caller sent: a request
// that names its own credential keeps it.
func TestAnExplicitHeaderIsNotOverwrittenByTheCookie(t *testing.T) {
	h := newHarness(t, fakeRunner{})
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.httpAddr+"/ui/", nil)
	req.Header.Set("Authorization", "Bearer not-a-key")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "op-secret"})
	resp, err := stopAtRedirect(&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a request carrying its own bad token was answered %d; the cookie overrode it", resp.StatusCode)
	}
}
