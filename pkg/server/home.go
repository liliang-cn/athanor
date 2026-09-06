package server

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The front page: what the shelf stands on, what wants a person, and where
// the other doors are. Server-rendered, no build step, embedded — the same
// choice both projects underneath already made for their own pages.
//
// It authenticates with the same key as everything else, carried in a cookie
// a sign-in form sets, because a browser cannot send a bearer header on its
// own. The cookie is the secret itself, HttpOnly and SameSite=Strict; nothing
// here invents a session store to protect a credential the key file already
// holds in plain text. The review UI alchemy ships (/ui) had a sign-in of its
// own and no longer needs one: this form is the only one, and session.go
// carries what it establishes into that UI.

const cookieName = "athanor_key"

var homeTmpl = template.Must(template.New("home").Parse(`<!doctype html>
<meta charset="utf-8"><title>Athanor</title>
<style>
body{font:15px/1.5 system-ui,sans-serif;max-width:56rem;margin:2.5rem auto;padding:0 1.25rem;color:#1c1c1c}
h1{font-weight:600;letter-spacing:-.01em}h2{font-size:1.05rem;margin-top:2.2rem}
table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:.35rem .6rem;border-bottom:1px solid #e6e6e6}
th{font-weight:500;color:#666}code{background:#f3f3f3;padding:.1rem .3rem;border-radius:3px}
.grade{display:inline-block;min-width:8rem}.n{text-align:right;font-variant-numeric:tabular-nums}
nav a{margin-right:1.2rem}form.in input{font:inherit;padding:.4rem .6rem;width:22rem}
.muted{color:#777}.why{color:#8a3b00}
</style>
<h1>Athanor</h1>
<p class="muted">{{.Describe}}</p>
{{if not .SignedIn}}
<form class="in" method="post" action="/signin">
  <p>Sign in with a key from the policy file.</p>
  <input type="password" name="key" placeholder="bearer key" autofocus>
  <button>Enter</button>
</form>
{{else}}
<nav>
  <a href="/ui/">Review queue</a>
  <a href="/graph/">Live graph</a>
  <a href="/graph/ontology">Ontology</a>
  <a href="/metrics">Metrics</a>
  <form method="post" action="/signout" style="display:inline"><button>Sign out</button></form>
</nav>

<h2>What the shelf stands on</h2>
<table>
<tr><th>grade</th><th>meaning</th><th class="n">nodes</th><th class="n">edges</th></tr>
{{range .Tally}}<tr><td class="grade"><code>{{.Grade}}</code></td><td>{{.Meaning}}</td><td class="n">{{.Nodes}}</td><td class="n">{{.Edges}}</td></tr>{{end}}
</table>

<h2>Wants a person</h2>
{{if not .Attention}}<p class="muted">Nothing is held or refused.</p>{{else}}
<table>
<tr><th>grade</th><th>record</th><th>why</th><th>from</th></tr>
{{range .Attention}}<tr><td><code>{{.Grade}}</code></td><td>{{.Record}}</td><td class="why">{{.Why}}</td><td class="muted">{{.Source}}</td></tr>{{end}}
</table>{{end}}

<h2>Decisions</h2>
<p class="muted">Every act this server performed, and every one an agent recorded through it.
<code>GET /athanor/decisions?kind=&amp;subject=&amp;actor=</code> asks the same question from a script.</p>
{{if not .Decisions}}<p class="muted">Nothing has been decided yet.</p>{{else}}
<table>
<tr><th>when</th><th>kind</th><th>who</th><th>what</th></tr>
{{range .Decisions}}<tr><td class="muted">{{.At}}</td><td><code>{{.Kind}}</code></td><td>{{.Actor}}</td>
<td><a href="/athanor/decisions/{{.Href}}">{{.Line}}</a></td></tr>{{end}}
</table>{{end}}

<h2>Load a finished job into the brain</h2>
<p class="muted">Jobs come from <a href="/v1/jobs">/v1/jobs</a>; a held job is refused here until it is reviewed.
<code>POST /athanor/loads {"job": "…"}</code> with your bearer key does the same from a script.</p>
<form method="post" action="/athanor/loads/form" class="in">
  <input name="job" placeholder="job id"> <input name="load" placeholder="load name (optional)" style="width:14rem">
  <button>Load</button>
</form>
<h2>Import from a database somebody else runs</h2>
<p class="muted">Three steps and the order is the point: <code>POST /athanor/livedb/plans</code> reads the schema and drafts a plan,
<code>POST /athanor/livedb/plans/{id}/signature</code> signs the hash you read, and <code>POST /athanor/livedb/runs</code> imports under
what was signed. Nothing reads a row until somebody has signed for what leaves the database, and
<code>GET /athanor/livedb/plans/current?source_key=</code> says which plan that is.</p>
<p class="muted">There is no form here. The connection string is a password and this page would carry it through a redirect,
a browser history and this server's own notice — so a run is made with a bearer key or not at all. The plan keeps the redacted
form of the string; the credential is supplied again on every run and is never stored, never shown and never in the ledger.</p>
<p class="muted">Keeping the brain in step with the database afterwards is a job rather than a request:
<code>POST /athanor/livedb/follows {"plan": "…", "dsn": "…"}</code> starts one under the same signed plan, <code>GET</code> the same path
says what is running, and <code>DELETE /athanor/livedb/follows/{id}</code> stops it. The credential is held in this server's memory
for the life of the follow and nowhere else, so a restart does not resume one and somebody supplies it again.</p>

{{if .Notice}}<p>{{.Notice}}</p>{{end}}
{{end}}
`))

type tallyRow struct {
	Grade, Meaning string
	Nodes, Edges   int
}

type attentionRow struct {
	Grade, Record, Why, Source string
}

// decisionRow is one ledger entry as the front page shows it: when, what kind,
// who, and the first line of the note — which is the sentence the entry was
// written as. The structured detail below it belongs on the chain page, where
// there is room to read it.
type decisionRow struct {
	Kind, Actor, At, Line, Href string
}

type homeData struct {
	Describe  string
	SignedIn  bool
	Tally     []tallyRow
	Attention []attentionRow
	Decisions []decisionRow
	Notice    string
}

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

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data := homeData{Describe: s.describe, SignedIn: s.signedIn(r), Notice: r.URL.Query().Get("notice")}
	if data.SignedIn {
		if t, err := s.db.ContractTally(r.Context()); err == nil {
			data.Tally = tallyRows(t)
		}
		// The latest twenty, Athanor's own beside the agents'. An error here
		// leaves the section empty rather than the page broken: the front page
		// is a view and a view that refuses to render because one of its four
		// questions failed is worse than one that shows the other three.
		if recs, err := s.latestDecisions(r.Context(), homeDecisionLimit); err == nil {
			data.Decisions = decisionRows(recs)
		}
		if att, err := s.db.NeedsAttention(r.Context(), 20); err == nil {
			for _, a := range att {
				rec := a.Content
				if a.Edge {
					rec = a.From + " —" + a.Type + "→ " + a.To
				}
				data.Attention = append(data.Attention, attentionRow{Grade: a.Grade, Record: rec, Why: a.Why, Source: a.Source})
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = homeTmpl.Execute(w, data)
}

// homeDecisionLimit is the twenty the front page shows. The rest are a query
// away, and a front page that scrolled the whole ledger would be a report.
const homeDecisionLimit = 20

func decisionRows(recs []cortexdb.DecisionRecord) []decisionRow {
	out := make([]decisionRow, 0, len(recs))
	for _, rec := range recs {
		line, _, _ := strings.Cut(rec.Note, "\n")
		out = append(out, decisionRow{
			Kind:  rec.Kind,
			Actor: rec.Actor,
			At:    rec.At,
			Line:  line,
			Href:  url.PathEscape(rec.ID),
		})
	}
	return out
}

func tallyRows(t cortexdb.ContractTally) []tallyRow {
	return []tallyRow{
		{"verified", "a named person kept it", t.Verified.Nodes, t.Verified.Edges},
		{"self_consistent", "derived from something that stated it", t.SelfConsistent.Nodes, t.SelfConsistent.Edges},
		{"asserted", "a model or a person said so; nobody checked", t.Asserted.Nodes, t.Asserted.Edges},
		{"held", "waiting on a person", t.Held.Nodes, t.Held.Edges},
		{"refused", "the vocabulary declined it", t.Refused.Nodes, t.Refused.Edges},
		{"untagged", "no contract at all", t.Untagged.Nodes, t.Untagged.Edges},
	}
}

func (s *Server) handleSignin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if _, ok := s.keys.Lookup(key); !ok && s.keys.Enabled() {
		http.Redirect(w, r, "/?notice=that+key+is+not+in+the+policy", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: key, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleSignout ends both sessions. One form signed the person in, so one
// button has to sign them out of everything it opened — a review UI still
// open after "Sign out" is worse than two sign-in forms ever were.
func (s *Server) handleSignout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	clearAlchemySession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLoadForm is the front page's button: the same act as POST
// /athanor/loads, authenticated by the cookie instead of a header.
func (s *Server) handleLoadForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	c, err := r.Cookie(cookieName)
	if err != nil && s.keys.Enabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var bearer string
	if c != nil {
		bearer = "Bearer " + c.Value
	}
	body := `{"job":` + jsonString(r.FormValue("job")) + `,"load":` + jsonString(r.FormValue("load")) + `}`
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "/athanor/loads", strings.NewReader(body))
	req.Header.Set("Authorization", bearer)
	rec := &captureWriter{header: http.Header{}}
	s.handleLoads(rec, req)
	notice := strings.TrimSpace(rec.body.String())
	if rec.status == 0 || rec.status == http.StatusOK {
		notice = "loaded: " + notice
	}
	http.Redirect(w, r, "/?notice="+template.URLQueryEscaper(notice), http.StatusSeeOther)
}

func jsonString(s string) string {
	b, _ := jsonMarshal(s)
	return string(b)
}
