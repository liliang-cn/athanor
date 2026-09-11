package server

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
)

// The shell every Athanor screen is drawn in.
//
// Server-rendered html/template, embedded, no build step — the choice both
// projects underneath already made, and the reason the front page has never
// needed a toolchain. What this file adds is the part a single page did not
// need: one layout, one palette, and one place that decides whether a browser
// may see a screen at all.
//
// # Why /app and not /athanor
//
// /athanor/… is the JSON API and answers a script. These pages are a second
// representation of the same acts, so they get their own prefix rather than
// content-negotiating one path into two audiences: a wrong Accept header
// should not be able to turn a page into an API response or the other way
// round, and the JSON routes are already classified read-or-write in tables
// with tests behind them. /ui/ is alchemy's review queue and stays its own.
//
// # The palette is the design canvas's, exactly
//
// Lifted from alchemy's /ui light scheme so the two halves of one product do
// not look like two products. One value is deliberately absent from the text
// colours: #8f9bb0 is the border token, it measures 2.8:1 against the panel,
// and an earlier draft used it for the word "asserted". Dim (#657189) is what
// unimportant text uses; #8f9bb0 draws lines only.
const uiCSS = `
:root{
  --bg:#f4f6f9; --panel:#fff; --ink:#1c2330; --dim:#657189; --line:#dcdfe6;
  --accent:#2563eb; --accent-ink:#1d4ed8; --bad:#b42318;
  --verified:#047857; --self:#0e7490; --asserted:#657189; --held:#b45309; --refused:#b42318;
  --r1:8px; --r2:12px;
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);
  font:15px/1.55 -apple-system,"SF Pro Text",system-ui,"Segoe UI",sans-serif;-webkit-font-smoothing:antialiased}
code,.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.9em}
a{color:var(--accent);text-decoration:none}a:hover{color:var(--accent-ink);text-decoration:underline}
.shell{display:grid;grid-template-columns:200px 1fr;min-height:100vh}
.rail{background:var(--panel);border-right:1px solid var(--line);padding:1.1rem .75rem;display:flex;flex-direction:column;gap:.15rem}
.rail .brand{font-weight:600;letter-spacing:-.01em;padding:.2rem .55rem 1rem}
.rail a{display:block;padding:.4rem .55rem;border-radius:var(--r1);color:var(--ink)}
.rail a:hover{background:var(--bg);text-decoration:none}
.rail a[aria-current="page"]{background:#eaf0fe;color:var(--accent-ink);font-weight:500}
.rail .out{margin-top:auto;padding-top:1rem;border-top:1px solid var(--line);color:var(--dim);font-size:.85rem}
.main{padding:1.6rem 2rem 4rem;max-width:64rem}
.top{display:flex;align-items:baseline;justify-content:space-between;gap:1rem;margin-bottom:1.4rem}
h1{font-size:1.35rem;font-weight:600;letter-spacing:-.01em;margin:0}
h2{font-size:1rem;font-weight:600;margin:2rem 0 .6rem}
.lede{color:var(--dim);margin:.35rem 0 0}
.card{background:var(--panel);border:1px solid var(--line);border-radius:var(--r2);padding:1rem 1.1rem;margin-bottom:1rem}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:.45rem .6rem;border-bottom:1px solid var(--line);vertical-align:top}
th{font-weight:500;color:var(--dim);font-size:.82rem;text-transform:uppercase;letter-spacing:.03em}
tr:last-child td{border-bottom:0}
.n{text-align:right;font-variant-numeric:tabular-nums}
.muted{color:var(--dim)}
.pill{display:inline-block;padding:.05rem .45rem;border-radius:999px;border:1px solid var(--line);font-size:.8rem}
.g-verified{color:var(--verified)}.g-self_consistent{color:var(--self)}.g-asserted{color:var(--asserted)}
.g-held{color:var(--held)}.g-refused{color:var(--refused)}
button,.btn{font:inherit;padding:.4rem .85rem;border-radius:var(--r1);border:1px solid var(--line);
  background:var(--panel);color:var(--ink);cursor:pointer}
button:hover,.btn:hover{background:var(--bg);text-decoration:none}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}
button.primary:hover{background:var(--accent-ink)}
input,select,textarea{font:inherit;padding:.4rem .6rem;border:1px solid var(--line);border-radius:var(--r1);
  background:var(--panel);color:var(--ink);max-width:100%}
label{display:block;font-size:.85rem;color:var(--dim);margin:.7rem 0 .2rem}
.row{display:flex;gap:.6rem;flex-wrap:wrap;align-items:flex-end}
.notice{background:#eaf0fe;border:1px solid #c7d7fb;border-radius:var(--r1);padding:.6rem .8rem;margin-bottom:1rem}
.notice.bad{background:#fdeceb;border-color:#f3c3bf;color:var(--bad)}
.empty{color:var(--dim);padding:.6rem 0}
.scroll{overflow-x:auto}
@media (max-width:760px){
  .shell{grid-template-columns:1fr}
  .rail{flex-direction:row;flex-wrap:wrap;align-items:center;border-right:0;border-bottom:1px solid var(--line);padding:.6rem}
  .rail .brand{padding:.2rem .55rem;width:100%}
  .rail .out{margin:0 0 0 auto;padding:0;border:0}
  .main{padding:1.1rem}
}
`

// uiNav is every screen in the rail, in the order a person meets them: what
// the brain holds, what wants a person, how more gets in, how it is looked at,
// and what was decided. Two entries are not Athanor's own pages — the review
// queue is alchemy's and the graph is CortexDB's — and they are in the same
// list because a person navigating a product does not care which repository
// drew the page.
var uiNav = []struct{ Href, Label string }{
	{"/app/shelf", "Shelf"},
	{"/ui/", "Review"},
	{"/app/import", "Import"},
	{"/graph/", "Graph"},
	{"/graph/ontology", "Ontology"},
	{"/app/decisions", "Decisions"},
}

// uiPage is what every screen hands the layout. Body is already-rendered HTML
// from the screen's own template, which is why it is template.HTML and why
// every screen must render through html/template rather than concatenating
// strings — the escaping is the type's whole job.
type uiPage struct {
	Title    string
	Lede     string
	Nav      string // the Href of the rail entry to mark current
	Actor    string
	Describe string
	Notice   string
	Bad      bool
	Body     template.HTML
}

var uiLayout = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · Athanor</title><style>` + uiCSS + `</style></head>
<body><div class="shell">
<nav class="rail">
  <a class="brand" href="/">Athanor</a>
  {{$cur := .Nav}}{{range .NavItems}}<a href="{{.Href}}"{{if eq .Href $cur}} aria-current="page"{{end}}>{{.Label}}</a>{{end}}
  <div class="out">{{if .Actor}}{{.Actor}}<br>{{end}}<span class="mono">{{.Describe}}</span>
    <form method="post" action="/signout" style="margin-top:.5rem"><button>Sign out</button></form>
  </div>
</nav>
<main class="main">
  <div class="top"><div><h1>{{.Title}}</h1>{{if .Lede}}<p class="lede">{{.Lede}}</p>{{end}}</div></div>
  {{if .Notice}}<div class="notice{{if .Bad}} bad{{end}}">{{.Notice}}</div>{{end}}
  {{.Body}}
</main>
</div></body></html>`))

// renderUI writes one screen. The layout never fails on a screen's data: a
// template that cannot execute has already written a partial page by the time
// it says so, so the body is rendered to completion first and only then sent.
func (s *Server) renderUI(w http.ResponseWriter, page uiPage) {
	data := struct {
		uiPage
		NavItems []struct{ Href, Label string }
	}{uiPage: page, NavItems: uiNav}
	if data.Describe == "" {
		data.Describe = s.describe
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = uiLayout.Execute(w, data)
}

// uiRender is a screen's own body: parse once at init, execute into a buffer,
// hand the layout HTML it can trust.
func uiRender(t *template.Template, data any) (template.HTML, error) {
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", err
	}
	return template.HTML(sb.String()), nil //nolint:gosec // t escapes its own data
}

// uiKey is the browser's key, or a redirect to the one sign-in form.
//
// A page is not an API: an unsigned browser gets sent to the door rather than
// a 401 it cannot act on. The key that comes back is the same authz.Key every
// JSON route resolves, so a screen authorizes exactly as its API twin does and
// the two cannot drift into disagreeing about who may see what.
func (s *Server) uiKey(w http.ResponseWriter, r *http.Request) (authz.Key, bool) {
	if !s.keys.Enabled() {
		return authz.Key{ID: openKeyID, Clearance: authz.ReadWrite}, true
	}
	c, err := r.Cookie(cookieName)
	if err == nil && c.Value != "" {
		if key, ok := s.keys.Lookup(c.Value); ok {
			return key, true
		}
	}
	http.Redirect(w, r, "/?notice=sign+in+to+see+that+page", http.StatusSeeOther)
	return authz.Key{}, false
}

// uiBearer is the cookie as the header the JSON handlers expect, for a screen
// that performs its act by calling its own API rather than reimplementing it.
// handleLoadForm established the pattern; captureWriter collects the answer.
func uiBearer(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return "Bearer " + c.Value
	}
	return ""
}
