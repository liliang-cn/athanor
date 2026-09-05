# Athanor — skeleton design

*2026-09-05. Sub-project 1 of 4. Approved in conversation; this is the record.*

## What it is

Athanor is the product: CortexDB's brain and alchemy's pipeline behind one
door. It is a third repository because the two underneath each state, in their
own design documents, that they are not the product — CortexDB is a library
("one file, no service to run"), alchemy is a service that "returns and forgets"
— and the thing that composes them has to be neither.

It exists to be sold and deployed inside a customer's network, where the two
claims that matter are *the data does not leave* and *every answer can be
traced and distrusted*. The open-source story grows out of that; it is not
the other way round.

## What it is not

- Not a merge of the two repositories. The dependency runs `athanor → CortexDB`
  and `athanor → alchemy`, the same direction `alchemy/connectors → CortexDB`
  already runs, and the only one that does not make a cycle. CortexDB never
  imports alchemy: it would pull LLM-shaped dependencies into `pkg/`, which its
  own CLAUDE.md forbids, and would invert both projects' identities.
- Not a rename. The umbrella name is Athanor's; CortexDB and alchemy keep
  theirs. Their module paths, published clients and plugin manifests are load-
  bearing and renaming is not reversible.
- Not a second implementation of anything. Every component is a public
  constructor from the project that owns it. This package's own work is the
  order they are called in and two decisions combining them forced.

## Architecture

One process, two listeners.

```
athanor serve
 ├─ *cortexdb.DB                one brain: SQLite file or postgres:// DSN
 ├─ alchemy service             jobs / review / ontology, in-process, in-memory job store
 └─ one grpc.Server             alchemy.v1 + cortexdb.v1 on one port
     └─ one http.Server         /v1 /ui   alchemy's REST gateway + review UI
                                /brain/   CortexDB's REST
                                /graph/   the live 3D graph and the ontology page
                                /athanor/loads   the one verb Athanor adds
                                /metrics /debug/vars /healthz
                                /         the front page
```

Existing CortexDB clients — the MCP server in remote mode, the Rust/Python/
Node clients — set `CORTEXDB_REMOTE` to the gRPC port and need no change,
because `cortexdb.v1` is mounted unchanged.

alchemy's REST gateway dials Athanor's own gRPC front door rather than
holding the service, which is alchemy's rule kept: a gateway that holds the
service can call what no RPC exposes and skip the interceptors. Every REST
call is therefore a translation of a gRPC call and authorized identically.

### Decision 1 — one policy over two services

alchemy checks a single bearer token and refuses to be built without one.
CortexDB checks a scoped key (clearance, row confinement, per-tool
classification, SPARQL narrowing) and passes the caller's key to handlers
that check ownership. A `grpc.Server` has one interceptor chain.

Athanor's interceptor authenticates every call against the scoped-key policy
and routes by service. A `cortexdb.v1` call goes to CortexDB's own interceptor
(`rpcserver.AuthInterceptor`, exported for this in v2.95.0). An `alchemy.v1`
call is classified by an explicit table in `methods.go` — read or write, with
a test that walks the registered service and fails on any method missing from
it — authorized against the caller's clearance, and handed to alchemy's
interceptor carrying a token minted at startup that never leaves the process.
alchemy's invariant holds; the only credentials a caller can present are the
ones in the key file. Streams follow the same routing; a stream that is not
alchemy's is refused, because CortexDB has none and the safe direction for one
that appears is denial.

Not done here: row confinement on the pipeline. A key confined to one
`user_id` has nothing on `CreateJob` to be confined by. The decision ledger
(sub-project 2) is where that closes.

### Decision 2 — the brain receives finished graphs by an explicit act

A job that finishes is not in the brain. `POST /athanor/loads {job, load}`
asks alchemy's own `GetResult` — which refuses a held or running job — and
drives the result through `sink.Load` into `connectors/cortexdb` on the same
`*cortexdb.DB` the process serves. It is a write; a read-only key is refused
before anything is fetched. The brain therefore holds only graphs that were
finished, and finished means reviewed when review was owed.

This is the first kind of entry the decision ledger will record.

## Data flow

files → `/v1/sources` → `/v1/jobs` (ontology + model endpoints) → *held* →
`/ui` (signed decision) → finished → `/athanor/loads` → brain, graded by the
knowledge contract → `contract_tally`, `fact_provenance`, MCP, REST.

## Front page

Server-rendered, embedded, no build step — what both projects already chose.
Shows the contract tally, what wants a person, and the load form. Signs in
with a key from the policy carried in an HttpOnly SameSite=Strict cookie,
because a browser cannot send a bearer header on its own. alchemy's `/ui` has
a sign-in of its own; unifying them is a follow-up.

## Upstream changes this required

- CortexDB v2.94.0: `liveview.New` + `Handler()` — the live graph as routes
  a process mounts, instead of a listener of its own.
- CortexDB v2.95.0: `rpcserver.AuthInterceptor` exported.
- CortexDB v2.96.0: `liveview.SourceFor(db, describe)` — a view over a brain
  the caller already holds open.

## Testing

`pkg/server` is tested against a real bound server with a fake pipeline
(`service.Runner` returning a canned result), so nothing needs a model:
every pipeline RPC is classified; a read-only key may look and not change; no
key is refused before either service; the brain keeps its own policy on the
shared listener; the internal token is useless from outside; a finished job
loads and lands `asserted`; a held job is refused and nothing leaks; a
reader cannot load. The provenance-chain demo in alchemy is the end-to-end
run with a real model, and Athanor is verified by running it through the
product's own doors.

## Deploy

`deploy/systemd` (hardened unit, same shape as CortexDB's, verified on
Ubuntu 24.04), `deploy/docker` (static image whose healthcheck is the binary
itself, compose with every port overridable). Ports default to 47831/47832.

## Sub-projects after this one

2. Decision ledger — primitive in CortexDB, chain/precedents in Athanor.
3. General rule engine (declared IF-THEN over `apply_inference`) + ontology
   propose → approve → publish over alchemy's `Extend`.
4. Time travel — bitemporal columns in CortexDB, snapshots and diff in
   Athanor. Storage surgery, last.
