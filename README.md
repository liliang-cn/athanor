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

## What is where

| | |
|---|---|
| `pkg/server` | the assembly: one policy over two services, and the load |
| `deploy/` | systemd, Docker, compose, example key file |
| `docs/superpowers/specs/` | the design, and why the two decisions were made |

Underneath: [CortexDB](https://github.com/liliang-cn/cortexdb) is the brain —
vectors, hybrid retrieval, memory, the RDF graph, the knowledge contract —
and [alchemy](https://github.com/liliang-cn/alchemy) is the pipeline — files
to a governed graph, with conflicts found and held. Athanor depends on both
and neither depends on it.

## Next

Decision ledger; a general rule engine and ontology propose → approve →
publish; point-in-time snapshots. In that order — see the spec.

## License

MIT
