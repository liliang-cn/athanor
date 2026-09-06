package livedb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
	"github.com/liliang-cn/cortexdb/v2/pkg/importflow"
)

// run imports under a signed plan.
func (s *storeImpl) run(ctx context.Context, req RunRequest, actor string) (RunReport, error) {
	p, err := s.load(ctx, nil, req.Plan)
	if err != nil {
		return RunReport{}, err
	}
	if p.State != Signed {
		return RunReport{}, fmt.Errorf("%w: %s is %s", ErrUnsigned, p.ID, p.State)
	}
	// The credential is supplied again because the store never kept it, and
	// checked against the source the plan was signed for — otherwise a plan
	// signed over a staging database would run against production the first
	// time somebody pasted the wrong DSN into a job.
	src := Source{
		Driver: p.Source.Driver,
		DSN:    req.DSN,
		Schema: p.Source.Schema,
		Tables: p.Source.Tables,
	}
	if err := validateSource(src); err != nil {
		return RunReport{}, err
	}
	if key := src.Key(); key != p.SourceKey {
		return RunReport{}, fmt.Errorf("%w: the credential describes %s, the plan was signed for %s", ErrWrongSource, key, p.SourceKey)
	}

	rep := RunReport{Plan: p.ID, SourceKey: p.SourceKey, DryRun: req.DryRun, StartedAt: s.now()}
	if rep.ID, err = mintID("run"); err != nil {
		return RunReport{}, err
	}

	live, err := s.opener.Open(ctx, src)
	if err != nil {
		return RunReport{}, err
	}
	defer func() { _ = live.Close() }()

	// Every run re-reads the schema. The desensitizer already fails closed on
	// a column the plan does not name, but silent correctness is not enough
	// for something a person signed: the drift is reported by name so the
	// operator learns that a re-signing is owed.
	schemas, err := live.Schemas(ctx)
	if err != nil {
		return RunReport{}, fmt.Errorf("livedb: read schemas: %w", err)
	}
	rep.Drift, rep.Gone = driftOf(p.Plan, schemas)

	des, err := connector.NewDesensitizer(maskingPlan(p.Plan), connector.DesensitizerOptions{
		Tenant:      s.tenant,
		KeyProvider: s.keys,
		Vault:       s.vault,
		// Named rather than defaulted. It is the same value the connector
		// would have chosen, and writing it here means a change to that
		// default cannot quietly turn drift into a leak.
		OnUnlisted: connector.ActionDrop,
	})
	if err != nil {
		return RunReport{}, fmt.Errorf("livedb: build the desensitizer for %s: %w", p.ID, err)
	}
	clean := connector.Desensitized(live, des)

	namespace := firstNamed(req.Namespace, p.ID)
	mapping := importflow.MappingPlan{}
	if req.Mapping != nil {
		mapping = withNamespace(*req.Mapping, namespace)
	} else {
		// Derived from the DESENSITIZED schemas, so a dropped column cannot
		// appear in a content template, a metadata list or an entity's
		// properties — the mapping never names what the plan removed.
		kept, err := clean.Schemas(ctx)
		if err != nil {
			return RunReport{}, fmt.Errorf("livedb: read the desensitized schemas: %w", err)
		}
		if mapping, err = s.deriveMapping(ctx, live, kept, namespace); err != nil {
			return RunReport{}, err
		}
	}

	if req.DryRun {
		// Reads and desensitizes and writes nothing, so an operator can see
		// the drift and the row count before committing to either. The
		// importer is not called at all: a dry run that reached the importer
		// would be relying on the importer to be talked out of writing.
		if err := clean.Records(ctx, func(importflow.Record) error {
			rep.RowsRead++
			return nil
		}); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		}
	} else {
		out, err := s.importer.Run(ctx, clean, mapping)
		if err != nil {
			return RunReport{}, fmt.Errorf("livedb: import under %s: %w", p.ID, err)
		}
		if out != nil {
			rep.RowsRead, rep.Chunks = out.RowsRead, out.ChunksIndexed
			rep.Triples, rep.Skipped = out.TriplesCreated, out.Skipped
			for _, e := range out.Errors {
				rep.Errors = append(rep.Errors, e.Error())
			}
			rep.Errors = append(rep.Errors, out.UnparsedStatements...)
		}
	}
	rep.EndedAt = s.now()

	if err := s.insertRun(ctx, rep); err != nil {
		return RunReport{}, err
	}
	act, err := s.act(ActRun, actor, rep.EndedAt, rep.ID, "", map[string]any{
		"plan":       p.ID,
		"source_key": p.SourceKey,
		"redacted":   p.Source.Redacted,
		"dry_run":    rep.DryRun,
		"rows_read":  rep.RowsRead,
		"chunks":     rep.Chunks,
		"triples":    rep.Triples,
		"drift":      rep.Drift,
		"gone":       rep.Gone,
	})
	if err != nil {
		return RunReport{}, err
	}
	// The run rests on the signature that permitted it, so "where did this
	// node come from" walks back to a name and from there to the column it
	// was made of.
	act.Premises = []string{p.ID}
	_ = s.mirror(ctx, act)

	if req.Follow {
		if err := s.follow(ctx, p, src, des, mapping); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// driftOf compares the signed plan against the schema as it is now. Both lists
// are "table.column" and both are sorted, so a report is comparable with the
// one before it.
func driftOf(p Plan, schemas []importflow.Schema) (drift, gone []string) {
	planned := map[string]bool{}
	for _, t := range p.Columns {
		planned[t.Table+"."+t.Column] = true
	}
	live := map[string]bool{}
	for _, sc := range schemas {
		for _, col := range sc.Columns {
			name := sc.Table + "." + col.Name
			live[name] = true
			if !planned[name] {
				drift = append(drift, name)
			}
		}
	}
	for name := range planned {
		if !live[name] {
			gone = append(gone, name)
		}
	}
	sort.Strings(drift)
	sort.Strings(gone)
	return drift, gone
}

// withNamespace fills in the RAG namespace on any table plan that left it
// empty, so RunRequest.Namespace means the same thing for a supplied mapping
// as for a derived one.
func withNamespace(in importflow.MappingPlan, namespace string) importflow.MappingPlan {
	out := importflow.MappingPlan{Tables: make(map[string]importflow.TablePlan, len(in.Tables))}
	for table, tp := range in.Tables {
		if tp.RAG != nil && tp.RAG.Namespace == "" {
			rag := *tp.RAG
			rag.Namespace = namespace
			tp.RAG = &rag
		}
		out.Tables[table] = tp
	}
	return out
}

// keySeparator joins a multi-column primary key into one id. A character no
// identifier contains, so "a-b" and "a" + "-b" cannot be the same node.
const keySeparator = "|"

// deriveMapping builds the deterministic mapping: primary key as the id, kept
// columns as content and properties, foreign keys as edges. It calls no model,
// which is the point — a mapping a model invented is a mapping nobody signed.
func (s *storeImpl) deriveMapping(ctx context.Context, live Live, kept []importflow.Schema, namespace string) (importflow.MappingPlan, error) {
	sort.Slice(kept, func(i, j int) bool { return kept[i].Table < kept[j].Table })
	out := importflow.MappingPlan{Tables: make(map[string]importflow.TablePlan, len(kept))}

	for _, sc := range kept {
		names := make([]string, 0, len(sc.Columns))
		present := map[string]bool{}
		for _, c := range sc.Columns {
			names = append(names, c.Name)
			present[c.Name] = true
		}
		if len(names) == 0 {
			// Every column dropped. Skipping is honest: an empty chunk is a
			// row in the brain that says nothing and matches everything.
			out.Tables[sc.Table] = importflow.TablePlan{Skip: true}
			continue
		}

		keys, err := live.Keys(ctx, sc.Table)
		if err != nil {
			return importflow.MappingPlan{}, fmt.Errorf("livedb: primary key of %s: %w", sc.Table, err)
		}
		// A key column the plan dropped is not a key this run can use.
		usable := make([]string, 0, len(keys))
		for _, k := range keys {
			if present[k] {
				usable = append(usable, k)
			}
		}

		lines := make([]string, 0, len(names))
		for _, n := range names {
			lines = append(lines, n+": {"+n+"}")
		}
		rag := &importflow.RAGPlan{
			Namespace:   namespace,
			ContentTmpl: strings.Join(lines, "\n"),
			Metadata:    names,
		}
		if len(usable) == 1 {
			rag.IDColumn = usable[0]
		}

		tp := importflow.TablePlan{RAG: rag}
		// A table with no usable primary key still gets a RAG plan —
		// importflow synthesizes "table:row" for the chunk id — but it gets
		// no KG entity. A node with no stable id is a node that duplicates on
		// every re-run, and a graph that grows a second copy of every row per
		// import is worse than a graph missing that table.
		if len(usable) > 0 {
			kg, err := s.deriveKG(ctx, live, sc.Table, usable, names, present)
			if err != nil {
				return importflow.MappingPlan{}, err
			}
			tp.KG = kg
		}
		out.Tables[sc.Table] = tp
	}
	return out, nil
}

func (s *storeImpl) deriveKG(ctx context.Context, live Live, table string, keys, props []string, present map[string]bool) (*importflow.KGPlan, error) {
	kg := &importflow.KGPlan{Entities: []importflow.EntityMap{{
		Ref:       table,
		Type:      table,
		IDTmpl:    entityIDTmpl(table, keys),
		LabelTmpl: entityIDTmpl(table, keys),
		Props:     props,
	}}}

	rels, err := live.Relations(ctx, table)
	if err != nil {
		return nil, fmt.Errorf("livedb: foreign keys of %s: %w", table, err)
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].Predicate < rels[j].Predicate })
	for _, r := range rels {
		if r.Target == "" || len(r.Columns) == 0 {
			continue
		}
		dropped := false
		for _, c := range r.Columns {
			if !present[c] {
				dropped = true
			}
		}
		if dropped {
			// The edge was made of a column the plan removed. There is
			// nothing left to point with, and inventing an edge from what
			// survived would be an edge nobody signed for.
			continue
		}
		// An edge needs both ends, and importflow only knows the ends a table
		// plan names — so the target is emitted as a second entity on THIS
		// table, keyed on the foreign-key column's value. Its id template is
		// the shape the target table's own entity uses, so the two mint the
		// same IRI and the node the edge lands on is the node the target
		// table's import fills in.
		ref := table + "_" + r.Predicate
		kg.Entities = append(kg.Entities, importflow.EntityMap{
			Ref:    ref,
			Type:   r.Target,
			IDTmpl: entityIDTmpl(r.Target, r.Columns),
		})
		kg.Relations = append(kg.Relations, importflow.RelationMap{
			Subject: table, Predicate: r.Predicate, Object: ref,
		})
	}
	return kg, nil
}

// entityIDTmpl is "<type>:{col}", or the columns joined for a composite key.
func entityIDTmpl(typ string, cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, "{"+c+"}")
	}
	return typ + ":" + strings.Join(parts, keySeparator)
}

// follow keeps the brain in step with the database after the first pass,
// reading changes through this same plan and this same desensitizer.
//
// It returns when ctx is done. A polling source drains once per Run call and a
// log-based one blocks until cancellation, so the loop covers both without
// asking which it got.
func (s *storeImpl) follow(ctx context.Context, p storedPlan, src Source, des *connector.Desensitizer, mapping importflow.MappingPlan) error {
	// The watcher's own validation would refuse this, but it would refuse it
	// without saying which table, and "CDC needs a stable key" is not an
	// answer an operator can act on.
	for table, tp := range mapping.Tables {
		if tp.Skip || tp.RAG == nil {
			continue
		}
		if tp.RAG.IDColumn == "" {
			return fmt.Errorf("livedb: %s cannot be followed: it has no single-column primary key, so a change has nothing to address the chunk it should replace", table)
		}
	}

	cp, _, err := s.checkpoints().Load(ctx, p.SourceKey)
	if err != nil {
		return err
	}
	changes, err := s.opener.Changes(ctx, src, cp)
	if err != nil {
		return fmt.Errorf("livedb: open the change stream for %s: %w", p.SourceKey, err)
	}
	defer func() { _ = changes.Close() }()

	cols := map[string][]importflow.Column{}
	for table, tp := range mapping.Tables {
		if tp.Skip || tp.RAG == nil {
			continue
		}
		for _, name := range tp.RAG.Metadata {
			cols[table] = append(cols[table], importflow.Column{Name: name})
		}
	}
	w, err := connector.NewWatcher(s.brain, changes, connector.WatcherOptions{
		SourceKey:    p.SourceKey,
		Desensitizer: des,
		Mapping:      mapping,
		Checkpoint:   s.checkpoints(),
		Columns:      cols,
	})
	if err != nil {
		return fmt.Errorf("livedb: watch %s: %w", p.SourceKey, err)
	}
	for ctx.Err() == nil {
		if err := w.Run(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			return fmt.Errorf("livedb: follow %s: %w", p.SourceKey, err)
		}
	}
	// Cancellation is how following ends, not how it fails.
	return nil
}

func (s *storeImpl) insertRun(ctx context.Context, r RunReport) error {
	drift, _ := json.Marshal(orEmpty(r.Drift))
	gone, _ := json.Marshal(orEmpty(r.Gone))
	errs, _ := json.Marshal(orEmpty(r.Errors))
	dry := 0
	if r.DryRun {
		dry = 1
	}
	const q = `INSERT INTO athanor_livedb_runs
		(id, plan, source_key, started_at, ended_at, dry_run, rows_read, chunks, triples, skipped, drift, gone, errors)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.exec(ctx, q, r.ID, r.Plan, r.SourceKey, r.StartedAt, r.EndedAt, dry,
		r.RowsRead, r.Chunks, r.Triples, r.Skipped, string(drift), string(gone), string(errs)); err != nil {
		return fmt.Errorf("livedb: record run %s: %w", r.ID, err)
	}
	return nil
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
