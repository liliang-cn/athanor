# Athanor

CortexDB's brain and alchemy's pipeline behind one door: files go in under a
vocabulary, a disagreement stops the job, a person answers it, and the answer
survives into a store that can still say how it knows.

Every record in the brain carries which file, which chunk and which producer
it came from, whether a source stated it or a model inferred it, and a grade:
`verified` when a named person kept it, `refused` when the vocabulary declined
it, `asserted` when a model said so and nobody checked. Ask the store what its
shelf stands on and it answers with numbers, including the ones that are bad.

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
the binary itself.

Existing CortexDB clients point `CORTEXDB_REMOTE` at the gRPC port and need no
change. Every door reads one key file — see `deploy/keys.example.json`.

## The chain, over the doors

```sh
K='Authorization: Bearer <operator key>'
# 1. a file, under a vocabulary
curl -H "$K" -H 'Content-Type: text/markdown' --data-binary @runbook.md \
  'http://127.0.0.1:47832/v1/sources?name=runbook.md&kind=SOURCE_KIND_DOCUMENT'
curl -H "$K" -d '{"source_ids":["<id>"],"ontology":"<json>","part":"prose","models":{"llm":{"name":"…","base_url":"…"}}}' \
  http://127.0.0.1:47832/v1/jobs
# 2. held — two sources disagree — review it
open http://127.0.0.1:47832/ui/
# 3. finished → into the brain, graded
curl -H "$K" -d '{"job":"<id>"}' http://127.0.0.1:47832/athanor/loads
# 4. ask the brain how it knows
curl -H "$K" -d '{}' http://127.0.0.1:47832/brain/v1/tools/contract_tally
```

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

## The ledger

Four kinds of act reach one store, and one query answers all four.

```sh
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions?limit=20'
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions?kind=load'
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions?kind=review&actor=liliang'
curl -H "$K" 'http://127.0.0.1:47832/athanor/decisions/decision:athanor:load:<job>:<load>'
```

A load records who put which job's graph into the brain, as which name, with
the report's counts. A review decision records who accepted, rejected or
edited which finding, with the note they wrote — recorded from an interceptor
placed after authorization, so nothing unauthorized is ever recorded. Every
ontology act is mirrored as `ontology.draft`, `ontology.propose`,
`ontology.approve`, `ontology.publish`, `ontology.retire`; the two tables in
`pkg/ontologies` stay the source of truth for the workflow and the ledger is
the audit view. An agent's own `decision_record` over MCP or gRPC has been
landing in the same store since CortexDB v2.98.0, and shows up beside them on
the front page.

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

## What is where

| | |
|---|---|
| `pkg/server` | the assembly: one policy over two services, and the load |
| `pkg/ontologies` | vocabulary versions and the signed acts on them |
| `deploy/` | systemd, Docker, compose, example key file |
| `docs/superpowers/specs/` | the design, and why the two decisions were made |

Underneath: [CortexDB](https://github.com/liliang-cn/cortexdb) is the brain —
vectors, hybrid retrieval, memory, the RDF graph, the knowledge contract —
and [alchemy](https://github.com/liliang-cn/alchemy) is the pipeline — files
to a governed graph, with conflicts found and held. Athanor depends on both
and neither depends on it.

## Next

A general rule engine; point-in-time snapshots. In that order — see the spec.
The decision ledger is done: loads, review decisions and ontology acts are all
entries, and row confinement is closed on the ledger's own routes and open on
the pipeline, for the reason `pkg/server/auth.go` gives.

## License

MIT
