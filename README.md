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

Decision ledger; a general rule engine; point-in-time snapshots. In that
order — see the spec. Ontology propose → approve → publish is done, and its
acts are shaped to become the ledger's second kind of entry.

## License

MIT
