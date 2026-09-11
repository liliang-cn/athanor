package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/liliang-cn/athanor/pkg/livedb"
	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
)

// How more gets into the brain, and what it costs to check afterwards.
//
// Four screens: the chooser at /app/import, the live-database wizard, a
// plan's runs, and what is being followed. They follow ui_shelf.go's shape —
// resolve the key, ask, build one flat view, render this file's own template,
// hand it to the layout — with two additions that are this screen's alone.
//
// # The kinds are ordered by how checkable their result is
//
// A DDL file and an exported graph call no model, so what comes out is a
// function of what went in and a second run produces the same thing. A table
// calls one only if nobody states the mapping. Prose calls one per chunk and
// cannot run at all without a vocabulary to disagree with. That is the whole
// product argument, and it is on the cards rather than in a document because
// the moment a person is choosing is the moment it decides anything.
//
// # The connection string
//
// home.go says, in prose on the front page, that there is deliberately no
// form for the DSN, because a page "would carry it through a redirect, a
// browser history and this server's own notice". The reasoning is right about
// all three and the conclusion does not follow: a form that never redirects,
// never takes the string from the URL and never renders it back has none of
// those three exits. This screen is that form, and the rules it keeps are
// listed on the screen itself as well as here:
//
//   - POST only, type="password", autocomplete="off". Read from PostFormValue
//     and never from FormValue, so a query parameter cannot supply one; a
//     ?dsn= in the URL is refused rather than used.
//   - Never rendered back. Not masked, not in a hidden input, not in an error
//     message. Nothing this file puts in a view struct is derived from it.
//     What names the database on every screen is Plan.Source.Redacted.
//   - Never in a Location, a notice or a log line. No act on this screen
//     answers with a redirect; the outcome is rendered into the same response
//     as the page, which is what makes that promise cheap to keep.
//   - Supplied again for the run and for the follow, exactly as the JSON API
//     requires, because the store never persisted it.
//
// The last line of defence is uiRenderWithout: every response this file
// writes is rendered into a buffer and the credential the request carried is
// removed from the finished bytes — raw, HTML-escaped and URL-escaped —
// before anything is sent. Nothing is supposed to reach it. It is here for
// the same reason livedbRedact is: the cost of one leak is a password in a
// browser history for ever, and a second check that costs a buffer is cheaper
// than the belief that the first one is complete.
//
// # Every act is this server's own JSON handler
//
// handleLoadForm established it: build the request the API takes, promote the
// cookie to a bearer header, call the handler, read the answer off a
// captureWriter. It matters more here than there. Proposing, amending,
// signing, running and following are five acts with a clearance rule, an
// allow-list, a ledger entry and a set of refusals apiece; a screen that
// called the store directly would be a second implementation of all of that,
// and the first thing it would get wrong is which of the five a read-only key
// may perform.

// The kinds of import, ordered by how checkable the result is.
//
// Only the live-database path has a screen. The others say where their door
// is, and say it exactly: a card that gestured at "the API" would send a
// person to read the source, and a card that named a route this server does
// not serve would be worse than saying nothing.
type importKind struct {
	Title string
	// Model is what it costs — the sentence the ordering is about.
	Model string
	// Determinism is the pill: high, or conditional.
	Determinism string
	What        string
	Door        template.HTML
	Href        string
	CTA         string
}

var importKinds = []importKind{{
	Title:       "A database schema (DDL)",
	Model:       "No model is called.",
	Determinism: "high determinism",
	What: "A CREATE TABLE file becomes tables, columns and the foreign keys between them. " +
		"Nothing is inferred: the types are read, the keys are read, and a second pass over the same file writes the same graph.",
	Door: `The deterministic planner is CortexDB's <code>importflow_ddl_plan</code>, and this server does not mount that toolbox yet — ` +
		`the door here that reads a schema is the live-database one below, which reads it from the database rather than from a file.`,
}, {
	Title:       "An existing graph",
	Model:       "No model is called.",
	Determinism: "high determinism",
	What: "Triples you already have, imported as they are. The vocabulary judges them on the way in and refuses what it does not declare, " +
		"which is the only thing that happens to them.",
	Door: `<code>POST /brain/v1/tools/knowledge_graph_import</code> with your bearer key.`,
}, {
	Title:       "A table (CSV, or a live database)",
	Model:       "A model is called only if you decline to state the mapping.",
	Determinism: "conditional determinism",
	What: "Rows become chunks and triples. Which column is the identity, which are properties and which foreign key is an edge " +
		"is either something you say — in which case nothing is inferred — or something a model proposes from the schema.",
	Door: `A live database is the wizard on this page. A CSV goes through CortexDB's <code>importflow_plan</code> and ` +
		`<code>importflow_run</code>, which this server does not mount yet.`,
	Href: "/app/import/livedb",
	CTA:  "Import from a live database",
}, {
	Title:       "Prose",
	Model:       "A model is called per chunk, and it cannot run without a vocabulary.",
	Determinism: "low determinism, reviewed",
	What: "A document is read for the things the vocabulary declares. Two sources that disagree hold the job until a person answers, " +
		"and the answer is what the record is graded on.",
	Door: `<code>POST /v1/sources</code> then <code>POST /v1/jobs</code> with an ontology; the review queue is <a href="/ui/">/ui/</a>, ` +
		`and a finished job enters the brain through <code>POST /athanor/loads</code>.`,
}}

var importChooserTmpl = template.Must(template.New("import").Parse(`
{{range .Kinds}}<div class="card">
  <h2 style="margin-top:0">{{.Title}}</h2>
  <p><span class="pill">{{.Determinism}}</span> <span class="muted">{{.Model}}</span></p>
  <p>{{.What}}</p>
  <p class="muted">{{.Door}}</p>
  {{if .Href}}<p><a class="btn" href="{{.Href}}">{{.CTA}}</a></p>{{end}}
</div>{{end}}

<div class="card">
  <h2 style="margin-top:0">Afterwards</h2>
  <p class="muted">An import that ran is a run under a plan somebody signed, and keeping the brain in step with a source
  afterwards is a job this server owns rather than a request somebody holds open.</p>
  <p class="row">
    <a class="btn" href="/app/import/runs">Runs</a>
    <a class="btn" href="/app/import/follows">Follows</a>
    <a class="btn" href="/">Load a finished job</a>
  </p>
</div>
`))

func (s *Server) handleUIImport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.uiKey(w, r); !ok {
		return
	}
	body, err := uiRender(importChooserTmpl, struct{ Kinds []importKind }{importKinds})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the import chooser could not be drawn")
		return
	}
	s.renderUI(w, uiPage{
		Title: "Import", Nav: "/app/import",
		Lede: "Four ways in, in the order of how checkable the result is. " +
			"The difference is whether a model was asked anything, and that is the difference a person is choosing between.",
		Body: body,
	})
}

// uiRenderWithout is renderUI with a last pass over the finished bytes.
//
// The page is rendered into a buffer, every secret this request carried is
// removed from it in each of the three shapes it could have taken — raw, HTML
// escaped, URL escaped — and only then is anything written. See the file
// comment on why a second check exists at all: nothing is supposed to reach
// it, and the whole point is not to rely on that.
func (s *Server) uiRenderWithout(w http.ResponseWriter, page uiPage, secrets ...string) {
	buf := &captureWriter{header: http.Header{}}
	s.renderUI(buf, page)
	out := uiScrub(buf.body.String(), secrets...)
	for name, values := range buf.header {
		w.Header()[name] = values
	}
	if buf.status != 0 {
		w.WriteHeader(buf.status)
	}
	_, _ = w.Write([]byte(out))
}

// uiSecretFloor is the shortest string this will scrub.
//
// A credential is a connection string and is never four characters long, and
// a scrubber that accepted one would replace every occurrence of a common
// fragment and mangle the page it was protecting. Below the floor the answer
// is to render nothing derived from it, which is what the rest of this file
// does anyway.
const uiSecretFloor = 8

func uiScrub(page string, secrets ...string) string {
	for _, secret := range secrets {
		for _, form := range uiSecretForms(secret) {
			page = strings.ReplaceAll(page, form, livedbPlaceholder)
		}
		for _, form := range uiSecretForms(livedbPassword(secret)) {
			page = strings.ReplaceAll(page, form, livedbPlaceholder)
		}
	}
	return page
}

// uiSecretForms is one secret in every encoding a page could carry it in.
func uiSecretForms(secret string) []string {
	secret = strings.TrimSpace(secret)
	if len(secret) < uiSecretFloor {
		return nil
	}
	forms := []string{secret}
	for _, encoded := range []string{
		template.HTMLEscapeString(secret),
		url.QueryEscape(secret),
		url.PathEscape(secret),
	} {
		if encoded != secret {
			forms = append(forms, encoded)
		}
	}
	return forms
}

// uiDSNInQuery is what a connection string in the URL is told.
//
// It is refused rather than used, and the page it lands on is rendered
// without it. A GET that carried one has already put it in the browser's
// history and possibly a proxy's log — this server cannot undo either, and
// the least it can do is not add its own copy and say plainly what happened.
const uiDSNInQuery = "a connection string was passed in the URL, and this screen will not read one from there: " +
	"it is in your browser's history now, so treat it as disclosed and rotate it. Type it into the form below, which POSTs it."

// uiJSON performs one act by calling this server's own JSON handler with the
// browser's cookie promoted to the bearer header handleLoadForm established.
//
// id, when set, is the {id} path value the handler reads — a synthesized
// request has no mux to fill it in.
func (s *Server) uiJSON(r *http.Request, h http.HandlerFunc, method, target, id, body string) (int, []byte) {
	req, err := http.NewRequestWithContext(r.Context(), method, target, strings.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer := uiBearer(r); bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := &captureWriter{header: http.Header{}}
	h(rec, req)
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	return status, rec.body.Bytes()
}

// uiMessage reads the message out of a JSON answer, which is httpError's
// {"error": …} for every refusal these handlers produce.
func uiMessage(body []byte) string {
	var answer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err == nil && answer.Error != "" {
		return answer.Error
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return trimmed
	}
	return "the server did not say why"
}

// uiRefusal is a refusal as a notice, carrying its status.
//
// The number is in the sentence because these statuses are the product: a 409
// on a signature is the plan having changed under the signer, and a page that
// rendered it as "could not sign" would hide the one distinction this whole
// workflow exists to make.
func uiRefusal(status int, body []byte) string {
	return fmt.Sprintf("%s (%d)", uiMessage(body), status)
}

// The live-database wizard.

// livedbUIActions is the closed set a treatment may be amended to, in the
// order of how much of the value survives. It is connector's own vocabulary;
// an action outside it is not sent.
var livedbUIActions = []connector.MaskAction{
	connector.ActionDrop,
	connector.ActionRedact,
	connector.ActionHash,
	connector.ActionPseudonymize,
	connector.ActionGeneralize,
	connector.ActionMask,
	connector.ActionKeep,
}

func livedbUIAction(raw string) (connector.MaskAction, bool) {
	want := connector.MaskAction(strings.TrimSpace(raw))
	for _, action := range livedbUIActions {
		if action == want {
			return action, true
		}
	}
	return "", false
}

func livedbUIDriver(raw string) string {
	switch driver := strings.TrimSpace(raw); driver {
	case "postgres", "mysql":
		return driver
	default:
		return "postgres"
	}
}

// livedbSensitivity names the ordered level, which is an integer on the wire
// and a word to a reader.
func livedbSensitivity(s connector.Sensitivity) string {
	switch s {
	case connector.Public:
		return "public"
	case connector.Internal:
		return "internal"
	case connector.Confidential:
		return "confidential"
	case connector.Restricted:
		return "restricted"
	default:
		return "unknown"
	}
}

type livedbColumnRow struct {
	Table, Column, Type string
	Kind                string
	Sensitivity         string
	Action              string
	Reason, By          string
	Sample, Enters      string
	Scan                bool
	Reversible          bool
	Actions             []connector.MaskAction
}

type livedbCountRow struct {
	Label string
	N     int
	Note  string
}

type livedbPlanView struct {
	ID, SourceKey, Redacted   string
	Driver, Schema            string
	Tables                    []string
	Hash, State, Note         string
	CreatedBy, SignedBy       string
	CreatedAt, SignedAt       string
	Counts                    []livedbCountRow
	Reversible                int
	Columns                   []livedbColumnRow
	Draft, Signed, Superseded bool
}

type livedbRunRow struct {
	ID, Plan               string
	StartedAt, EndedAt     string
	DryRun                 bool
	Rows, Chunks, Triples  int
	Skipped                int
	Drift, Gone, Failures  []string
	HasDrift, HasSomething bool
}

type livedbWizardView struct {
	Drivers []string
	Actions []connector.MaskAction
	// Driver, Schema and Tables are echoed back so a refused proposal does not
	// make the operator retype the harmless half of the form. The DSN is not
	// among them and never will be.
	Driver, Schema, Tables string
	DefaultAction          string
	ScanText               bool

	Plan    *livedbPlanView
	PlanErr string
	Report  *livedbRunRow
}

var livedbWizardTmpl = template.Must(template.New("livedb").Parse(`
<div class="card">
  <h2 style="margin-top:0">1 · Connect</h2>
  <p class="muted">Proposing reads the schema — column names, types and a handful of sample rows — classifies every column and
  writes a draft. It puts nothing in the brain and it imports no row.</p>
  <form method="post" action="/app/import/livedb" autocomplete="off">
    <input type="hidden" name="act" value="propose">
    <div class="row">
      <div><label for="driver">driver</label>
        <select id="driver" name="driver">{{$d := .Driver}}{{range .Drivers}}<option value="{{.}}"{{if eq . $d}} selected{{end}}>{{.}}</option>{{end}}</select></div>
      <div style="flex:1;min-width:22rem"><label for="dsn">connection string</label>
        <input id="dsn" name="dsn" type="password" autocomplete="off" spellcheck="false" style="width:100%"
               placeholder="postgres://user:password@host:5432/db"></div>
    </div>
    <div class="row">
      <div><label for="schema">schema <span class="muted">(optional)</span></label>
        <input id="schema" name="schema" value="{{.Schema}}" placeholder="public"></div>
      <div style="flex:1;min-width:18rem"><label for="tables">tables <span class="muted">(optional, comma separated)</span></label>
        <input id="tables" name="tables" value="{{.Tables}}" placeholder="every base table" style="width:100%"></div>
    </div>
    <div class="row">
      <div><label for="default_action">what happens to a column the classifier does not call personal</label>
        {{$a := .DefaultAction}}<select id="default_action" name="default_action">
          <option value=""{{if eq $a ""}} selected{{end}}>redact — fail closed</option>
          {{range .Actions}}<option value="{{.}}"{{if eq (printf "%s" .) $a}} selected{{end}}>{{.}}</option>{{end}}
        </select></div>
      <div><label for="scan_text">free text</label>
        <label style="color:inherit"><input type="checkbox" id="scan_text" name="scan_text" value="1"{{if .ScanText}} checked{{end}}>
          scan inside text columns for things that read like a phone number or an address</label></div>
    </div>
    <p class="row" style="margin-top:1rem"><button class="primary" type="submit">Propose a plan</button></p>
  </form>
  <p class="muted" style="margin-bottom:0">The connection string is a password, and this is the only place on this screen it exists.
  It is POSTed and never a URL parameter; it is never rendered back — not masked, not in a hidden field, not inside an error;
  it is never in a redirect, a notice or a log line. What names this database everywhere afterwards is the redacted form the plan keeps.
  The store never stores it, so the run and the follow below ask for it again.</p>
</div>

{{if .PlanErr}}<div class="card"><h2 style="margin-top:0">2 · The plan</h2>
<p class="empty">The plan could not be read: {{.PlanErr}}</p></div>{{end}}

{{with .Plan}}
<div class="card">
  <h2 style="margin-top:0">2 · The plan</h2>
  <p><code>{{.ID}}</code> <span class="pill">{{.State}}</span>
    <span class="muted">{{.Driver}} · {{.Redacted}}{{if .Schema}} · schema {{.Schema}}{{end}}</span></p>
  <div class="scroll"><table>
    <tr>{{range .Counts}}<th>{{.Label}}</th>{{end}}</tr>
    <tr>{{range .Counts}}<td class="n">{{.N}}</td>{{end}}</tr>
  </table></div>
  <p class="muted">{{range .Counts}}{{if and .Note (gt .N 0)}}<strong>{{.Label}}</strong>: {{.Note}} {{end}}{{end}}</p>
  {{if gt .Reversible 0}}
  <p class="notice">{{if eq .Reversible 1}}One of these columns is reversible{{else}}{{.Reversible}} of these columns are reversible{{end}}: the original is not thrown away, it is put in this Athanor's
  vault and the graph holds a token standing for it. That is a real copy of the personal data, on this machine, in a file beside the
  brain and not inside it — a backup of the brain carries neither the value nor the key. Signing a plan with a reversible treatment
  is refused outright when this deployment has no vault, so that the choice is made while you are reading rather than an hour into an import.</p>
  {{end}}
  <p class="muted">Covers {{len .Tables}} table{{if ne (len .Tables) 1}}s{{end}}: {{range $i, $t := .Tables}}{{if $i}}, {{end}}<code>{{$t}}</code>{{end}}.
    {{if .CreatedBy}}Proposed by {{.CreatedBy}}{{if .CreatedAt}} at {{.CreatedAt}}{{end}}.{{end}}
    {{if .SignedBy}}Signed by {{.SignedBy}}{{if .SignedAt}} at {{.SignedAt}}{{end}}.{{end}}</p>
  <div class="scroll"><table>
    <tr><th>column</th><th>in the database</th><th>detected</th><th>treatment</th><th>what enters the graph</th>{{if $.Plan.Draft}}<th>amend</th>{{end}}</tr>
    {{range .Columns}}<tr>
      <td><code>{{.Table}}.{{.Column}}</code>{{if .Type}}<br><span class="muted">{{.Type}}</span>{{end}}</td>
      <td>{{if .Sample}}<code>{{.Sample}}</code>{{else}}<span class="muted">no sample</span>{{end}}</td>
      <td>{{if .Kind}}<span class="pill">{{.Kind}}</span>{{else}}<span class="muted">not personal</span>{{end}}
        <br><span class="muted">{{.Sensitivity}}</span></td>
      <td><code>{{.Action}}</code>{{if .Reversible}} <span class="g-held">reversible</span>{{end}}{{if .Scan}} <span class="pill">text scanned</span>{{end}}
        {{if .Reason}}<br><span class="muted">{{.Reason}}</span>{{end}}{{if .By}}<br><span class="muted">by {{.By}}</span>{{end}}</td>
      <td>{{if .Enters}}<code>{{.Enters}}</code>{{else}}<span class="muted">nothing</span>{{end}}</td>
      {{if $.Plan.Draft}}<td><form method="post" action="/app/import/livedb" autocomplete="off">
        <input type="hidden" name="act" value="amend">
        <input type="hidden" name="plan" value="{{$.Plan.ID}}">
        <input type="hidden" name="table" value="{{.Table}}">
        <input type="hidden" name="column" value="{{.Column}}">
        {{$cur := .Action}}<select name="action">{{range .Actions}}<option value="{{.}}"{{if eq (printf "%s" .) $cur}} selected{{end}}>{{.}}</option>{{end}}</select>
        <button type="submit">Override</button>
      </form></td>{{end}}
    </tr>{{end}}
  </table></div>
  {{if .Draft}}<p class="muted" style="margin-bottom:0">An override is a person disagreeing with the classifier. It rewrites the draft and
  therefore its hash, which is why the signature below names the hash you read.</p>{{end}}
</div>

{{if .Draft}}
<div class="card">
  <h2 style="margin-top:0">3 · Sign</h2>
  <p>The hash of the plan above is <code>{{.Hash}}</code>, and that is the string the signature carries.</p>
  <p class="muted">It is a signature and not a checkbox, and the difference is checkable: if this plan is amended in another tab
  between your reading it and your signing it, the hash you send is no longer the plan's and the signature is refused with a 409
  rather than quietly applied to something you did not read. The ledger records the signature, the hash, and the name below.</p>
  <form method="post" action="/app/import/livedb" autocomplete="off">
    <input type="hidden" name="act" value="sign">
    <input type="hidden" name="plan" value="{{.ID}}">
    <input type="hidden" name="hash" value="{{.Hash}}">
    <div class="row">
      <div><label for="by">signed by</label><input id="by" name="by" placeholder="your name"></div>
      <div style="flex:1;min-width:18rem"><label for="note">note <span class="muted">(optional)</span></label>
        <input id="note" name="note" style="width:100%" placeholder="why this is the right plan"></div>
    </div>
    <p class="row" style="margin-top:1rem"><button class="primary" type="submit">Sign this plan</button></p>
  </form>
</div>
{{end}}

{{if .Signed}}
<div class="card">
  <h2 style="margin-top:0">4 · Run</h2>
  <p class="muted">Only what was signed runs. Every run re-reads the schema and names any column the database has gained since the
  signature: it is dropped rather than imported, and a re-signing is owed. The credential is asked for again because nothing kept it.</p>
  <form method="post" action="/app/import/livedb" autocomplete="off">
    <input type="hidden" name="act" value="run">
    <input type="hidden" name="plan" value="{{.ID}}">
    <div class="row">
      <div style="flex:1;min-width:22rem"><label for="rundsn">connection string</label>
        <input id="rundsn" name="dsn" type="password" autocomplete="off" spellcheck="false" style="width:100%"></div>
      <div><label for="namespace">namespace <span class="muted">(optional)</span></label>
        <input id="namespace" name="namespace" placeholder="the plan id"></div>
    </div>
    <p class="row" style="margin-top:1rem">
      <button class="primary" type="submit">Run this plan</button>
      <label style="color:inherit;margin:0"><input type="checkbox" name="dry_run" value="1"> dry run — read and desensitize, write nothing</label>
    </p>
  </form>
  <p class="muted" style="margin-bottom:0">Keeping the brain in step afterwards is a job rather than a request:
    <a href="/app/import/follows">start a follow</a>. Past runs of this plan are at
    <a href="/app/import/runs?plan={{.ID}}">/app/import/runs</a>.</p>
</div>
{{end}}
{{end}}

{{with .Report}}
<div class="card">
  <h2 style="margin-top:0">The run</h2>
  <p><code>{{.ID}}</code>{{if .DryRun}} <span class="pill">dry run — nothing was written</span>{{end}}
    <span class="muted">{{.StartedAt}} → {{.EndedAt}}</span></p>
  <div class="scroll"><table>
    <tr><th>rows read</th><th>chunks</th><th>triples</th><th>skipped</th></tr>
    <tr><td class="n">{{.Rows}}</td><td class="n">{{.Chunks}}</td><td class="n">{{.Triples}}</td><td class="n">{{.Skipped}}</td></tr>
  </table></div>
  {{if .Drift}}<p class="notice bad" style="margin-top:1rem">Drift: the database has {{len .Drift}} column{{if ne (len .Drift) 1}}s{{end}}
    the signed plan does not — {{range $i, $c := .Drift}}{{if $i}}, {{end}}<code>{{$c}}</code>{{end}}.
    They were dropped, which is the safe answer and not the whole answer: nobody has signed for what they hold, so a re-signing is owed.</p>{{end}}
  {{if .Gone}}<p class="notice">Gone: the plan names {{range $i, $c := .Gone}}{{if $i}}, {{end}}<code>{{$c}}</code>{{end}},
    which the database no longer has.</p>{{end}}
  {{if .Failures}}<p class="muted">Rows that failed: {{range $i, $e := .Failures}}{{if $i}}; {{end}}{{$e}}{{end}}</p>{{end}}
</div>
{{end}}
`))

func (s *Server) handleUIImportLiveDB(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.uiKey(w, r); !ok {
		return
	}
	query := r.URL.Query()
	view := livedbWizardView{
		Drivers: []string{"postgres", "mysql"},
		Actions: livedbUIActions,
		Driver:  livedbUIDriver(query.Get("driver")),
	}
	page := uiPage{
		Title: "Import from a live database", Nav: "/app/import",
		Lede: "Propose a plan from the schema, read what every column turns into, sign the hash you read, and only then import. " +
			"Nothing reads a row until somebody has signed for what leaves the database.",
	}

	// Secrets are collected before anything is rendered, so that the final
	// pass over the page can remove whatever this request carried whichever
	// way the handler went — including the paths that end in a refusal.
	var secrets []string
	planID := strings.TrimSpace(query.Get("plan"))

	switch {
	case query.Has("dsn"):
		// Refused rather than used, and the page it lands on is rendered
		// without it. This is the one URL that could have carried one.
		secrets = append(secrets, query.Get("dsn"))
		page.Notice, page.Bad = uiDSNInQuery, true

	case r.Method == http.MethodPost:
		dsn := strings.TrimSpace(r.PostFormValue("dsn"))
		if dsn != "" {
			secrets = append(secrets, dsn)
		}
		if formPlan := strings.TrimSpace(r.PostFormValue("plan")); formPlan != "" {
			planID = formPlan
		}
		switch strings.TrimSpace(r.PostFormValue("act")) {
		case "propose":
			planID = s.uiLivedbPropose(r, dsn, &view, &page)
		case "amend":
			s.uiLivedbAmend(r, planID, &view, &page)
		case "sign":
			s.uiLivedbSign(r, planID, &view, &page)
		case "run":
			s.uiLivedbRun(r, planID, dsn, &view, &page)
		default:
			page.Notice, page.Bad = "that form said nothing this screen performs", true
		}
	}

	// Whatever happened above, the plan is shown as the store now holds it. An
	// act that answered with the plan has already filled this in; one that
	// refused has not, and the page is more use with the plan the operator was
	// looking at than without it.
	if view.Plan == nil && planID != "" {
		status, answer := s.uiJSON(r, s.handleLivedbPlan, http.MethodGet, "/athanor/livedb/plans/"+url.PathEscape(planID), planID, "")
		if status == http.StatusOK {
			view.Plan = livedbPlanRows(answer)
		}
		if view.Plan == nil {
			view.PlanErr = uiRefusal(status, answer)
		}
	}

	body, err := uiRender(livedbWizardTmpl, view)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the import wizard could not be drawn")
		return
	}
	page.Body = body
	s.uiRenderWithout(w, page, secrets...)
}

// uiLivedbPropose reads the schema and drafts a plan, and returns the id of
// the plan it drafted so the caller can render it.
func (s *Server) uiLivedbPropose(r *http.Request, dsn string, view *livedbWizardView, page *uiPage) string {
	view.Driver = livedbUIDriver(r.PostFormValue("driver"))
	view.Schema = strings.TrimSpace(r.PostFormValue("schema"))
	view.Tables = strings.TrimSpace(r.PostFormValue("tables"))
	view.DefaultAction = strings.TrimSpace(r.PostFormValue("default_action"))
	view.ScanText = r.PostFormValue("scan_text") != ""

	if dsn == "" {
		page.Notice, page.Bad = "a connection string is required: a plan is proposed by reading the database's own schema", true
		return ""
	}
	req := livedbProposeRequest{
		Source: livedb.Source{
			Driver: view.Driver, DSN: dsn,
			Schema: view.Schema, Tables: uiCommaList(view.Tables),
		},
		Options: livedb.ProposeOptions{ScanText: view.ScanText},
	}
	// An unknown default action is dropped rather than sent: the zero value is
	// redact, which fails closed, and a typo that widened what is kept would
	// be the wrong direction to guess in.
	if action, ok := livedbUIAction(view.DefaultAction); ok {
		req.Options.DefaultAction = action
	}
	body, err := json.Marshal(req)
	if err != nil {
		page.Notice, page.Bad = "that proposal could not be built", true
		return ""
	}
	status, answer := s.uiJSON(r, s.handleLivedbPlans, http.MethodPost, "/athanor/livedb/plans", "", string(body))
	if status != http.StatusCreated {
		page.Notice, page.Bad = uiRefusal(status, answer), true
		return ""
	}
	view.Plan = livedbPlanRows(answer)
	if view.Plan == nil {
		page.Notice, page.Bad = "the plan was drafted and this screen could not read it back", true
		return ""
	}
	page.Notice = "a draft plan was proposed. Nothing has been read from the database beyond the samples below, and nothing is in the brain."
	return view.Plan.ID
}

// uiLivedbAmend is one person overriding the classifier on one column.
func (s *Server) uiLivedbAmend(r *http.Request, planID string, view *livedbWizardView, page *uiPage) {
	action, ok := livedbUIAction(r.PostFormValue("action"))
	if !ok || planID == "" {
		page.Notice, page.Bad = "an amendment names a plan, a column and one of this vocabulary's treatments", true
		return
	}
	change := livedb.Change{
		Table:  strings.TrimSpace(r.PostFormValue("table")),
		Column: strings.TrimSpace(r.PostFormValue("column")),
		Action: action,
		Reason: "overridden on the import screen",
	}
	body, err := json.Marshal(livedbAmendRequest{Changes: []livedb.Change{change}})
	if err != nil {
		page.Notice, page.Bad = "that amendment could not be built", true
		return
	}
	status, answer := s.uiJSON(r, s.handleLivedbPlan, http.MethodPatch,
		"/athanor/livedb/plans/"+url.PathEscape(planID), planID, string(body))
	if status != http.StatusOK {
		page.Notice, page.Bad = uiRefusal(status, answer), true
		return
	}
	view.Plan = livedbPlanRows(answer)
	page.Notice = change.Table + "." + change.Column + " is now " + string(action) +
		". The plan has a new hash, so the signature below names the plan as it is now."
}

// uiLivedbSign puts the plan in force, naming the hash the operator read.
func (s *Server) uiLivedbSign(r *http.Request, planID string, view *livedbWizardView, page *uiPage) {
	hash := strings.TrimSpace(r.PostFormValue("hash"))
	if planID == "" || hash == "" {
		page.Notice, page.Bad = "a signature names the plan the signer read, and one that names nothing is a checkbox", true
		return
	}
	body, err := json.Marshal(livedbSignRequest{
		Hash: hash,
		By:   strings.TrimSpace(r.PostFormValue("by")),
		Note: strings.TrimSpace(r.PostFormValue("note")),
	})
	if err != nil {
		page.Notice, page.Bad = "that signature could not be built", true
		return
	}
	status, answer := s.uiJSON(r, s.handleLivedbSignature, http.MethodPost,
		"/athanor/livedb/plans/"+url.PathEscape(planID)+"/signature", planID, string(body))
	if status != http.StatusOK {
		page.Notice, page.Bad = uiRefusal(status, answer), true
		return
	}
	view.Plan = livedbPlanRows(answer)
	page.Notice = "signed. The ledger has the signature, the hash it named and who signed it; the plan below is now what runs."
}

// uiLivedbRun imports under a signed plan.
func (s *Server) uiLivedbRun(r *http.Request, planID, dsn string, view *livedbWizardView, page *uiPage) {
	if planID == "" || dsn == "" {
		page.Notice, page.Bad = "a run names a signed plan and supplies the credential again, because the store never kept it", true
		return
	}
	body, err := json.Marshal(livedb.RunRequest{
		Plan: planID, DSN: dsn,
		Namespace: strings.TrimSpace(r.PostFormValue("namespace")),
		DryRun:    r.PostFormValue("dry_run") != "",
	})
	if err != nil {
		page.Notice, page.Bad = "that run could not be built", true
		return
	}
	status, answer := s.uiJSON(r, s.handleLivedbRuns, http.MethodPost, "/athanor/livedb/runs", "", string(body))
	if status != http.StatusOK {
		page.Notice, page.Bad = uiRefusal(status, answer), true
		return
	}
	var report livedb.RunReport
	if err := json.Unmarshal(answer, &report); err != nil {
		page.Notice, page.Bad = "the import ran and this screen could not read the report", true
		return
	}
	row := livedbRunRowOf(report)
	view.Report = &row
	page.Notice = "the import ran."
	if len(report.Drift) > 0 {
		page.Notice, page.Bad = "the import ran, and the database has columns the signed plan does not. They were dropped; a re-signing is owed.", true
	}
}

// livedbPlanRows turns a plan answer into the one flat struct the template
// reads. A body that will not decode returns nil, and the caller says so
// rather than rendering an empty plan, which would read as a plan with no
// columns in it.
func livedbPlanRows(answer []byte) *livedbPlanView {
	var plan livedb.Plan
	if err := json.Unmarshal(answer, &plan); err != nil || plan.ID == "" {
		return nil
	}
	view := &livedbPlanView{
		ID: plan.ID, SourceKey: plan.SourceKey,
		Redacted: plan.Source.Redacted, Driver: plan.Source.Driver, Schema: plan.Source.Schema,
		Tables: plan.Source.Tables, Hash: plan.Hash, State: string(plan.State), Note: plan.Note,
		CreatedBy: plan.CreatedBy, SignedBy: plan.SignedBy,
		CreatedAt: uiWhen(plan.CreatedAt), SignedAt: uiWhen(plan.SignedAt),
		Reversible: plan.Counts.Reversible,
		Draft:      plan.State == livedb.Draft,
		Signed:     plan.State == livedb.Signed,
		Superseded: plan.State == livedb.Superseded,
	}
	// Counts lead the screen, in the order a reviewer reads them: how much
	// there is, how much of it is personal, and then what was done about it.
	// Two carry a note because two are not self-explaining.
	view.Counts = []livedbCountRow{
		{Label: "columns", N: plan.Counts.Columns},
		{Label: "personal", N: plan.Counts.Personal},
		{Label: "dropped", N: plan.Counts.Dropped},
		{Label: "masked", N: plan.Counts.Masked},
		{Label: "generalized", N: plan.Counts.Generalized},
		{Label: "passed", N: plan.Counts.Passed},
		{Label: "reversible", N: plan.Counts.Reversible, Note: "can be turned back, through this Athanor's vault."},
		{Label: "unscanned_text", N: plan.Counts.UnscannedText,
			Note: "free-text columns that pass through whole, with nothing looking inside them. " +
				"The classifier judges a column, not the sentences in it, and people type addresses into free text."},
	}
	for _, treatment := range plan.Columns {
		view.Columns = append(view.Columns, livedbColumnRow{
			Table: treatment.Table, Column: treatment.Column, Type: treatment.Type,
			Kind:        string(treatment.Kind),
			Sensitivity: livedbSensitivity(treatment.Sensitivity),
			Action:      string(treatment.Action),
			Reason:      treatment.Reason, By: treatment.By,
			Sample: treatment.Sample, Enters: treatment.Enters,
			Scan:       treatment.Scan,
			Reversible: treatment.Action.Reversible(),
			Actions:    livedbUIActions,
		})
	}
	return view
}

func livedbRunRowOf(report livedb.RunReport) livedbRunRow {
	return livedbRunRow{
		ID: report.ID, Plan: report.Plan,
		StartedAt: uiWhen(report.StartedAt), EndedAt: uiWhen(report.EndedAt),
		DryRun: report.DryRun,
		Rows:   report.RowsRead, Chunks: report.Chunks, Triples: report.Triples, Skipped: report.Skipped,
		Drift: report.Drift, Gone: report.Gone, Failures: report.Errors,
	}
}

func uiWhen(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format("2006-01-02 15:04:05Z")
}

// uiCommaList reads a comma-separated field, dropping the empties a trailing
// comma leaves behind.
func uiCommaList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// A plan's runs.

type livedbRunsView struct {
	Plan     string
	Runs     []livedbRunRow
	RunsErr  string
	Plans    []livedbPlanSummary
	PlansErr string
}

type livedbPlanSummary struct {
	ID, State, Redacted, SourceKey, CreatedAt, SignedBy string
}

var livedbRunsTmpl = template.Must(template.New("livedbruns").Parse(`
<div class="card">
  <h2 style="margin-top:0">A plan's runs</h2>
  <p class="muted">Runs are read per plan, because a run is only explicable under the plan it ran with.
    <code>GET /athanor/livedb/runs?plan=…</code> asks the same question from a script.</p>
  <form method="get" action="/app/import/runs" class="row">
    <div style="flex:1;min-width:18rem"><label for="plan">plan</label>
      <input id="plan" name="plan" value="{{.Plan}}" placeholder="plan id" style="width:100%"></div>
    <button type="submit">Show its runs</button>
  </form>
</div>

{{if .Plan}}
<div class="card">
  <h2 style="margin-top:0">Runs of <code>{{.Plan}}</code></h2>
  {{if .RunsErr}}<p class="empty">They could not be read: {{.RunsErr}}</p>
  {{else if not .Runs}}<p class="empty">This plan has not run.</p>
  {{else}}<div class="scroll"><table>
    <tr><th>run</th><th>when</th><th class="n">rows</th><th class="n">chunks</th><th class="n">triples</th><th>drift</th></tr>
    {{range .Runs}}<tr>
      <td><code>{{.ID}}</code>{{if .DryRun}}<br><span class="pill">dry run</span>{{end}}</td>
      <td class="muted">{{.StartedAt}}{{if .EndedAt}}<br>→ {{.EndedAt}}{{end}}</td>
      <td class="n">{{.Rows}}</td><td class="n">{{.Chunks}}</td><td class="n">{{.Triples}}</td>
      <td>{{if .Drift}}<span class="g-held">{{range $i, $c := .Drift}}{{if $i}}, {{end}}<code>{{$c}}</code>{{end}}</span>
          <br><span class="muted">dropped; a re-signing is owed</span>{{else}}<span class="muted">none</span>{{end}}
        {{if .Gone}}<br><span class="muted">gone: {{range $i, $c := .Gone}}{{if $i}}, {{end}}<code>{{$c}}</code>{{end}}</span>{{end}}</td>
    </tr>{{end}}
  </table></div>{{end}}
</div>
{{else}}
<div class="card">
  <h2 style="margin-top:0">Plans</h2>
  {{if .PlansErr}}<p class="empty">They could not be read: {{.PlansErr}}</p>
  {{else if not .Plans}}<p class="empty">No plan has been proposed. <a href="/app/import/livedb">Propose one.</a></p>
  {{else}}<div class="scroll"><table>
    <tr><th>plan</th><th>state</th><th>database</th><th>proposed</th><th>signed by</th></tr>
    {{range .Plans}}<tr>
      <td><a href="/app/import/runs?plan={{.ID}}"><code>{{.ID}}</code></a></td>
      <td><span class="pill">{{.State}}</span></td>
      <td class="muted">{{.Redacted}}</td>
      <td class="muted">{{.CreatedAt}}</td>
      <td class="muted">{{if .SignedBy}}{{.SignedBy}}{{else}}nobody{{end}}</td>
    </tr>{{end}}
  </table></div>{{end}}
</div>
{{end}}
`))

func (s *Server) handleUIImportRuns(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.uiKey(w, r); !ok {
		return
	}
	view := livedbRunsView{Plan: strings.TrimSpace(r.URL.Query().Get("plan"))}
	if view.Plan != "" {
		status, answer := s.uiJSON(r, s.handleLivedbRuns, http.MethodGet,
			"/athanor/livedb/runs?plan="+url.QueryEscape(view.Plan), "", "")
		var decoded struct {
			Runs []livedb.RunReport `json:"runs"`
		}
		switch {
		case status != http.StatusOK:
			view.RunsErr = uiRefusal(status, answer)
		case json.Unmarshal(answer, &decoded) != nil:
			view.RunsErr = "the answer could not be read"
		default:
			for _, report := range decoded.Runs {
				view.Runs = append(view.Runs, livedbRunRowOf(report))
			}
		}
	} else {
		status, answer := s.uiJSON(r, s.handleLivedbPlans, http.MethodGet, "/athanor/livedb/plans?limit=50", "", "")
		var decoded struct {
			Plans []livedb.Plan `json:"plans"`
		}
		switch {
		case status != http.StatusOK:
			view.PlansErr = uiRefusal(status, answer)
		case json.Unmarshal(answer, &decoded) != nil:
			view.PlansErr = "the answer could not be read"
		default:
			for _, plan := range decoded.Plans {
				view.Plans = append(view.Plans, livedbPlanSummary{
					ID: plan.ID, State: string(plan.State), Redacted: plan.Source.Redacted,
					SourceKey: plan.SourceKey, CreatedAt: uiWhen(plan.CreatedAt), SignedBy: plan.SignedBy,
				})
			}
		}
	}
	body, err := uiRender(livedbRunsTmpl, view)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the runs could not be drawn")
		return
	}
	s.renderUI(w, uiPage{
		Title: "Runs", Nav: "/app/import",
		Lede: "What one import did: how many rows it read, what it made of them, and which columns the database has gained " +
			"since somebody signed for them.",
		Body: body,
	})
}

// What is being followed.

type livedbFollowRow struct {
	ID, Plan, SourceKey, Redacted, Namespace string
	State, StartedAt, StoppedAt, Failure     string
	Running                                  bool
}

type livedbFollowsView struct {
	Follows    []livedbFollowRow
	FollowsErr string
}

var livedbFollowsTmpl = template.Must(template.New("livedbfollows").Parse(`
<div class="card">
  <h2 style="margin-top:0">What is running</h2>
  {{if .FollowsErr}}<p class="empty">They could not be read: {{.FollowsErr}}</p>
  {{else if not .Follows}}<p class="empty">Nothing is being followed.</p>
  {{else}}<div class="scroll"><table>
    <tr><th>follow</th><th>plan</th><th>database</th><th>state</th><th>started</th><th></th></tr>
    {{range .Follows}}<tr>
      <td><code>{{.ID}}</code>{{if .Namespace}}<br><span class="muted">into {{.Namespace}}</span>{{end}}</td>
      <td><a href="/app/import/runs?plan={{.Plan}}"><code>{{.Plan}}</code></a></td>
      <td class="muted">{{.Redacted}}</td>
      <td>{{if .Running}}<span class="g-verified">running</span>{{else}}<span class="g-held">stopped</span>
        {{if .StoppedAt}}<br><span class="muted">{{.StoppedAt}}</span>{{end}}
        {{if .Failure}}<br><span class="g-refused">{{.Failure}}</span>{{end}}{{end}}</td>
      <td class="muted">{{.StartedAt}}</td>
      <td><form method="post" action="/app/import/follows">
        <input type="hidden" name="act" value="stop">
        <input type="hidden" name="id" value="{{.ID}}">
        <button type="submit">{{if .Running}}Stop{{else}}Clear{{end}}</button>
      </form></td>
    </tr>{{end}}
  </table></div>
  <p class="muted" style="margin-bottom:0">A follow that stopped stays listed with the reason until somebody clears it, because that is
  how the reason gets read — and clearing it is how another one is started for the same plan.</p>{{end}}
</div>

<div class="card">
  <h2 style="margin-top:0">Follow a signed plan</h2>
  <p class="muted">A follow is a job this server owns rather than a request somebody holds open, so starting one answers 202 and the
  work carries on without you. The first pass is a full import and runs before the change stream opens, so a follow that has just
  started may be importing for hours.</p>
  <p class="muted">The credential is held in this server's memory for the life of the follow and nowhere else — not on a struct, not in
  this listing, not in a log line and not in the ledger. A restart does not resume a follow, because there would be nothing left to
  resume it with, and somebody supplies it again.</p>
  <form method="post" action="/app/import/follows" autocomplete="off">
    <input type="hidden" name="act" value="start">
    <div class="row">
      <div><label for="plan">signed plan</label><input id="plan" name="plan" placeholder="plan id"></div>
      <div style="flex:1;min-width:20rem"><label for="dsn">connection string</label>
        <input id="dsn" name="dsn" type="password" autocomplete="off" spellcheck="false" style="width:100%"></div>
      <div><label for="namespace">namespace <span class="muted">(optional)</span></label>
        <input id="namespace" name="namespace" placeholder="the plan id"></div>
    </div>
    <p class="row" style="margin-top:1rem"><button class="primary" type="submit">Start following</button></p>
  </form>
</div>
`))

func (s *Server) handleUIImportFollows(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.uiKey(w, r); !ok {
		return
	}
	var view livedbFollowsView
	page := uiPage{
		Title: "Follows", Nav: "/app/import",
		Lede: "Keeping the brain in step with a database it does not own. Every one of these holds a live credential in memory " +
			"for as long as it runs.",
	}
	var secrets []string

	if query := r.URL.Query(); query.Has("dsn") {
		secrets = append(secrets, query.Get("dsn"))
		page.Notice, page.Bad = uiDSNInQuery, true
	} else if r.Method == http.MethodPost {
		dsn := strings.TrimSpace(r.PostFormValue("dsn"))
		if dsn != "" {
			secrets = append(secrets, dsn)
		}
		switch strings.TrimSpace(r.PostFormValue("act")) {
		case "start":
			s.uiLivedbStartFollow(r, dsn, &page)
		case "stop":
			s.uiLivedbStopFollow(r, &page)
		default:
			page.Notice, page.Bad = "that form said nothing this screen performs", true
		}
	}

	// The listing is read after the act, so that a start or a stop is visible
	// in the same response that reports it. It is asked of the follows route
	// and not of the store, which is deliberate there: a listing that answered
	// 503 because the store could not be built would hide running goroutines
	// behind the reason they could not have been started.
	status, answer := s.uiJSON(r, s.handleLivedbFollows, http.MethodGet, "/athanor/livedb/follows", "", "")
	var decoded struct {
		Follows []livedbFollowView `json:"follows"`
	}
	switch {
	case status != http.StatusOK:
		view.FollowsErr = uiRefusal(status, answer)
	case json.Unmarshal(answer, &decoded) != nil:
		view.FollowsErr = "the answer could not be read"
	default:
		for _, follow := range decoded.Follows {
			row := livedbFollowRow{
				ID: follow.ID, Plan: follow.Plan, SourceKey: follow.SourceKey,
				Redacted: follow.Redacted, Namespace: follow.Namespace,
				State: follow.State, StartedAt: uiWhen(follow.StartedAt),
				Failure: follow.Error, Running: follow.State == livedbFollowRunning,
			}
			if follow.StoppedAt != nil {
				row.StoppedAt = uiWhen(*follow.StoppedAt)
			}
			view.Follows = append(view.Follows, row)
		}
	}

	body, err := uiRender(livedbFollowsTmpl, view)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the follows could not be drawn")
		return
	}
	page.Body = body
	s.uiRenderWithout(w, page, secrets...)
}

func (s *Server) uiLivedbStartFollow(r *http.Request, dsn string, page *uiPage) {
	plan := strings.TrimSpace(r.PostFormValue("plan"))
	if plan == "" || dsn == "" {
		page.Notice, page.Bad = "a follow names a signed plan and supplies the credential, which it needs for as long as it runs", true
		return
	}
	body, err := json.Marshal(livedbFollowRequest{
		Plan: plan, DSN: dsn, Namespace: strings.TrimSpace(r.PostFormValue("namespace")),
	})
	if err != nil {
		page.Notice, page.Bad = "that follow could not be built", true
		return
	}
	status, answer := s.uiJSON(r, s.handleLivedbFollows, http.MethodPost, "/athanor/livedb/follows", "", string(body))
	if status != http.StatusAccepted {
		page.Notice, page.Bad = uiRefusal(status, answer), true
		return
	}
	page.Notice = "accepted (202). The first pass runs before the change stream opens, so this follow may be importing for some time yet."
}

func (s *Server) uiLivedbStopFollow(r *http.Request, page *uiPage) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		page.Notice, page.Bad = "stopping a follow names the follow", true
		return
	}
	status, answer := s.uiJSON(r, s.handleLivedbFollow, http.MethodDelete,
		"/athanor/livedb/follows/"+url.PathEscape(id), id, "")
	if status != http.StatusOK {
		page.Notice, page.Bad = uiRefusal(status, answer), true
		return
	}
	page.Notice = "stopped. The change stream is unwinding on its own schedule; it is already out of the listing."
}
