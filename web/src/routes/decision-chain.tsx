import { useEffect, useState } from "react";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { ArrowLeft, ChevronRight, CornerDownRight } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { GradeBadge } from "@/components/grade-badge";
import { Empty, LoadFailed } from "@/components/empty";
import { ApiError, api } from "@/lib/api";
import type { DecisionRecord } from "@/lib/types";
import { decisionPath } from "@/routes/decisions";

/**
 * One decision, and what it rests on.
 *
 * The chain is the point of the ledger. An entry on its own says a thing was
 * done; the chain says why it was allowed to be — a run walks back to the
 * signature that permitted it, and that signature walks back to the column it
 * was made of. So the premises get as much of this page as the entry does, and
 * every one of them that is itself a decision is a link to its own chain.
 *
 * The premises are also where the grades are. The decision itself is
 * `verified` by construction, because a key the policy knows signed it, and
 * saying so on every page would be noise. What a premise was graded is not
 * noise: a decision resting on something `asserted` is a decision resting on
 * something nobody checked, and a chain that listed its premises without
 * saying so would look like evidence and not be.
 *
 * # The note
 *
 * A ledger note is two lines (pkg/server/ledger.go): a sentence, then the
 * structured detail as JSON. The detail is rendered as labelled fields,
 * because a person reading why a database was read should not have to parse
 * braces to find the row count. If the second line is not an object — an older
 * entry, or a note somebody wrote by hand through decision_record — it is
 * shown verbatim in a `pre` rather than dropped. Dropping it would be the one
 * unforgivable thing a ledger viewer can do: quietly show less than the record
 * holds.
 *
 * # The id in the path
 *
 * The route is a splat, because a decision id carries colons and a load is
 * named by whoever ran it, so it can carry anything. The path is read off the
 * location rather than out of `useParams`, and handed to the door still
 * percent-encoded, so nothing in this file has to guess how many times the
 * router already decoded it.
 */

interface DecisionPremise {
  id: string;
  edge?: boolean;
  type?: string;
  content?: string;
  from?: string;
  to?: string;
  /** What makes the chain recurse. */
  decision?: boolean;
  grade?: string;
  source?: string;
  /** Recorded once, and since deleted: the shelf changed under a decision that
   *  still stands. */
  missing?: boolean;
}

/** DecisionRecord as the chain route returns it. The extra fields are the
 *  contract's own keys plus the two links a chain is walked along. */
interface ChainRecord extends DecisionRecord {
  grade?: string;
  source?: string;
  producer?: string;
  premises?: DecisionPremise[];
  supersedes?: string[];
}

interface Chain {
  root: string;
  decisions: ChainRecord[];
  depth: number;
  truncated?: boolean;
}

/** How far to walk when a reader asks for more than the default. CortexDB's
 *  own ceiling; asking for more is refused there, so it is not offered here. */
const DEEP = 32;

function splitNote(note?: string): { lead: string; detail?: Record<string, unknown>; raw?: string } {
  const text = (note ?? "").trim();
  if (!text) return { lead: "" };
  const nl = text.indexOf("\n");
  if (nl < 0) return { lead: text };
  const lead = text.slice(0, nl).trim();
  const rest = text.slice(nl + 1).trim();
  if (!rest) return { lead };
  try {
    const parsed: unknown = JSON.parse(rest);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      return { lead, detail: parsed as Record<string, unknown> };
    }
  } catch {
    // Not an object. Shown as it was written; see the file comment.
  }
  return { lead, raw: rest };
}

function when(at?: string): string {
  if (!at) return "";
  const d = new Date(at);
  return Number.isNaN(d.getTime()) ? at : d.toLocaleString();
}

function fieldValue(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return JSON.stringify(v);
}

/** Undo the encoding the path carries, for display only. An id that was never
 *  encoded decodes to itself, and one this fails on is shown as it arrived. */
function readable(path: string): string {
  try {
    return decodeURIComponent(path);
  } catch {
    return path;
  }
}

/** What a premise is, in words, when it is not a decision. */
function premiseLabel(p: DecisionPremise): string {
  if (p.edge) return `${p.from ?? "?"} —${p.type ?? "?"}→ ${p.to ?? "?"}`;
  return p.content || p.id;
}

export function DecisionChain() {
  const location = useLocation();
  const [params, setParams] = useSearchParams();
  const [chain, setChain] = useState<Chain | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [missing, setMissing] = useState(false);

  // Still encoded: exactly what the browser holds, and exactly what the door
  // takes.
  const encoded = location.pathname.replace(/^\/decisions\/?/, "");
  const id = readable(encoded);
  const depth = params.get("depth") ?? "";

  useEffect(() => {
    let live = true;
    setChain(null);
    setError(null);
    setMissing(false);
    if (!encoded) {
      setMissing(true);
      return;
    }
    api
      .get<{ chain: Chain }>(`/athanor/decisions/${encoded}${depth ? `?depth=${depth}` : ""}`)
      .then((r) => live && setChain(r.chain))
      .catch((e: Error) => {
        if (!live) return;
        if (e instanceof ApiError && e.isNotFound) setMissing(true);
        else setError(e.message);
      });
    return () => {
      live = false;
    };
  }, [encoded, depth]);

  const back = (
    <Link
      to="/decisions"
      className="text-muted-foreground hover:text-foreground inline-flex items-center gap-1.5 text-sm transition-colors"
    >
      <ArrowLeft className="size-4" aria-hidden /> Decisions
    </Link>
  );

  if (missing) {
    return (
      <div className="space-y-6">
        {back}
        <Alert variant="destructive">
          <AlertTitle>Nothing carries that id.</AlertTitle>
          <AlertDescription>
            <p>
              Either no entry was ever written under it, or it belongs to somebody this key may not
              read. The ledger answers both the same way on purpose: an id space that answered them
              differently would tell a caller what exists.
            </p>
            <p className="font-mono text-xs break-all">{id}</p>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-6">
        {back}
        <LoadFailed what="This decision" error={error} />
      </div>
    );
  }

  if (!chain) {
    return (
      <div className="space-y-6">
        {back}
        <Skeleton className="h-8 w-2/3" />
        <Skeleton className="h-56 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  const root = chain.decisions[0];
  if (!root) {
    return (
      <div className="space-y-6">
        {back}
        <Empty>The chain came back with no entry in it.</Empty>
      </div>
    );
  }

  const { lead, detail, raw } = splitNote(root.note);
  const premises = root.premises ?? [];
  const rested = chain.decisions.slice(1);

  return (
    <div className="space-y-6">
      {back}

      <header>
        <h1 className="text-xl font-semibold tracking-tight text-pretty break-words">
          {lead || root.verdict || root.id}
        </h1>
        <p className="text-muted-foreground mt-1.5 font-mono text-xs break-all">{chain.root}</p>
      </header>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">The entry</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant="secondary" className="font-mono text-xs">
              {root.verdict || "—"}
            </Badge>
            {root.kind && (
              <Link
                to={`/decisions?kind=${encodeURIComponent(root.kind)}`}
                className="text-muted-foreground hover:text-foreground font-mono text-xs break-words underline-offset-4 transition-colors hover:underline"
              >
                {root.kind}
              </Link>
            )}
          </div>

          <dl className="grid gap-x-6 gap-y-3 sm:grid-cols-2">
            <Field label="Actor">
              <Link
                to={`/decisions?actor=${encodeURIComponent(root.actor)}`}
                className="font-mono text-xs break-all underline-offset-4 hover:underline"
              >
                {root.actor || "nobody"}
              </Link>
            </Field>
            <Field label="When">
              <time dateTime={root.at} className="text-xs tabular-nums">
                {when(root.at)}
              </time>
            </Field>
            <Field label="About">
              {root.subject ? (
                <Link
                  to={`/decisions?subject=${encodeURIComponent(root.subject)}`}
                  className="font-mono text-xs break-all underline-offset-4 hover:underline"
                >
                  {root.subject}
                </Link>
              ) : (
                <span className="text-muted-foreground text-xs text-pretty">
                  nothing on the shelf — what it acted on is in the id and the detail below
                </span>
              )}
            </Field>
            {root.supersedes && root.supersedes.length > 0 && (
              <Field label="Replaces">
                <div className="space-y-1">
                  {root.supersedes.map((s) => (
                    <Link
                      key={s}
                      to={decisionPath(s)}
                      className="block font-mono text-xs break-all underline-offset-4 hover:underline"
                    >
                      {s}
                    </Link>
                  ))}
                </div>
              </Field>
            )}
          </dl>

          {(detail || raw) && <Separator />}

          {detail && (
            <dl className="grid gap-x-6 gap-y-3 sm:grid-cols-2">
              {Object.entries(detail).map(([k, v]) => (
                <Field key={k} label={k.replace(/_/g, " ")}>
                  <span className="font-mono text-xs break-all">{fieldValue(v)}</span>
                </Field>
              ))}
            </dl>
          )}

          {raw && (
            <div className="space-y-1.5">
              <p className="text-muted-foreground text-xs">
                The note's second line, which is not an object. Shown as it was written.
              </p>
              <pre className="bg-muted overflow-x-auto rounded-md p-3 font-mono text-xs break-words whitespace-pre-wrap">
                {raw}
              </pre>
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">What it rests on</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          {!premises.length && (
            <Empty>
              Nothing. This is where the ledger begins — the act rested on no recorded premise.
            </Empty>
          )}
          {premises.map((p) => {
            const body = (
              <>
                <div className="flex flex-wrap items-center gap-2">
                  {p.grade ? (
                    <GradeBadge grade={p.grade} />
                  ) : (
                    <Badge
                      variant="outline"
                      className="text-untagged font-mono text-xs"
                      title="the record carries no contract key at all, which is a different answer from being graded untagged"
                    >
                      ungraded
                    </Badge>
                  )}
                  {p.type && (
                    <span className="text-muted-foreground font-mono text-xs break-words">
                      {p.type}
                    </span>
                  )}
                  {p.decision && (
                    <span className="text-muted-foreground text-xs">a decision of its own</span>
                  )}
                  {p.missing && (
                    <Badge variant="destructive" className="text-xs">
                      deleted since
                    </Badge>
                  )}
                </div>
                <p className="mt-1.5 text-sm break-words text-pretty">
                  {splitNote(premiseLabel(p)).lead || p.id}
                </p>
                <p className="text-muted-foreground mt-1 font-mono text-xs break-all">{p.id}</p>
                {p.source && (
                  <p className="text-muted-foreground mt-1 font-mono text-xs break-all">
                    {p.source}
                  </p>
                )}
              </>
            );
            return p.decision ? (
              <Link
                key={p.id}
                to={decisionPath(p.id)}
                className="hover:bg-accent/50 focus-visible:ring-ring block rounded-lg border p-3 transition-colors focus-visible:ring-2 focus-visible:outline-none"
              >
                <div className="flex items-start gap-3">
                  <div className="min-w-0 flex-1">{body}</div>
                  <ChevronRight
                    className="text-muted-foreground mt-0.5 size-4 shrink-0"
                    aria-hidden
                  />
                </div>
              </Link>
            ) : (
              <div key={p.id} className="rounded-lg border p-3">
                {body}
              </div>
            );
          })}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Walked back</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          <p className="text-muted-foreground text-xs">
            Every decision reachable from this one, nearest first — {chain.depth}{" "}
            {chain.depth === 1 ? "hop" : "hops"}.
          </p>
          {!rested.length && (
            <Empty>No decision stands behind this one. It is the beginning of its chain.</Empty>
          )}
          {rested.map((d) => (
            <Link
              key={d.id}
              to={decisionPath(d.id)}
              className="hover:bg-accent/50 focus-visible:ring-ring block rounded-lg border p-3 transition-colors focus-visible:ring-2 focus-visible:outline-none"
            >
              <div className="flex items-start gap-3">
                <CornerDownRight
                  className="text-muted-foreground mt-0.5 size-4 shrink-0"
                  aria-hidden
                />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <Badge variant="secondary" className="font-mono text-xs">
                      {d.verdict || "—"}
                    </Badge>
                    <span className="text-muted-foreground font-mono text-xs break-words">
                      {d.kind || "untyped"}
                    </span>
                  </div>
                  <p className="mt-1.5 text-sm break-words text-pretty">
                    {splitNote(d.note).lead || <span className="text-muted-foreground">no note</span>}
                  </p>
                  <div className="text-muted-foreground mt-1.5 flex flex-col gap-0.5 text-xs sm:flex-row sm:flex-wrap sm:gap-x-3">
                    <span className="break-words">
                      by <span className="font-mono">{d.actor || "nobody"}</span>
                    </span>
                    <span className="tabular-nums">
                      <time dateTime={d.at}>{when(d.at)}</time>
                    </span>
                  </div>
                  <p className="text-muted-foreground mt-1 font-mono text-xs break-all">{d.id}</p>
                </div>
                <ChevronRight
                  className="text-muted-foreground mt-0.5 size-4 shrink-0"
                  aria-hidden
                />
              </div>
            </Link>
          ))}

          {chain.truncated && (
            <Alert>
              <AlertTitle>The walk stopped before the chain did.</AlertTitle>
              <AlertDescription>
                <p>
                  There are decisions behind this one that the depth bound did not reach. A chain
                  that quietly stopped reads as a complete account of why something was done.
                </p>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    const next = new URLSearchParams(params);
                    next.set("depth", String(DEEP));
                    setParams(next);
                  }}
                >
                  Walk further back
                </Button>
              </AlertDescription>
            </Alert>
          )}
          {depth && !chain.truncated && (
            <p className="text-muted-foreground text-xs">
              Walked to a depth of {depth}; the chain ended before it.
            </p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

/** One labelled field. Below `sm` the grid is one column, so a row of these is
 *  a stack — which is what keeps a long id from pushing a phone sideways. */
function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-muted-foreground text-xs">{label}</dt>
      <dd className="mt-0.5 min-w-0 break-words">{children}</dd>
    </div>
  );
}
