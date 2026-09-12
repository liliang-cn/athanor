package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// Take names the present moment.
//
// The order of what it does is the substance. The instant is stamped first,
// then the graph is counted at that instant, then the ladder is tallied, then
// the row is written — so what the row says was true is read after the moment
// it claims and never before it. The act's own record in the decision ledger
// is written by the caller afterwards, which is what keeps a snapshot from
// counting the entry that says it was taken.
func (s *Store) Take(ctx context.Context, req Taking) (Snapshot, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return Snapshot{}, fmt.Errorf("%w: a snapshot needs a name somebody will say out loud", ErrInvalid)
	}
	if strings.Contains(name, ":") {
		// The door spells an act as `{name}:drop`. A name carrying a colon
		// would make "nightly:drop" ambiguous between a moment called
		// "nightly:drop" and dropping "nightly", and an ambiguity in a path is
		// resolved by whichever branch the handler happens to check first.
		return Snapshot{}, fmt.Errorf("%w: a snapshot name cannot contain a colon: %q", ErrInvalid, name)
	}
	by := strings.TrimSpace(req.By)
	if by == "" {
		return Snapshot{}, fmt.Errorf("%w: a snapshot is what a later argument about what we believed then rests on, and one nobody is named for cannot be argued with", ErrUnsigned)
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		return Snapshot{}, fmt.Errorf("%w: no key signed this snapshot", ErrUnsigned)
	}

	previous, err := s.Get(ctx, name)
	switch {
	case err == nil && !req.Replace:
		return Snapshot{}, fmt.Errorf("%w: %s already names %s — say replace to move it", ErrExists, name, previous.At.Format(time.RFC3339Nano))
	case err != nil && !errors.Is(err, ErrNotFound):
		return Snapshot{}, err
	}
	replacing := err == nil

	at := s.at()
	// CortexDB's own counts, not a second walk of the same tables: a snapshot
	// that disagreed with an as-of read of its instant would be worse than no
	// snapshot, and the only way to be sure they agree is to ask the same
	// question.
	shape, err := s.db.GraphSnapshotAt(ctx, at, cortexdb.SnapshotOptions{})
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshots: count the graph at %s: %w", at.Format(time.RFC3339Nano), err)
	}
	// The ladder is a present-tense read — see the package note on why that is
	// also why a snapshot cannot be backdated — and it is stored rather than
	// recomputed because nothing can ask it again about this instant.
	grades, err := s.db.ContractTally(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshots: tally the shelf: %w", err)
	}

	snap := Snapshot{
		Name: name, At: at, State: Kept, Actor: actor, By: by, Note: req.Note,
		Counts:  Counts{Nodes: shape.Nodes, Edges: shape.Edges, Orphans: shape.Orphans},
		Grades:  grades,
		Footing: req.Footing,
	}
	if replacing {
		was := previous.At
		snap.Replaced = &was
	}

	encodedGrades, err := json.Marshal(grades)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshots: %s: recording its grades: %w", name, err)
	}
	ontologies, err := json.Marshal(footingMap(req.Footing.Ontologies))
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshots: %s: recording its vocabularies: %w", name, err)
	}
	loads, err := json.Marshal(footingList(req.Footing.Loads))
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshots: %s: recording its loads: %w", name, err)
	}
	more := 0
	if req.Footing.MoreLoads {
		more = 1
	}

	if replacing {
		// Everything about the previous moment goes, including the record that
		// it was dropped: a name that has been taken again is a name somebody
		// is using, and leaving "dropped by X" on a live row would read as a
		// fact about the moment it now names.
		const q = `UPDATE athanor_snapshots SET
			at = ?, state = ?, actor = ?, by_name = ?, note = ?,
			nodes = ?, edges = ?, orphans = ?, grades = ?, ontologies = ?,
			loads = ?, more_loads = ?, load_count = ?, replaced = ?,
			dropped_actor = '', dropped_by = '', dropped_at = NULL, drop_note = ''
			WHERE name = ?`
		if _, err := s.exec(ctx, q, at, string(Kept), actor, by, req.Note,
			snap.Counts.Nodes, snap.Counts.Edges, snap.Counts.Orphans,
			string(encodedGrades), string(ontologies), string(loads), more,
			req.Footing.LoadCount, previous.At, name); err != nil {
			return Snapshot{}, fmt.Errorf("snapshots: retake %s: %w", name, err)
		}
		return snap, nil
	}

	const q = `INSERT INTO athanor_snapshots
		(name, at, state, actor, by_name, note, nodes, edges, orphans,
		 grades, ontologies, loads, more_loads, load_count, replaced,
		 dropped_actor, dropped_by, dropped_at, drop_note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', '', NULL, '')`
	if _, err := s.exec(ctx, q, name, at, string(Kept), actor, by, req.Note,
		snap.Counts.Nodes, snap.Counts.Edges, snap.Counts.Orphans,
		string(encodedGrades), string(ontologies), string(loads), more,
		req.Footing.LoadCount); err != nil {
		// A name taken between the read above and this insert: two callers
		// naming one moment at once. The primary key is what decides, and the
		// refusal is the same one the read would have given.
		if _, taken := s.Get(ctx, name); taken == nil {
			return Snapshot{}, fmt.Errorf("%w: %s", ErrExists, name)
		}
		return Snapshot{}, fmt.Errorf("snapshots: take %s: %w", name, err)
	}
	return snap, nil
}

// Drop retires a name.
//
// The row stays. See State on why a delete here would destroy the only record
// of what the counts were and free nothing in exchange.
func (s *Store) Drop(ctx context.Context, name string, req Dropping) (Snapshot, error) {
	name = strings.TrimSpace(name)
	by := strings.TrimSpace(req.By)
	if by == "" {
		return Snapshot{}, fmt.Errorf("%w: retiring the moment a later argument would have rested on is a decision, not a cleanup", ErrUnsigned)
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		return Snapshot{}, fmt.Errorf("%w: no key signed this drop", ErrUnsigned)
	}
	snap, err := s.Get(ctx, name)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.State == Dropped {
		// Not a refusal. Dropping twice is somebody making sure, and answering
		// it with the row as it stands says what they wanted to know: it is
		// dropped, by this person, then.
		return snap, nil
	}
	at := s.at()
	const q = `UPDATE athanor_snapshots SET state = ?, dropped_actor = ?, dropped_by = ?, dropped_at = ?, drop_note = ?
		WHERE name = ?`
	if _, err := s.exec(ctx, q, string(Dropped), actor, by, at, req.Note, name); err != nil {
		return Snapshot{}, fmt.Errorf("snapshots: drop %s: %w", name, err)
	}
	snap.State, snap.DroppedActor, snap.DroppedBy, snap.DroppedAt, snap.DropNote = Dropped, actor, by, &at, req.Note
	return snap, nil
}

// footingMap and footingList render an absent footing as an empty JSON value
// rather than `null`, so a column read back by a later version of this code
// does not have to know the difference between "nothing was recorded" and
// "nothing was there".
func footingMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func footingList(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}
