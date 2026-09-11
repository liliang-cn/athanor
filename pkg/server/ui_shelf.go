package server

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The shelf: what the brain holds, and how much of it anybody checked.
//
// This is the reference screen. The other two follow its shape, so it is worth
// saying what the shape is: a handler resolves the browser's key, asks the
// brain, builds one flat view struct, renders its own body template, and hands
// that to the layout. No screen queries in a template, and no screen writes to
// the response before its body is fully rendered.
//
// The one judgement in it: a section whose query failed shows as failed rather
// than as empty. A page with four questions on it that renders three and
// silently drops the fourth tells a reader the brain holds nothing of that
// kind, which is a different and worse claim than "I could not ask".

const shelfGradedLimit = 25

var shelfTmpl = template.Must(template.New("shelf").Parse(`
<div class="card">
  <h2 style="margin-top:0">What the shelf stands on</h2>
  {{if .TallyErr}}<p class="empty">The tally could not be read: {{.TallyErr}}</p>{{else}}
  <div class="scroll"><table class="cards">
    <tr class="hd"><th>grade</th><th>meaning</th><th class="n">nodes</th><th class="n">edges</th></tr>
    {{range .Tally}}<tr>
      <td class="id" data-label="grade"><a class="g-{{.Grade}}" href="/app/shelf?grade={{.Grade}}"><code>{{.Grade}}</code></a></td>
      <td class="muted" data-label="meaning">{{.Meaning}}</td>
      <td class="n" data-label="nodes">{{.Nodes}}</td><td class="n" data-label="edges">{{.Edges}}</td>
    </tr>{{end}}
  </table></div>
  {{end}}
</div>

<div class="card">
  <h2 style="margin-top:0">Wants a person</h2>
  {{if .AttentionErr}}<p class="empty">Could not be read: {{.AttentionErr}}</p>
  {{else if not .Attention}}<p class="empty">Nothing is held or refused.</p>
  {{else}}<div class="scroll"><table class="cards">
    <tr class="hd"><th>grade</th><th>record</th><th>why</th><th>from</th></tr>
    {{range .Attention}}<tr>
      <td class="g-{{.Grade}}" data-label="grade"><code>{{.Grade}}</code></td>
      <td class="id" data-label="record">{{.Record}}</td><td class="g-held" data-label="why">{{.Why}}</td>
      <td class="muted" data-label="from">{{.Source}}</td>
    </tr>{{end}}
  </table></div>{{end}}
</div>

<div class="card">
  <h2 style="margin-top:0">Records{{if .Grade}} graded <code class="g-{{.Grade}}">{{.Grade}}</code>{{end}}</h2>
  {{if not .Grade}}
    <p class="empty">Pick a grade above to read the records behind the count.</p>
  {{else if .GradedErr}}<p class="empty">Could not be read: {{.GradedErr}}</p>
  {{else if not .Graded}}<p class="empty">Nothing carries that grade.</p>
  {{else}}
  <p class="muted">The first {{len .Graded}}. <code>GET /brain/v1/tools/graded_records</code> asks the same question from a script.</p>
  <div class="scroll"><table class="cards">
    <tr class="hd"><th>record</th><th>source</th><th>producer</th><th>when</th></tr>
    {{range .Graded}}<tr>
      <td class="id" data-label="record">{{.Record}}{{if .Why}}<br><span class="muted">{{.Why}}</span>{{end}}</td>
      <td class="muted" data-label="source">{{.Source}}</td><td class="muted" data-label="producer">{{.Producer}}</td>
      <td class="muted" data-label="when">{{.At}}</td>
    </tr>{{end}}
  </table></div>{{end}}
</div>
`))

type shelfRecord struct{ Record, Why, Source, Producer, At string }

type shelfView struct {
	Tally        []tallyRow
	TallyErr     string
	Attention    []attentionRow
	AttentionErr string
	Grade        string
	Graded       []shelfRecord
	GradedErr    string
}

func (s *Server) handleUIShelf(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.uiKey(w, r); !ok {
		return
	}
	ctx := r.Context()
	view := shelfView{Grade: shelfGrade(r.URL.Query().Get("grade"))}

	if t, err := s.db.ContractTally(ctx); err != nil {
		view.TallyErr = err.Error()
	} else {
		view.Tally = tallyRows(t)
	}

	if att, err := s.db.NeedsAttention(ctx, 20); err != nil {
		view.AttentionErr = err.Error()
	} else {
		for _, a := range att {
			view.Attention = append(view.Attention, attentionRow{
				Grade: a.Grade, Record: recordLine(a.Edge, a.Content, a.From, a.Type, a.To),
				Why: a.Why, Source: a.Source,
			})
		}
	}

	if view.Grade != "" {
		recs, err := s.db.GradedRecords(ctx, cortexdb.GradedQuery{
			Grades: []string{view.Grade}, Limit: shelfGradedLimit,
		})
		if err != nil {
			view.GradedErr = err.Error()
		}
		for _, rec := range recs {
			view.Graded = append(view.Graded, shelfRecord{
				Record:   recordLine(rec.Edge, rec.Content, rec.From, rec.Type, rec.To),
				Why:      rec.Why,
				Source:   rec.Source,
				Producer: rec.Producer,
				At:       rec.At,
			})
		}
	}

	body, err := uiRender(shelfTmpl, view)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the shelf could not be drawn")
		return
	}
	s.renderUI(w, uiPage{
		Title: "Shelf", Nav: "/app/shelf",
		Lede: "Every record carries which file, which chunk and which producer it came from, and a grade. " +
			"This is the count, including the bad ones.",
		Body: body,
	})
}

// shelfGrade keeps the query parameter to the closed set the contract defines.
// An unknown grade shows no records rather than passing a caller's string into
// a query that would answer honestly and confusingly with nothing.
func shelfGrade(raw string) string {
	switch g := strings.TrimSpace(raw); g {
	case "verified", "self_consistent", "asserted", "held", "refused", "untagged":
		return g
	default:
		return ""
	}
}

// recordLine renders a node as its content and an edge as the sentence it is.
func recordLine(edge bool, content, from, typ, to string) string {
	if edge {
		return from + " —" + typ + "→ " + to
	}
	if strings.TrimSpace(content) == "" {
		return "(no content)"
	}
	return content
}
