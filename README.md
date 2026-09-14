# Athanor

CortexDB's brain and alchemy's pipeline behind one door: files go in under a
vocabulary, a disagreement stops the job, a person answers it, and the answer
survives into a store that can still say how it knows. A database somebody else
runs goes in the same way, after somebody signs for which columns may leave the
building.

Every record in the brain carries where it came from — which file and chunk, or
which signed plan and run — and which producer made it, whether a source stated
it or a model inferred it, and a grade: `verified` when a named person kept it,
`refused` when the vocabulary declined it, `asserted` when something said so and
nobody checked. Ask the store what its shelf stands on and it answers with
numbers, including the ones that are bad.

It runs inside your network. Models are endpoints you supply per job; nothing
here hardcodes a host, a key or a vendor.

## Run

```sh
go build ./cmd/athanor
./athanor -db ./brain.db -keys ./keys.json
```

Two ports: gRPC on `127.0.0.1:47831` carrying `alchemy.v1` and `cortexdb.v1`,
HTTP on `127.0.0.1:47832` carrying their REST translations, the review UI
(`/ui`), the live graph (`/graph/`), metrics (`/metrics`) and the front page.
`deploy/` has a hardened systemd unit and a container image whose healthcheck is
the binary itself. `deploy/push user@host` copies this checkout onto a host and
rebuilds there; use it rather than rsync by hand, because the key policy and the
`.env` beside it live only on the host and a `--delete` without them protected
takes them with it.

Existing CortexDB clients point `CORTEXDB_REMOTE` at the gRPC port and need no
change. Every door reads one key file — see `deploy/keys.example.json`.

## The chain, over the doors

```sh
K='Authorization: Bearer <operator key>'
# 1. a file, under a vocabulary
curl -H "$K" -H 'Content-Type: application/octet-stream' --data-binary @runbook.md \
  'http://127.0.0.1:47832/v1/sources?name=runbook.md&kind=SOURCE_KIND_DOCUMENT&media_type=text/markdown'
curl -H "$K" -d '{"source_ids":["<id>"],"ontology":"<json>","part":"prose","models":{"llm":{"name":"…","endpoint":"…","api_key":"…"}}}' \
  http://127.0.0.1:47832/v1/jobs
# 2. held — two sources disagree — review it
open http://127.0.0.1:47832/ui/
# 3. finished → into the brain, graded
curl -H "$K" -d '{"job":"<id>","load":"runbook"}' http://127.0.0.1:47832/athanor/loads
# 3b. a record in it is wrong: say so, and say what it replaces
curl -H "$K" -d '{"by":"liliang","entities":[…],
  "supersedes":[{"retires":"<node id>","reason":"decommissioned in March"}]}' \
  http://127.0.0.1:47832/athanor/assertions
# 4. ask the brain how it knows
curl -H "$K" -d '{}' http://127.0.0.1:47832/brain/v1/tools/contract_tally
# what this brain holds, and how to take one back out
curl -H "$K" http://127.0.0.1:47832/athanor/loads
curl -H "$K" -X DELETE -d '{"by":"liliang","why":"loaded under the wrong vocabulary"}' \
  http://127.0.0.1:47832/athanor/loads/runbook
```

A load can be listed and it can be dropped, and for a while it could be
neither: the only thing that could be said about a load already in the brain
was to put another graph over it, which answers "this one is wrong" with "here
is a different one" — and there is not always a different one. A drop names a
person and a reason, like every act that changes what the shelf stands on, and
more so than most: what it takes is not recoverable from anything else here, so
its ledger entry is the only place the reason will survive. It is the one entry
in this store that is about something no longer in it.

A held job is refused at step 3 until someone answers at step 2. The brain
holds only graphs that finished, and finished means reviewed when review was
owed.

## The vocabulary, as a workflow

An ontology used to be a JSON string pasted into every `CreateJob`, with no
record of which edit was in force or who accepted the type that let a fact in.
alchemy has the two hard pieces — a run reports the types its corpus used and
the vocabulary lacked, and `Extend` declares the accepted ones under a new id —
and holds neither, because it holds nothing. Athanor holds them, in the brain,
in two tables beside the graph they govern.

```sh
# draft a vocabulary; the body is the document itself
curl -H "$K" -H 'Content-Type: application/json' --data-binary @sds.json \
  http://127.0.0.1:47832/athanor/ontologies
# a finished run wanted types it does not declare — record them
curl -H "$K" -d '{"job":"<id>","part":"prose"}' \
  'http://127.0.0.1:47832/athanor/ontologies/sds@1:propose'
# accept some of them: Extend runs, sds@2 is written, parent sds@1
curl -H "$K" -d '{"accept":["Cluster","member_of"],"by":"liliang","note":"both are real"}' \
  'http://127.0.0.1:47832/athanor/ontologies/sds@1:approve'
# make it current; sds@1 is retired, recorded
curl -H "$K" -d '{"by":"liliang"}' \
  'http://127.0.0.1:47832/athanor/ontologies/sds@2:publish'
# what a client pastes into the next CreateJob
curl -H "$K" 'http://127.0.0.1:47832/athanor/ontologies/current?lineage=sds'
```

`GET /athanor/ontologies` lists every version; `GET /athanor/ontologies/{id}`
is one of them with its state, parent, proposals and signatures. Reads take any
key from the policy, writes take a read-write one, same as every other door.

A held job is refused at `:propose` exactly as it is at `/athanor/loads`, and
by the same call: the proposals are pulled through alchemy's own `GetResult`.
Approving without a `by` is refused — a judgement about what a type means, and
one nobody is named for, cannot be argued with later. One version of a lineage
is published at a time; publishing the next retires the previous.

Retired is not deleted, and nothing that ran under a retired version is
touched. A job carries the document it was created with, every record it
produced names that id in its provenance, and the row keeps its body — so a
graph loaded under `sds@1` can still be asked what checked it, long after
`sds@2` became current.

## The rules, as a workflow

CortexDB has had a general rule engine since the rule became data: a Horn
clause over graph edges, forward-chained to a fixpoint, every derived edge
carrying the rule's id, the rule's text and the premise edges under it. It
holds no workflow, because it holds nothing — `rules_save` is an upsert into a
configuration table and `rules_apply` fires whatever is enabled, for whoever
asked, with no record that anybody asked. A rule that adds edges to the brain
is a claim about what is true, so Athanor holds the other half.

```sh
# declare a rule; the body is the document CortexDB's own rules_save takes
curl -H "$K" -H 'Content-Type: application/json' -d '{"id":"chain@1",
  "text":"IF manages(?a, ?b) AND manages(?b, ?c) THEN manages_chain(?a, ?c)",
  "note":"a manager'"'"'s manager manages you"}' \
  http://127.0.0.1:47832/athanor/rules
# read what it would do before letting it loose
curl -H "$K" -d '{"by":"liliang","dry_run":true}' \
  'http://127.0.0.1:47832/athanor/rules/chain@1:apply'
# in force
curl -H "$K" -d '{"by":"liliang"}' \
  'http://127.0.0.1:47832/athanor/rules/chain@1:publish'
# fired — an act with an author, over a scope
curl -H "$K" -d '{"by":"liliang","document":"orgchart"}' \
  'http://127.0.0.1:47832/athanor/rules/chain@1:apply'
# what the rules have actually derived, newest first
curl -H "$K" 'http://127.0.0.1:47832/athanor/rules/firings?limit=20'
```

Firing without a `by` is refused, exactly as approving a vocabulary is. A draft
fires only as a dry run; a retired rule does not fire at all, and everything it
derived stays exactly where it is, still naming the version that derived it —
`fact_provenance` says which rule, `inference_explain` names the premises under
the conclusion.

What a firing writes reaches the brain graded: `self_consistent`, because it
was derived deterministically from what was already stated and checked against
nothing in the world, produced `compiled`, naming the rule as its source, the
key as its author and the firing as the run that made it. Without that a rule's
output would be the one thing on the shelf `contract_tally` cannot count, and
an uncountable record is one nobody distrusts.

The rules are deliberately not written into CortexDB's own rule table. A rule
sitting enabled there can be fired through the brain's own door by any key with
write clearance — no `by`, no ledger entry, no firing recorded — which is the
thing this workflow exists to prevent. An application hands the engine the rule
as an ad-hoc definition instead: same engine, same derivation, same provenance
on the edges, and no second path that fires it without an author.

## The ledger

Five kinds of act reach one store, and one query answers all five.

```sh
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions?limit=20'
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions?kind=load'
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions?kind=review&actor=liliang'
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions?kind=rule.apply'
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions/decision:athanor:load:<job>:<load>'
```

A load records who put which job's graph into the brain, as which name, with
the report's counts. A review decision records who accepted, rejected or
edited which finding, with the note they wrote — recorded from an interceptor
placed after authorization, so nothing unauthorized is ever recorded. Every
ontology act is mirrored as `ontology.draft`, `ontology.propose`,
`ontology.approve`, `ontology.publish`, `ontology.retire`, and every rule act
as `rule.draft`, `rule.publish`, `rule.retire`, `rule.apply`; the tables in
`pkg/ontologies` and `pkg/rules` stay the source of truth for their workflows
and the ledger is the audit view. An agent's own `decision_record` over MCP or
gRPC has been landing in the same store since CortexDB v2.98.0, and shows up
beside them on the front page.

The actor is always the **key id** — the identity the policy knows and an
operator can revoke. A free-text `by` (alchemy's `ReviewDecision.by`, an
ontology approval's `by`) is a name nobody checked; it is kept verbatim in the
entry's note, so `_by` says which credential acted, the note says who claimed
to, and a disagreement between them is itself in the record.

Entry ids are deterministic in the act — `decision:athanor:load:<job>:<load>`,
`decision:athanor:review:<job>:<item>` — so re-running a load updates one entry
rather than growing a second, and a load can name the review decisions that
unblocked it as its own premises without a search. `GET
/athanor/decisions/{id}` is that chain, with each premise's grade and source.

A ledger write that fails never undoes the act it describes: a load that
succeeded answers 200 with `ledger_error` beside the report, and says so in
the log.

Reads take any key from the policy. A key confined to a `user_id` sees only
the entries it signed, and a chain whose root was signed by somebody else
answers exactly as one that does not exist — no status and no wording tells the
two apart. The pipeline itself is still unconfined: a job could be owned, but a
source id could not without alchemy's spool carrying an owner, and a
confinement with a hole in it reads as a guarantee.

## A database somebody else runs

A file is not the only thing an engagement has. The other thing is a production
database nobody is going to hand over — and reading one is not an import, it is
a decision: which tables, which columns, what gets masked before it leaves the
building, and whose name is on that. So it is a workflow, not a flag.

```sh
# 1. propose — reads the schema, classifies every column, drafts the treatment
curl -H "$K" -d '{"source":{"driver":"postgres","dsn":"postgres://ro:***@db.internal:5432/crm","schema":"public"}}' \
  http://127.0.0.1:47832/athanor/livedb/plans
# 2. read it, change what is wrong
curl -H "$K" -X PATCH -d '{"changes":[{"table":"customers","column":"city","action":"keep"}],"by":"liliang"}' \
  http://127.0.0.1:47832/athanor/livedb/plans/<id>
# 3. sign the hash you actually read
curl -H "$K" -d '{"hash":"<the hash the plan came back with>","by":"liliang"}' \
  http://127.0.0.1:47832/athanor/livedb/plans/<id>/signature
# 4. run it — dry first; the credential is supplied again and never stored
curl -H "$K" -d '{"plan":"<id>","dsn":"...","dry_run":true}' \
  http://127.0.0.1:47832/athanor/livedb/runs
# 5. keep up with it afterwards
curl -H "$K" -d '{"plan":"<id>","dsn":"..."}' http://127.0.0.1:47832/athanor/livedb/follows
```

The signature is over a hash of the plan, so signing something you did not read
is not a thing that can happen quietly: amend it and the hash moves, and the
old signature is refused by name. The credential is never kept — the plan holds
the redacted form — and it is supplied again at run time and checked against the
source the plan was signed for, so a plan signed over staging cannot be pointed
at production by pasting a different DSN. `ATHANOR_LIVEDB_HOSTS` is the blunt
second control: the hosts this Athanor will dial at all.

What a run writes arrives graded like everything else: `asserted`, because a
system of record said so and nobody checked it — the ladder measures whether
anybody checked, not how trustworthy a producer feels — with `_producer:
tabular`, `_source` naming the **plan** and never the DSN, `_by` the operator
and `_run` the run whose report says how many rows were read and what drifted.
Ask such a fact where it came from and the answer walks back through the run to
the signature to a person.

Masked values leave the building as tokens. Resolving one is its own permission
(`athanor.livedb.unmask`), separate from the permission to build the graph,
requires a written reason, and is a ledger entry whether or not it found
anything — including the reason, the caller and the count, never the value.

Three things it does not do:

- **A follow is at-least-once, not exactly-once.** It reports what it has seen
  and where it is; it does not promise a row is applied once.
- **Only what the publication carries.** A follow over a Postgres change stream
  refuses to start against a publication that would carry none of the tables the
  plan names, rather than reporting "running" over a database it cannot see.
- **The graph is not confined per row.** A restricted key cannot propose, sign
  or run, but the facts a run wrote are readable by anything that can read the
  brain.

## Time travel

CortexDB's graph has been bitemporal since v2.100.0: every node and edge
carries when it was true and when this store believed it, superseded rows move
into history rather than out of existence, and a read at an instant answers as
the shelf stood then. What it has no word for is *which* instants matter. A
timestamp somebody wrote down in a chat window is not an audit.

```sh
# name this moment
curl -H "$K" -d '{"name":"before-the-migration","by":"liliang","note":"sds@2 is in force"}' \
  http://127.0.0.1:47832/athanor/snapshots
# what has been named
curl -H "$K" http://127.0.0.1:47832/athanor/snapshots
# what changed between two of them
curl -H "$K" 'http://127.0.0.1:47832/athanor/snapshots/diff?from=before-the-migration&to=this-morning'
# retire a name
curl -H "$K" -d '{"by":"liliang","note":"superseded"}' \
  'http://127.0.0.1:47832/athanor/snapshots/before-the-migration:drop'
```

Nothing is copied. A snapshot row is a few kilobytes whatever the brain
weighs: the instant, what the graph counted then, the grade ladder at that
moment, and what it rested on — which ontology version was published, and the
loads that had landed. The graph as it stood is read back out of the brain's
own history at the stored instant, so a snapshot and a point-in-time read
cannot disagree. Taking one is an act like approving a vocabulary: no `by`, no
snapshot. It reaches the ledger as `snapshot.take` — `snapshot.drop` when
somebody retires a name — signed by the key id, with the typed name kept beside
it. Dropping is not a delete; it frees nothing, so all it can honestly mean is
"stop comparing against this", and the row keeps what was counted.

A diff answers in the product's own words: **added**, **withdrawn**,
**regraded**, **changed** — and the third one is the point. Storage says a row
"changed" whether a label gained a hyphen or a fact stopped being `verified`,
and those are not the same news. A grade change is its own kind here, and a
fall down the knowledge contract's ladder is counted again, because a fact that
quietly went from `verified` to `asserted` is invisible in every count of nodes
and is the thing an operator is looking for.

Four things it does not do, said here rather than discovered:

- **A snapshot is always of now.** The graph could answer for a past instant;
  `contract_tally` could not — it reads the live tables and ignores the
  as-of — so a backdated snapshot would carry today's grades under yesterday's
  date. The ladder is stored for the same reason: nothing can ask it again.
- **Only the graph.** Chunks, vectors, memories and the RDF triples carry no
  temporal columns, so a snapshot is a statement about the graph and says
  nothing about the rest of the brain.
- **A vacuum outruns it.** `vacuum_graph` deletes history closed before a
  cutoff. A snapshot older than the last vacuum still names a real moment and
  its stored counts are still true, but a diff reaching back past the cutoff
  reports less than happened.
- **No restore.** There is no verb that puts the brain back. Reading the past
  is a read; writing it back over the present is a different act with different
  consequences, and Athanor does not pretend to have thought about them yet.

## What is where

| | |
|---|---|
| `pkg/server` | the assembly: one policy over two services, and the load |
| `pkg/ontologies` | vocabulary versions and the signed acts on them |
| `pkg/rules` | declared rules, who put them in force, and every firing |
| `pkg/livedb` | plans over a live database, the signature, the run, the follow |
| `pkg/snapshots` | named moments, and the diff between two of them |
| `deploy/` | systemd, Docker, compose, example key file |
| `docs/superpowers/specs/` | the design, and why the two decisions were made |

Underneath: [CortexDB](https://github.com/liliang-cn/cortexdb) is the brain —
vectors, hybrid retrieval, memory, the RDF graph, the knowledge contract —
and [alchemy](https://github.com/liliang-cn/alchemy) is the pipeline — files
to a governed graph, with conflicts found and held. Athanor depends on both
and neither depends on it.

## Next

The four the spec names are done. What is left is not in this repository.

The rule engine is done: a rule is declared under a versioned id, one is in
force per lineage, retiring is not deleting, and firing one is a signed act
whose output reaches the brain graded. Snapshots are done, and the spec's
account of them needs one correction: it called the CortexDB half "storage
surgery, last", and that surgery had already landed by v2.100.0. Athanor's
half turned out to be the naming, the signature and the translation into the
knowledge contract's ladder — no storage work at all.

The decision ledger is done: loads, review decisions, ontology acts, rule acts,
live-database acts and snapshots are all entries. Row confinement is closed on
the ledger's own routes and on the snapshots, both for the same reason — an
entry and a moment each name their actor — and open on the pipeline, for the
reason `pkg/server/auth.go` gives.

What is still owed is upstream and narrow, and the snapshot section above names
it: a tally that can be asked of the past, temporal columns on anything but the
property graph, a vacuum that refuses to cut below the oldest kept snapshot,
and the question of whether reading a moment back should ever be allowed to
write over the present.

## License

MIT
