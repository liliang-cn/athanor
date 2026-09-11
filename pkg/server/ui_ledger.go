package server

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/liliang-cn/cortexdb/v2/pkg/authz"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The ledger, as two screens: every act, and one act's account of itself.
//
// These are the browser's half of ledger.go's read door, and they are
// deliberately thin over it. The list asks s.readLedger with the same four
// filters GET /athanor/decisions takes, under the same names, so a filtered
// view is a URL somebody can send and the same URL rewritten to /athanor is
// the JSON. The chain page asks s.db.DecisionChain, the same call the JSON
// route makes. Nothing here queries the brain a way the API does not.
//
// # Confinement is not restated here
//
// A key confined to a user_id may see only its own entries, and the rule for
// what that means — including the refusal for a confinement a decision cannot
// express — lives in ledgerConfinement and nowhere else. Both handlers call
// it and apply its answer exactly as handleDecisions and handleDecisionChain
// do: the list narrows to the confined actor (and answers nothing at all when
// the caller asked for a different one), and the chain is opened only when the
// ROOT's actor matches, because a chain reaches decisions other people made
// and that is what a chain is for.
//
// What is not shared with the API is the door: a browser without a key gets
// sent to the sign-in form rather than a 401 it cannot act on, so these
// resolve the key through uiKey and then check the same operation and access
// authorizeLedger checks. s.authorizeLedger itself does not fit — it writes a
// 401 — but the clearance question it asks is asked here verbatim.
//
// # There is no write on these screens
//
// A decision is recorded by performing the act it describes. A form that
// posted an entry would make the ledger a diary, which is ledger.go's own
// reason for having no write route, and a page is not an exception to it.

// uiLedgerDepth is how far the chain page walks by default. Zero is
// DecisionChain's own "as far as it goes", which is the honest default for a
// page whose whole purpose is showing where something came from; ?depth= on
// this page means what ?depth= means on the JSON route.
const uiLedgerDepth = 0

var ledgerListTmpl = template.Must(template.New("decisions").Parse(`
<div class="card">
  <form method="get" action="/app/decisions" class="row">
    <div><label for="f-kind">kind</label>
      <input id="f-kind" name="kind" value="{{.Query.Kind}}" placeholder="load, review, ontology.approve…" size="22"></div>
    <div><label for="f-subject">subject</label>
      <input id="f-subject" name="subject" value="{{.Query.Subject}}" placeholder="what it was about" size="20"></div>
    <div><label for="f-actor">actor</label>
      <input id="f-actor" name="actor" value="{{.Query.Actor}}" placeholder="key id"{{if .Confined}} readonly{{end}} size="14"></div>
    <div><label for="f-limit">limit</label>
      <input id="f-limit" name="limit" value="{{.Query.Limit}}" size="4" inputmode="numeric"></div>
    <div><button class="primary">Filter</button></div>
    <div><a class="btn" href="/app/decisions">Clear</a></div>
  </form>
  {{if .Confined}}<p class="muted" style="margin-bottom:0">This key is confined to <code>{{.Confined}}</code> and sees only its own entries.</p>{{end}}
</div>

<div class="card">
  <h2 style="margin-top:0">Entries</h2>
  <p class="muted"><code>GET {{.APIPath}}</code> asks the same question from a script.</p>
  {{if .Err}}<p class="empty">The ledger could not be read: {{.Err}}</p>
  {{else if not .Rows}}
    {{if .Filtered}}<p class="empty">Nothing in the ledger matches that filter.</p>
    {{else}}<p class="empty">The ledger holds nothing yet. An entry is written by performing an act — loading a job, deciding a finding, signing a plan — not by posting one here.</p>{{end}}
  {{else}}
  <div class="scroll"><table>
    <tr><th>when</th><th>kind</th><th>verdict</th><th>actor</th><th>what</th></tr>
    {{range .Rows}}<tr>
      <td class="muted">{{.At}}</td>
      <td><a href="/app/decisions?kind={{.Kind}}"><code>{{.Kind}}</code></a></td>
      <td><span class="pill {{.VerdictClass}}">{{.Verdict}}</span></td>
      <td>{{.Actor}}</td>
      <td><a href="/app/decisions/{{.Href}}">{{if .Line}}{{.Line}}{{else}}{{.ID}}{{end}}</a></td>
    </tr>{{end}}
  </table></div>
  {{end}}
</div>
`))

var ledgerChainTmpl = template.Must(template.New("decision").Parse(`
<div class="card">
  <h2 style="margin-top:0">The entry</h2>
  <div class="scroll"><table>
    <tr><th>id</th><td><code>{{.Entry.ID}}</code></td></tr>
    <tr><th>kind</th><td><a href="/app/decisions?kind={{.Entry.Kind}}"><code>{{.Entry.Kind}}</code></a></td></tr>
    <tr><th>actor</th><td><a href="/app/decisions?actor={{.Entry.Actor}}">{{.Entry.Actor}}</a></td></tr>
    <tr><th>verdict</th><td><span class="pill {{.Entry.VerdictClass}}">{{.Entry.Verdict}}</span></td></tr>
    <tr><th>when</th><td class="muted">{{.Entry.At}}</td></tr>
    {{if .Entry.Subject}}<tr><th>about</th><td><a href="/app/decisions?subject={{.Entry.Subject}}"><code>{{.Entry.Subject}}</code></a></td></tr>{{end}}
  </table></div>
  {{if .Entry.Line}}<p style="margin-bottom:0">{{.Entry.Line}}</p>{{end}}
</div>

{{if or .Fields .Raw}}
<div class="card">
  <h2 style="margin-top:0">What the entry records</h2>
  {{if .Fields}}<div class="scroll"><table>
    {{range .Fields}}<tr><th>{{.Key}}</th><td>{{.Value}}</td></tr>{{end}}
  </table></div>
  {{else}}<p class="muted">The detail on this entry is not the JSON object the ledger writes, so it is shown as it stands.</p>
  <pre class="mono scroll">{{.Raw}}</pre>{{end}}
</div>
{{end}}

<div class="card">
  <h2 style="margin-top:0">What it rests on</h2>
  {{if not .Premises}}<p class="empty">Nothing. This entry stands on its own.</p>
  {{else}}<div class="scroll"><table>
    <tr><th>premise</th><th>grade</th><th>kind</th></tr>
    {{range .Premises}}<tr>
      <td>{{if .Href}}<a href="/app/decisions/{{.Href}}">{{.Label}}</a>{{else}}{{.Label}}{{end}}
        {{if .Missing}}<br><span class="g-refused">recorded, and no longer on the shelf</span>{{end}}</td>
      <td>{{if .Grade}}<code class="g-{{.Grade}}">{{.Grade}}</code>{{else}}<span class="muted">none</span>{{end}}</td>
      <td class="muted">{{if .Decision}}decision{{else}}{{.Type}}{{end}}</td>
    </tr>{{end}}
  </table></div>{{end}}
</div>

<div class="card">
  <h2 style="margin-top:0">The chain, walked back</h2>
  {{if not .Chain}}<p class="empty">This entry rests on no other decision, so the chain is the entry.</p>
  {{else}}
  <p class="muted">{{len .Chain}} further decision{{if ne (len .Chain) 1}}s{{end}} in {{.Depth}} hop{{if ne .Depth 1}}s{{end}}.
    <code>GET {{.APIPath}}</code> answers the same from a script.</p>
  <div class="scroll"><table>
    <tr><th>when</th><th>kind</th><th>verdict</th><th>actor</th><th>what</th></tr>
    {{range .Chain}}<tr>
      <td class="muted">{{.At}}</td>
      <td><code>{{.Kind}}</code></td>
      <td><span class="pill {{.VerdictClass}}">{{.Verdict}}</span></td>
      <td>{{.Actor}}</td>
      <td><a href="/app/decisions/{{.Href}}">{{if .Line}}{{.Line}}{{else}}{{.ID}}{{end}}</a></td>
    </tr>{{end}}
  </table></div>
  {{if .Truncated}}<p class="g-refused">The walk stopped at the depth bound with decisions still unvisited — this is not the whole account.</p>{{end}}
  {{end}}
</div>
`))

// ledgerRow is one entry as a table line: the same five columns on both
// screens, so a decision looks the same wherever it is met.
type ledgerRow struct {
	ID, Href              string
	Kind, Actor, At       string
	Verdict, VerdictClass string
	Line, Subject         string
}

// ledgerField is one key of the note's structured detail, rendered rather than
// dumped. The value is already a string by the time a template sees it.
type ledgerField struct{ Key, Value string }

// ledgerPremiseRow is one thing an entry rested on. Href is set only for a
// premise that is itself a decision, because that is the only one with a page.
type ledgerPremiseRow struct {
	Href, Label, Grade, Type string
	Decision, Missing        bool
}

type ledgerListView struct {
	Query    ledgerQuery
	Confined string
	Filtered bool
	APIPath  string
	Rows     []ledgerRow
	Err      string
}

type ledgerChainView struct {
	Entry     ledgerRow
	Fields    []ledgerField
	Raw       string
	Premises  []ledgerPremiseRow
	Chain     []ledgerRow
	Depth     int
	Truncated bool
	APIPath   string
}

func (s *Server) handleUIDecisions(w http.ResponseWriter, r *http.Request) {
	key, ok := s.uiLedgerKey(w, r)
	if !ok {
		return
	}
	q := ledgerQuery{
		Kind:    ledgerTrim(r.URL.Query().Get("kind")),
		Subject: ledgerTrim(r.URL.Query().Get("subject")),
		Actor:   ledgerTrim(r.URL.Query().Get("actor")),
		Limit:   ledgerLimit(r.URL.Query().Get("limit")),
	}
	view := ledgerListView{
		Query:    q,
		Filtered: q.Kind != "" || q.Subject != "" || q.Actor != "",
	}

	// Exactly handleDecisions' confinement, in the same order: a confined key
	// that asked for somebody else's entries is answered with none rather than
	// with its own, because silently rewriting the question would let the page
	// claim an actor's ledger is empty when it is only unreadable.
	asked := true
	if actor, confined := ledgerConfinement(key); confined {
		view.Confined = actor
		if actor == "" || (q.Actor != "" && q.Actor != actor) {
			asked = false
		} else {
			q.Actor = actor
			view.Query.Actor = actor
		}
	}
	view.APIPath = "/athanor/decisions" + ledgerAPIQuery(q)

	if asked {
		recs, err := s.readLedger(r.Context(), q)
		if err != nil {
			view.Err = err.Error()
		}
		for _, rec := range recs {
			view.Rows = append(view.Rows, ledgerRowOf(rec))
		}
	}

	body, err := uiRender(ledgerListTmpl, view)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the ledger could not be drawn")
		return
	}
	s.renderUI(w, uiPage{
		Title: "Decisions", Nav: "/app/decisions",
		Lede: "Every act this server performed, and every one an agent recorded through it. " +
			"An entry is written by doing the thing, so this list is what happened and not what was claimed.",
		Body: body,
	})
}

func (s *Server) handleUIDecisionChain(w http.ResponseWriter, r *http.Request) {
	key, ok := s.uiLedgerKey(w, r)
	if !ok {
		return
	}
	id := ledgerTrim(r.PathValue("id"))
	if id == "" {
		http.Redirect(w, r, "/app/decisions", http.StatusSeeOther)
		return
	}
	depth := uiLedgerDepth
	if raw := ledgerTrim(r.URL.Query().Get("depth")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			depth = n
		}
	}

	chain, err := s.db.DecisionChain(r.Context(), id, depth)
	if err != nil || len(chain.Decisions) == 0 {
		s.uiNoSuchDecision(w)
		return
	}
	// The root's actor decides, as on the JSON route: a chain reaches
	// decisions other people made, and refusing one because a premise two
	// hops back was signed by somebody else would make every shared decision
	// unreadable. The answer for an entry that is not theirs is the answer for
	// an entry that is not there, word for word.
	if actor, confined := ledgerConfinement(key); confined {
		if actor == "" || chain.Decisions[0].Actor != actor {
			s.uiNoSuchDecision(w)
			return
		}
	}

	root := chain.Decisions[0]
	view := ledgerChainView{
		Entry:     ledgerRowOf(root),
		Depth:     chain.Depth,
		Truncated: chain.Truncated,
		APIPath:   "/athanor/decisions/" + url.PathEscape(root.ID),
	}
	view.Fields, view.Raw = ledgerDetail(root.Note)

	// A premise that is itself a decision is labelled with the sentence that
	// decision was written as, when the walk brought it back — an id is a
	// true label and a useless one.
	byID := make(map[string]cortexdb.DecisionRecord, len(chain.Decisions))
	for _, rec := range chain.Decisions {
		byID[rec.ID] = rec
	}
	for _, p := range root.Premises {
		row := ledgerPremiseRow{
			Label: p.ID, Grade: p.Grade, Type: p.Type,
			Decision: p.Decision, Missing: p.Missing,
		}
		if !p.Decision {
			row.Label = recordLine(p.Edge, p.Content, p.From, p.Type, p.To)
		} else {
			row.Href = url.PathEscape(p.ID)
			if rec, held := byID[p.ID]; held {
				if line := ledgerLine(rec.Note); line != "" {
					row.Label = line
				}
			}
		}
		view.Premises = append(view.Premises, row)
	}
	for _, rec := range chain.Decisions[1:] {
		view.Chain = append(view.Chain, ledgerRowOf(rec))
	}

	body, err := uiRender(ledgerChainTmpl, view)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the decision could not be drawn")
		return
	}
	s.renderUI(w, uiPage{
		Title: "Decision", Nav: "/app/decisions",
		Lede: "One act, what it rests on, and what that rests on. This is the walk that answers " +
			"where a record came from.",
		Body: body,
	})
}

// uiLedgerKey is the browser's door onto the ledger: uiKey for the redirect a
// page owes an unsigned browser, then the clearance authorizeLedger asks for.
// Reading the ledger is a read, so a read-only key passes.
func (s *Server) uiLedgerKey(w http.ResponseWriter, r *http.Request) (authz.Key, bool) {
	key, ok := s.uiKey(w, r)
	if !ok {
		return authz.Key{}, false
	}
	if err := key.AuthorizeOperation(ledgerOperation, authz.Method{Access: authz.Read}); err != nil {
		s.renderUIStatus(w, http.StatusForbidden, uiPage{
			Title: "Decisions", Nav: "/app/decisions",
			Notice: err.Error(), Bad: true,
		})
		return authz.Key{}, false
	}
	return key, true
}

// uiNoSuchDecision is the one answer for an entry that is not there and for
// one that is not the caller's — the same page, the same words, the same
// status, so the id space is not an oracle for what somebody else decided.
func (s *Server) uiNoSuchDecision(w http.ResponseWriter) {
	s.renderUIStatus(w, http.StatusNotFound, uiPage{
		Title: "Decision", Nav: "/app/decisions",
		Notice: notFoundDecision, Bad: true,
		Body: template.HTML(`<div class="card"><p class="empty">` + // #nosec G203 -- constant
			`Nothing in the ledger carries that id. <a href="/app/decisions">Back to the ledger</a>.</p></div>`),
	})
}

// renderUIStatus is renderUI with a status line. The headers are set before
// the code so the layout's own Set calls, which come after, change nothing
// that has already gone out; the body is still rendered whole beforehand,
// because a template that fails halfway has already written half a page.
func (s *Server) renderUIStatus(w http.ResponseWriter, code int, page uiPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	s.renderUI(w, page)
}

// ledgerAPIQuery renders the query back as the JSON route's own, so the line
// the page prints is a URL that answers it.
func ledgerAPIQuery(q ledgerQuery) string {
	v := url.Values{}
	if q.Kind != "" {
		v.Set("kind", q.Kind)
	}
	if q.Subject != "" {
		v.Set("subject", q.Subject)
	}
	if q.Actor != "" {
		v.Set("actor", q.Actor)
	}
	if q.Limit > 0 && q.Limit != ledgerDefaultLimit {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

func ledgerRowOf(rec cortexdb.DecisionRecord) ledgerRow {
	return ledgerRow{
		ID:           rec.ID,
		Href:         url.PathEscape(rec.ID),
		Kind:         rec.Kind,
		Actor:        rec.Actor,
		At:           rec.At,
		Verdict:      rec.Verdict,
		VerdictClass: verdictClass(rec.Verdict),
		Line:         ledgerLine(rec.Note),
		Subject:      rec.Subject,
	}
}

// verdictClass tints the few verdicts whose colour a reader already knows from
// the grade column: something that ended a thing reads refused, something
// still waiting on a person reads held. The vocabulary is open, so anything
// else gets the plain pill rather than a colour invented for it.
func verdictClass(verdict string) string {
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case "stopped", "reject", "rejected", "refused", "retire", "retired", "declined":
		return "g-refused"
	case "proposed", "held", "drafted", "draft":
		return "g-held"
	default:
		return ""
	}
}

// ledgerLine is the sentence an entry was written as: the note's first line.
// The structured detail lives on the line below it (ledgerEntry.note) and is
// rendered as fields, not shown as a blob of JSON in a table cell.
func ledgerLine(note string) string {
	line, _, _ := strings.Cut(note, "\n")
	return strings.TrimSpace(line)
}

// ledgerDetail reads the note's second line back into the fields it was
// written from. Keys are sorted so the same entry renders the same twice, and
// a line that is not the object the ledger writes is handed back raw rather
// than dropped: a reader who is shown nothing concludes there was nothing,
// which is the one thing this page must not say by accident.
func ledgerDetail(note string) ([]ledgerField, string) {
	_, rest, found := strings.Cut(note, "\n")
	rest = strings.TrimSpace(rest)
	if !found || rest == "" {
		return nil, ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(rest), &m); err != nil || m == nil {
		return nil, rest
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fields := make([]ledgerField, 0, len(keys))
	for _, k := range keys {
		fields = append(fields, ledgerField{Key: k, Value: ledgerValue(m[k])})
	}
	return fields, ""
}

// ledgerValue renders one detail value for a cell. A string is itself — a
// quoted one would read as JSON, which is what this page exists to stop —
// and anything else is its JSON form, which for a number or a bool is the
// shortest true rendering there is.
func ledgerValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
