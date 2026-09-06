package livedb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
	"github.com/liliang-cn/cortexdb/v2/pkg/importflow"
)

// propose reads the source's schemas, classifies every column and writes a
// draft. Nothing beyond the sample rows is read and nothing at all is written
// to the brain's knowledge — a proposal is a question, and a question that
// imported half a table would be a bad one.
func (s *storeImpl) propose(ctx context.Context, src Source, opts ProposeOptions) (Plan, error) {
	if err := validateSource(src); err != nil {
		return Plan{}, err
	}
	live, err := s.opener.Open(ctx, src)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = live.Close() }()

	schemas, err := live.Schemas(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("livedb: read schemas: %w", err)
	}
	if len(schemas) == 0 {
		return Plan{}, fmt.Errorf("livedb: %s holds no table this plan could cover", src.Driver)
	}
	// Sorted here rather than trusted from the driver: the treatment order is
	// hashed, and a hash that depends on the order information_schema felt
	// like returning is a hash that changes for no reason anybody signed.
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Table < schemas[j].Table })

	// An empty allow-list is resolved to the real list, so a plan always
	// names what it covers and its source key is the key of a fixed tableset
	// rather than of "whatever was there that day".
	tables := make([]string, 0, len(schemas))
	for _, sc := range schemas {
		tables = append(tables, sc.Table)
	}

	mp, err := connector.BuildMaskingPlan(ctx, live, connector.NewRuleClassifier(), connector.PlanOptions{
		DefaultAction:   firstAction(opts.DefaultAction, connector.ActionRedact),
		ActionFor:       opts.ActionFor,
		ScanTextColumns: opts.ScanText,
	})
	if err != nil {
		return Plan{}, fmt.Errorf("livedb: classify: %w", err)
	}

	p := storedPlan{}
	p.Source = src.resolved(tables)
	p.SourceKey = p.Source.Key()
	p.Columns = treatmentsFor(schemas, mp)
	p.Hash = planHash(p.Plan)
	p.State = Draft
	p.Counts = countOf(p.Columns)
	p.CreatedBy = opts.By
	p.CreatedAt = s.at()
	p.Note = opts.Note
	if p.ID, err = mintID("plan"); err != nil {
		return Plan{}, err
	}

	if err := s.insertPlan(ctx, p); err != nil {
		return Plan{}, err
	}
	act, err := s.act(ActPropose, opts.By, p.CreatedAt, p.ID, opts.Note, map[string]any{
		"source_key": p.SourceKey,
		"redacted":   p.Source.Redacted,
		"tables":     p.Source.Tables,
		"hash":       p.Hash,
		"counts":     p.Counts,
	})
	if err != nil {
		return Plan{}, err
	}
	// The draft stands whether or not the audit view hears about it.
	_ = s.mirror(ctx, act)
	return p.Plan, nil
}

// firstAction is the caller's choice, or the fail-closed default.
func firstAction(chosen, fallback connector.MaskAction) connector.MaskAction {
	if chosen == "" {
		return fallback
	}
	return chosen
}

// treatmentsFor turns the connector's rules into the reviewable rows, in a
// stable order: table, then the column's ordinal position in the table. The
// ordinal is kept rather than sorted alphabetically because a person reading a
// table's columns reads them in the order the table declares them.
func treatmentsFor(schemas []importflow.Schema, mp connector.MaskingPlan) []Treatment {
	out := make([]Treatment, 0, len(mp.Columns))
	for _, sc := range schemas {
		for _, col := range sc.Columns {
			rule, ok := mp.RuleFor(sc.Table, col.Name)
			if !ok {
				// Not classified is not "keep": a column the plan does not
				// name is dropped by the desensitizer, and the plan should
				// say so rather than leave a blank row.
				rule = connector.ColumnRule{
					Table: sc.Table, Column: col.Name,
					Action: connector.ActionDrop, Source: "rule",
					Reason: "the classifier did not see this column",
				}
			}
			raw := firstSample(sc, col.Name)
			out = append(out, Treatment{
				Table: sc.Table, Column: col.Name, Type: col.Type,
				Kind: rule.PiiKind, Sensitivity: rule.Sensitivity, Action: rule.Action,
				Reason: rule.Reason, By: firstNamed(rule.Source, "rule"),
				Sample: sampleValue(rule.PiiKind, raw),
				Enters: entersValue(rule.PiiKind, rule.Action, raw),
				Scan:   mp.TextScanFor(sc.Table, col.Name),
			})
		}
	}
	return out
}

// firstSample is the first non-NULL value the source offered for a column.
func firstSample(sc importflow.Schema, col string) string {
	for _, r := range sc.Sample {
		if v, ok := r.Get(col); ok && v != "" {
			return v
		}
	}
	return ""
}

// sampleValue is what the reviewer is shown. A column the classifier believes
// is personal is shown masked; a column it believes is not is shown as it is,
// which is exactly the case where the reviewer has to be able to see that the
// classifier was wrong.
func sampleValue(kind connector.PiiKind, raw string) string {
	if raw == "" {
		return ""
	}
	if kind == connector.PiiNone {
		return raw
	}
	return connector.MaskValue(kind, raw)
}

// entersValue is the sample with the chosen action actually applied: what a
// row of this column turns into on its way into the brain.
//
// Hash and pseudonymize render as a shape rather than a value. The real token
// is minted at run time from a key this package does not hold while a plan is
// being read, and rendering a digest of a real value here would put a stable
// re-identifiable string of somebody's data on a review screen — which is the
// thing the masked sample exists to avoid.
func entersValue(kind connector.PiiKind, action connector.MaskAction, raw string) string {
	switch action {
	case connector.ActionDrop:
		return ""
	case connector.ActionKeep:
		return raw
	case connector.ActionRedact:
		return connector.Redact(raw)
	case connector.ActionMask:
		return connector.MaskValue(kind, raw)
	case connector.ActionGeneralize:
		return connector.GeneralizeValue(kind, raw)
	case connector.ActionHash:
		return "hash:" + strings.Repeat("x", 12)
	case connector.ActionPseudonymize:
		return "tok:" + strings.Repeat("x", 12)
	default:
		return connector.MaskValue(kind, raw)
	}
}

func firstNamed(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// countOf is the plan in one line.
func countOf(ts []Treatment) Counts {
	var c Counts
	c.Columns = len(ts)
	for _, t := range ts {
		if t.Kind != connector.PiiNone {
			c.Personal++
		}
		if t.Action.Reversible() {
			c.Reversible++
		}
		switch t.Action {
		case connector.ActionDrop:
			c.Dropped++
		case connector.ActionMask:
			c.Masked++
		case connector.ActionGeneralize:
			c.Generalized++
		case connector.ActionHash:
			c.Hashed++
		case connector.ActionRedact:
			c.Redacted++
		case connector.ActionKeep:
			c.Passed++
			// Kept prose with nothing looking inside it. The column-level
			// classifier judged the column and cannot judge the sentences.
			if t.Type == "text" && !t.Scan {
				c.UnscannedText++
			}
		}
	}
	return c
}

// canonicalPlan is what the hash is taken over: the source's identity and the
// decisions, in order, and nothing else.
//
// A struct rather than a map, and marshalled with encoding/json rather than
// rendered by hand. Go does sort a map's keys on marshal, but relying on that
// puts the stability of a signature inside somebody else's implementation
// note; a struct says the field order in the source file and cannot drift.
//
// Sample and Enters are deliberately outside it. They are what one sampling of
// a live table looked like on one afternoon — re-proposing tomorrow would
// produce different strings for the same decisions, and a signature that went
// stale because a row changed would train operators to re-sign without
// reading. Reason and By are outside it for the mirror image of that: who said
// so is provenance, and what was decided is the decision.
type canonicalPlan struct {
	SourceKey string               `json:"source_key"`
	Driver    string               `json:"driver"`
	Redacted  string               `json:"redacted"`
	Schema    string               `json:"schema"`
	Tables    []string             `json:"tables"`
	Columns   []canonicalTreatment `json:"columns"`
}

type canonicalTreatment struct {
	Table       string                `json:"table"`
	Column      string                `json:"column"`
	Type        string                `json:"type"`
	Kind        connector.PiiKind     `json:"pii_kind"`
	Sensitivity connector.Sensitivity `json:"sensitivity"`
	Action      connector.MaskAction  `json:"action"`
	Scan        bool                  `json:"scan"`
}

// planHash is what a signature names, and what makes "I signed this"
// checkable.
func planHash(p Plan) string {
	tables := append([]string(nil), p.Source.Tables...)
	sort.Strings(tables)
	c := canonicalPlan{
		SourceKey: p.SourceKey,
		Driver:    normalizeDriver(p.Source.Driver),
		Redacted:  p.Source.Redacted,
		Schema:    p.Source.Schema,
		Tables:    tables,
		Columns:   make([]canonicalTreatment, 0, len(p.Columns)),
	}
	for _, t := range p.Columns {
		c.Columns = append(c.Columns, canonicalTreatment{
			Table: t.Table, Column: t.Column, Type: t.Type,
			Kind: t.Kind, Sensitivity: t.Sensitivity, Action: t.Action,
			Scan: t.Scan,
		})
	}
	body, err := json.Marshal(c)
	if err != nil {
		// canonicalPlan holds strings, ints and slices of them; Marshal has
		// no way to fail on it. Panicking on the impossible is better than
		// returning a hash nobody can distinguish from a real one.
		panic(fmt.Sprintf("livedb: canonical plan will not marshal: %v", err))
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s *storeImpl) insertPlan(ctx context.Context, p storedPlan) error {
	tables, _ := json.Marshal(p.Source.Tables)
	treatments, _ := json.Marshal(p.Columns)
	counts, _ := json.Marshal(p.Counts)
	const q = `INSERT INTO athanor_livedb_plans
		(id, source_key, driver, redacted, db_schema, tables, treatments,
		 hash, state, counts, created_by, created_at, signed_by, signed_at, sign_act, supersedes, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', NULL, '', '', ?)`
	if _, err := s.exec(ctx, q,
		p.ID, p.SourceKey, p.Source.Driver, p.Source.Redacted, p.Source.Schema,
		string(tables), string(treatments),
		p.Hash, string(p.State), string(counts), p.CreatedBy, p.CreatedAt, p.Note,
	); err != nil {
		return fmt.Errorf("livedb: write draft %s: %w", p.ID, err)
	}
	return nil
}

// amend applies a human's overrides to a draft.
//
// A change naming a table or column the plan does not hold is an error rather
// than a silent no-op: the caller believed they were tightening a column, and
// a request that quietly did nothing would be signed as though it had.
//
// Enters is recomputed from the sample as the plan holds it, which for a
// personal column is the masked form rather than the original — the plan does
// not keep the original, which is the point of it. An operator who wants a
// faithful preview of a treatment they have just chosen re-proposes.
func (s *storeImpl) amend(ctx context.Context, id string, changes []Change, by string) (Plan, error) {
	p, err := s.load(ctx, nil, id)
	if err != nil {
		return Plan{}, err
	}
	if p.State != Draft {
		return Plan{}, fmt.Errorf("%w: %s is %s", ErrNotDraft, id, p.State)
	}
	if len(changes) == 0 {
		return p.Plan, nil
	}
	for _, ch := range changes {
		i := indexOf(p.Columns, ch.Table, ch.Column)
		if i < 0 {
			return Plan{}, fmt.Errorf("livedb: plan %s does not cover %s.%s", id, ch.Table, ch.Column)
		}
		t := p.Columns[i]
		if ch.Action != "" {
			t.Action = ch.Action
		}
		if ch.Kind != nil {
			t.Kind = *ch.Kind
			t.Sample = sampleValue(t.Kind, t.Sample)
		}
		if ch.Reason != "" {
			t.Reason = ch.Reason
		}
		t.By = firstNamed(by, "human")
		t.Enters = entersValue(t.Kind, t.Action, t.Sample)
		p.Columns[i] = t
	}
	p.Hash = planHash(p.Plan)
	p.Counts = countOf(p.Columns)

	treatments, _ := json.Marshal(p.Columns)
	counts, _ := json.Marshal(p.Counts)
	const q = `UPDATE athanor_livedb_plans SET treatments = ?, hash = ?, counts = ? WHERE id = ?`
	if _, err := s.exec(ctx, q, string(treatments), p.Hash, string(counts), id); err != nil {
		return Plan{}, fmt.Errorf("livedb: amend %s: %w", id, err)
	}
	return p.Plan, nil
}

func indexOf(ts []Treatment, table, column string) int {
	for i, t := range ts {
		if t.Table == table && t.Column == column {
			return i
		}
	}
	return -1
}

// sign puts a plan in force.
//
// One transaction, because superseding the previous plan and signing this one
// are one fact. A supersession that committed alone would leave a source with
// nothing in force and every run refused; a signature that committed alone
// would leave two in force, and the partial unique index refuses that outright
// rather than letting Current become a coin toss.
func (s *storeImpl) sign(ctx context.Context, id, hash, by, note string) (Plan, error) {
	if strings.TrimSpace(by) == "" {
		return Plan{}, fmt.Errorf("livedb: signing decides what leaves the database; it needs a name")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, fmt.Errorf("livedb: sign %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	p, err := s.load(ctx, tx, id)
	if err != nil {
		return Plan{}, err
	}
	if p.State != Draft {
		return Plan{}, fmt.Errorf("%w: %s is %s", ErrNotDraft, id, p.State)
	}
	if hash != p.Hash {
		return Plan{}, fmt.Errorf("%w: you named %s, the plan is %s", ErrStaleHash, short(hash), short(p.Hash))
	}
	// Refused at signing rather than at running, so the operator learns it
	// while deciding rather than an hour into an import.
	if p.Counts.Reversible > 0 && (s.vault == nil || s.keys == nil) {
		return Plan{}, fmt.Errorf("%w: %d column(s) are pseudonymized", ErrNoVault, p.Counts.Reversible)
	}

	at := s.at()
	var superseded string
	const findCurrent = `SELECT id FROM athanor_livedb_plans
		WHERE source_key = ? AND state = ? ORDER BY signed_at DESC, id DESC`
	err = s.txQueryRow(ctx, tx, findCurrent, p.SourceKey, string(Signed)).Scan(&superseded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		superseded = ""
	case err != nil:
		return Plan{}, fmt.Errorf("livedb: sign %s: finding the plan in force: %w", id, err)
	}
	if superseded != "" {
		const q = `UPDATE athanor_livedb_plans SET state = ? WHERE id = ?`
		if _, err := s.txExec(ctx, tx, q, string(Superseded), superseded); err != nil {
			return Plan{}, fmt.Errorf("livedb: supersede %s: %w", superseded, err)
		}
	}
	act, err := s.act(ActSign, by, at, id, note, map[string]any{
		"hash":       p.Hash,
		"source_key": p.SourceKey,
		"redacted":   p.Source.Redacted,
		"tables":     p.Source.Tables,
		"counts":     p.Counts,
		"supersedes": superseded,
	})
	if err != nil {
		return Plan{}, err
	}
	const q = `UPDATE athanor_livedb_plans
		SET state = ?, signed_by = ?, signed_at = ?, sign_act = ?, supersedes = ?, note = ? WHERE id = ?`
	if _, err := s.txExec(ctx, tx, q, string(Signed), by, at, act.ID, superseded, firstNamed(note, p.Note), id); err != nil {
		return Plan{}, fmt.Errorf("livedb: sign %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return Plan{}, fmt.Errorf("livedb: sign %s: %w", id, err)
	}
	// After the commit, never inside it.
	_ = s.mirror(ctx, act)

	p.State, p.SignedBy, p.SignedAt, p.Supersedes = Signed, by, at, superseded
	p.Note = firstNamed(note, p.Note)
	p.signAct = act.ID
	return p.Plan, nil
}

// act mints one record of a decision. There is no acts table here: the plans
// and the runs already hold what happened, and the ledger is where the acts of
// every Athanor package meet. What is kept is the signature's id, on the plan
// row, because a run names it as its premise.
func (s *storeImpl) act(kind, actor string, at time.Time, subject, note string, detail map[string]any) (Act, error) {
	id, err := mintID("act")
	if err != nil {
		return Act{}, err
	}
	return Act{
		ID: id, Kind: kind, Actor: actor, At: at.UTC(),
		Subject: subject, Note: note, Detail: detail,
	}, nil
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12] + "…"
}

// maskingPlan renders a Plan back into the connector's own type.
//
// A signed plan renders signed, so connector.NewDesensitizer accepts it; an
// unsigned one renders unsigned and the connector refuses it. The same
// refusal, enforced twice, in this package and in the one below it — which is
// what makes the rule hold for a caller who reached past Run and built a
// desensitizer of their own.
func maskingPlan(p Plan) connector.MaskingPlan {
	mp := connector.MaskingPlan{Columns: make([]connector.ColumnRule, 0, len(p.Columns))}
	for _, t := range p.Columns {
		mp.Columns = append(mp.Columns, connector.ColumnRule{
			Table: t.Table, Column: t.Column, PiiKind: t.Kind,
			Sensitivity: t.Sensitivity, Action: t.Action,
			Reason: t.Reason, Source: t.By,
		})
		if t.Scan {
			mp.TextScan = append(mp.TextScan, connector.TextScanRule{Table: t.Table, Column: t.Column})
		}
	}
	if p.State == Signed {
		mp.Sign(p.SignedBy, p.SignedAt)
	}
	return mp
}
