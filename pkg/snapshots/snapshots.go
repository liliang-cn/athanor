// Package snapshots is the named moment: what the brain held, when, and who
// said so.
//
// CortexDB v2.100.0 holds the storage half already. The property graph is
// bitemporal — valid_from/valid_to and recorded_at/retracted_at on every node
// and edge, superseded rows moved into history tables, and a read wrapped in
// cortexdb.AsOf answering as the store stood then. GraphSnapshotAt counts the
// graph at an instant and GraphDiff walks what changed between two, in bounded
// pages, without ever holding a whole graph in memory.
//
// What none of that has is a name, a date and a signature. An instant is a
// string a caller has to have written down somewhere; "the graph as it was
// before the migration" is a fact about an afternoon that lives in somebody's
// head, and an audit that rests on a remembered timestamp is not an audit.
// This package is the half that has to be somewhere: which moments were
// named, who named them, what the brain held at each, and what the moment
// rested on.
//
// # Nothing here copies the graph
//
// A snapshot row is a few kilobytes whatever the brain weighs, and that is a
// decision rather than an optimisation. The bytes of the graph as it stood are
// already in the brain — that is what the history tables are — so a snapshot
// that serialised them would be a second copy of rows that are still there,
// one that drifts the moment anything is retracted and that a backup carries
// twice. What the row holds is the instant, what the counts were, and what the
// moment stood on. The graph itself is read back through cortexdb.AsOf at the
// stored instant, by the same machinery every other past read uses, so a
// snapshot and a GetNode at its instant can never disagree.
//
// Two consequences, and both are honest to say out loud rather than discover:
//
//   - cortexdb.VacuumGraph physically deletes history closed before a cutoff.
//     A snapshot older than the last vacuum still names a real moment and its
//     stored counts are still what was true then, but the rows behind it are
//     gone and a diff reaching back past the cutoff reports less than
//     happened. The counts are stored rather than recomputed for exactly this
//     reason: the numbers survive the vacuum even when the rows do not.
//   - Only the property graph is bitemporal. Chunks, vectors, memories and the
//     RDF triple store carry no temporal columns in v2.100.0, so a snapshot is
//     a statement about the graph and says nothing about the rest of the
//     brain. It does not claim to.
//
// # Why a snapshot is always of now
//
// Take stamps the moment itself and refuses to be told one. The graph could
// answer for a past instant, but the grade ladder cannot: ContractTally goes
// through graph.PropertyCounts, which queries graph_nodes and graph_edges
// directly and ignores the as-of on the context. A backdated snapshot would
// therefore carry today's grades under yesterday's date, and a number that is
// wrong inside a record is worse than a feature that is absent.
package snapshots

import (
	"errors"
	"time"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// State is whether a named moment is still one this brain keeps.
//
// Two, and the second one is not a delete. Dropping a snapshot frees nothing —
// there were never any bytes to free — so all it can honestly mean is that
// nobody is to go on treating this name as a moment worth comparing against.
// The row stays, with who dropped it and when, because it holds the only
// record of what the counts were at that instant, and deleting it to honour a
// request that reclaims no space would destroy the one thing a snapshot is.
type State string

const (
	// Kept is a moment this brain still names.
	Kept State = "kept"
	// Dropped is one somebody retired. Still readable, still diffable, and no
	// longer listed by default.
	Dropped State = "dropped"
)

// Counts is the size of the graph at the instant, from CortexDB's own
// GraphSnapshotAt so that a snapshot's numbers and an as-of read of the same
// instant come from one place.
type Counts struct {
	Nodes int `json:"nodes"`
	Edges int `json:"edges"`
	// Orphans is how many of those nodes no edge touched — the health number
	// the brain already reports for the present, kept here so an operator can
	// watch it move rather than only read today's.
	Orphans int `json:"orphans"`
}

// Footing is what the moment rested on: the vocabulary that was in force and
// the loads that had landed.
//
// It is gathered by the caller rather than read here, because both facts live
// in tables other packages own — pkg/ontologies has the published versions and
// the decision ledger has the loads — and a store that reached into them would
// be a second reader of somebody else's rows, disagreeing with the first the
// day either changes.
type Footing struct {
	// Ontologies maps a lineage to the version that was published at the
	// instant. A graph is only explicable against the vocabulary that checked
	// it, and "which one was current in March" is otherwise a question with no
	// answer once a later version is published.
	Ontologies map[string]string `json:"ontologies,omitempty"`
	// Loads are the ledger entry ids of the loads that had landed, newest
	// first. Ids rather than a count, so a reader can open each one; capped by
	// the caller, because a brain that has been fed for a year has thousands
	// and a snapshot is not a listing.
	Loads []string `json:"loads,omitempty"`
	// MoreLoads says the list was cut. LoadCount is how many there were.
	MoreLoads bool `json:"more_loads,omitempty"`
	LoadCount int  `json:"load_count,omitempty"`
}

// Snapshot is one named moment.
type Snapshot struct {
	// Name is the caller's own word for the moment — "before-the-migration",
	// "nightly" — and it is the primary key. A snapshot's identity is the name
	// somebody will say out loud; a minted id would mean an operator asking
	// for a diff has to look up two hexadecimal strings first.
	Name string `json:"name"`
	// At is the instant the graph is read at. It is also when the act
	// happened: taking a snapshot of a moment you were not present for is
	// refused (see the package note), so one column cannot disagree with the
	// other.
	At    time.Time `json:"at"`
	State State     `json:"state"`

	// Actor is the key id the act authenticated as — the identity an operator
	// can revoke, and the one row confinement is checked against. By is the
	// name typed into the request body, which nobody checked. Both are kept:
	// when they disagree, the disagreement is the record.
	Actor string `json:"actor"`
	By    string `json:"by,omitempty"`
	Note  string `json:"note,omitempty"`

	Counts  Counts                 `json:"counts"`
	Grades  cortexdb.ContractTally `json:"grades"`
	Footing Footing                `json:"footing"`

	// Replaced is the instant this name used to mean, when a take moved it.
	// Kept because moving a name forward is the one act here that loses
	// something, and the thing it loses should at least be named.
	Replaced *time.Time `json:"replaced,omitempty"`

	DroppedActor string     `json:"dropped_actor,omitempty"`
	DroppedBy    string     `json:"dropped_by,omitempty"`
	DroppedAt    *time.Time `json:"dropped_at,omitempty"`
	DropNote     string     `json:"drop_note,omitempty"`
}

// Taking is one request to capture the present moment.
type Taking struct {
	// Name is what the moment is called. Empty is refused, and so is a name
	// carrying a colon: the door spells an act as `{name}:drop`, and a name
	// with a colon in it makes that ambiguous.
	Name string
	// By is the person taking it. Empty is refused, for the reason an
	// unsigned ontology approval is: a snapshot is what a later argument about
	// "what did we believe then" rests on, and one nobody is named for cannot
	// be argued with.
	By string
	// Actor is the key id the door resolved. Never taken from the body.
	Actor string
	Note  string
	// Replace moves an existing name to the present moment. Without it a name
	// already taken is refused, because silently re-pointing "before the
	// migration" at this afternoon would answer every later question wrongly
	// and say nothing about it.
	Replace bool
	// Footing is what the door knew the moment rested on.
	Footing Footing
}

// Dropping retires a name.
type Dropping struct {
	// By is the person. Empty is refused: retiring the moment a later argument
	// would have rested on is a decision, not a cleanup.
	By string
	// Actor is the key id the door resolved.
	Actor string
	Note  string
}

// The refusals, as values, so an HTTP surface turns each into the status it
// deserves rather than reading error strings.
var (
	// ErrNotFound is a name nothing answers to.
	ErrNotFound = errors.New("snapshots: no such snapshot")
	// ErrExists is a name already taken. Names are not silently reused: two
	// moments under one name is the one thing that makes every diff drawn
	// against that name wrong.
	ErrExists = errors.New("snapshots: that name is already a moment")
	// ErrUnsigned is an act nobody is named for.
	ErrUnsigned = errors.New("snapshots: the act needs a name")
	// ErrInvalid is a malformed request — an empty name, a diff whose ends are
	// the wrong way round.
	ErrInvalid = errors.New("snapshots: refused")
)
