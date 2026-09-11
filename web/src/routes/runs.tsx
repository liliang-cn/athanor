import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { ChevronRight, Database, TriangleAlert } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Empty, LoadFailed } from "@/components/empty";
import { api } from "@/lib/api";
import type { Plan, RunReport } from "@/lib/types";

/**
 * What an import did, under the plan somebody signed for it.
 *
 * Runs are read per plan and never across plans, because the server reads
 * them that way for a reason it states: a run is only explicable under the
 * plan it ran with. So the plan is part of the question, and it lives in the
 * query string rather than in a piece of component state — a view somebody
 * arrived at is then a link they can send, which is the difference between an
 * operator saying "look at the drift" and an operator saying "click Import,
 * then runs, then pick the third one".
 *
 * The counts are the easy part. The part this screen exists for is `drift`:
 * columns the database has that the signed plan does not name. They were
 * dropped from the import — nothing personal leaked — but a column arriving
 * unnoticed is exactly the event a signature is supposed to cover, so it is
 * said in a sentence and not only as a list of names a reader has to
 * interpret.
 */

/** The plans listing, which is how a plan gets chosen when none is named. */
interface PlansAnswer {
  plans: Plan[];
}
interface RunsAnswer {
  runs: RunReport[];
}

/** A time the reader can compare with their own watch. */
function when(iso?: string): string {
  if (!iso) return "—";
  const at = new Date(iso);
  if (Number.isNaN(at.getTime()) || at.getUTCFullYear() < 1971) return "—";
  return at.toLocaleString();
}

/** How long it took, for a reader deciding whether to start another. */
function took(from: string, to: string): string | null {
  const a = new Date(from).getTime();
  const b = new Date(to).getTime();
  if (!Number.isFinite(a) || !Number.isFinite(b) || b < a || new Date(to).getUTCFullYear() < 1971) {
    return null;
  }
  const ms = b - a;
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 60) return `${minutes} min ${Math.round((ms % 60_000) / 1000)} s`;
  return `${Math.floor(minutes / 60)} h ${minutes % 60} min`;
}

function plural(n: number, one: string, many: string): string {
  return n === 1 ? one : many;
}

/** One number and what it counts. Two across on a phone, four on a desk. */
function Count({ label, value, quiet }: { label: string; value: number; quiet?: boolean }) {
  return (
    <div className="min-w-0 rounded-md border p-2.5">
      <div className={`text-lg leading-none font-medium tabular-nums ${quiet ? "text-muted-foreground" : ""}`}>
        {value.toLocaleString()}
      </div>
      <div className="text-muted-foreground mt-1 text-xs">{label}</div>
    </div>
  );
}

/** The columns nobody signed for, said in words first. */
function Drift({ drift, gone }: { drift?: string[]; gone?: string[] }) {
  const d = drift?.length ?? 0;
  const g = gone?.length ?? 0;
  if (!d && !g) return null;
  return (
    <div className="border-held/40 bg-held/5 mt-3 rounded-md border p-3">
      <div className="text-held flex items-start gap-2">
        <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
        <div className="min-w-0 space-y-1.5 text-sm text-pretty">
          {d > 0 && (
            <p>
              The database has {d} {plural(d, "column", "columns")} the signed plan does not name.{" "}
              {plural(d, "It was", "They were")} dropped from this import rather than imported unread, and a
              re-signing is owed before {plural(d, "it", "they")} may enter the brain.
            </p>
          )}
          {g > 0 && (
            <p>
              The plan names {g} {plural(g, "column", "columns")} the database no longer{" "}
              {plural(g, "has", "have")}. Nothing was read for {plural(g, "it", "them")}.
            </p>
          )}
        </div>
      </div>
      {d > 0 && <ColumnList label="Gained since the signature" names={drift!} />}
      {g > 0 && <ColumnList label="In the plan, gone from the database" names={gone!} />}
    </div>
  );
}

function ColumnList({ label, names }: { label: string; names: string[] }) {
  return (
    <div className="mt-2.5">
      <p className="text-muted-foreground text-xs">{label}</p>
      <ul className="mt-1 flex flex-wrap gap-1.5">
        {names.map((name) => (
          <li key={name}>
            <Badge variant="outline" className="max-w-full font-mono text-xs break-words whitespace-normal">
              {name}
            </Badge>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** One run. A card at every width: the fields are a paragraph's worth and a
 *  table of them would be a table dragged sideways on a phone. */
function Run({ run }: { run: RunReport }) {
  const duration = took(run.started_at, run.ended_at);
  return (
    <div className="rounded-lg border p-3 sm:p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="min-w-0 font-mono text-xs break-all">{run.id}</span>
        {run.dry_run && (
          <Badge variant="outline" className="text-xs">
            dry run — nothing was written
          </Badge>
        )}
        {!!run.drift?.length && (
          <Badge variant="outline" className="text-held text-xs">
            drift
          </Badge>
        )}
      </div>

      <p className="text-muted-foreground mt-1.5 text-sm text-pretty">
        Started {when(run.started_at)} · ended {when(run.ended_at)}
        {duration && ` · took ${duration}`}
      </p>

      <div className="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-4">
        <Count label="rows read" value={run.rows_read} />
        <Count label="chunks" value={run.chunks} />
        <Count label="triples" value={run.triples} />
        <Count label="skipped" value={run.skipped} quiet={run.skipped === 0} />
      </div>

      <Drift drift={run.drift} gone={run.gone} />

      {!!run.errors?.length && (
        <div className="border-destructive/40 bg-destructive/5 mt-3 rounded-md border p-3">
          <p className="text-destructive text-sm text-pretty">
            {run.errors.length} {plural(run.errors.length, "row", "rows")} failed and{" "}
            {plural(run.errors.length, "was", "were")} collected rather than stopping the import.
          </p>
          <ul className="text-destructive/90 mt-1.5 space-y-1 font-mono text-xs break-words">
            {run.errors.map((e, i) => (
              <li key={`${i}-${e}`}>{e}</li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

export function Runs() {
  const [params, setParams] = useSearchParams();
  const chosen = params.get("plan") ?? "";

  const [plans, setPlans] = useState<Plan[] | null>(null);
  const [plansError, setPlansError] = useState<string | null>(null);
  const [runs, setRuns] = useState<RunReport[] | null>(null);
  const [runsError, setRunsError] = useState<string | null>(null);

  useEffect(() => {
    api
      .get<PlansAnswer>("/athanor/livedb/plans")
      .then((a) => setPlans(a.plans ?? []))
      .catch((e: Error) => setPlansError(e.message));
  }, []);

  useEffect(() => {
    if (!chosen) {
      setRuns(null);
      setRunsError(null);
      return;
    }
    let current = true;
    setRuns(null);
    setRunsError(null);
    api
      .get<RunsAnswer>(`/athanor/livedb/runs?plan=${encodeURIComponent(chosen)}`)
      .then((a) => current && setRuns(a.runs ?? []))
      .catch((e: Error) => current && setRunsError(e.message));
    return () => {
      current = false;
    };
  }, [chosen]);

  // The server answers runs newest first and this sorts them again, because
  // the promise is the screen's and not the route's: a reader scanning for
  // the last import should never have to check.
  const newestFirst = runs
    ? [...runs].sort((a, b) => new Date(b.started_at).getTime() - new Date(a.started_at).getTime())
    : null;
  const plan = plans?.find((p) => p.id === chosen);
  const drifting = newestFirst?.filter((r) => r.drift?.length).length ?? 0;

  return (
    <div className="space-y-6">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Imports</h1>
        <p className="text-muted-foreground mt-1 text-sm text-pretty">
          What each pass over a live database read, and what it left behind. A run is read under the plan
          it ran with, because that plan is the only thing that explains it.
        </p>
      </header>

      {plansError && <LoadFailed what="The plans" error={plansError} />}

      {chosen ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Under this plan</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <p className="font-mono text-xs break-all">{chosen}</p>
            {plan && (
              <div className="flex flex-wrap items-center gap-2">
                <Badge variant="outline" className="text-xs">
                  {plan.state}
                </Badge>
                {plan.signed_by && (
                  <span className="text-muted-foreground text-xs">
                    signed by {plan.signed_by} · {when(plan.signed_at)}
                  </span>
                )}
              </div>
            )}
            {plan?.source.redacted && (
              <p className="text-muted-foreground flex items-start gap-2 font-mono text-xs break-all">
                <Database className="mt-0.5 size-3.5 shrink-0" aria-hidden />
                {plan.source.redacted}
              </p>
            )}
            <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-center">
              {plans && plans.length > 1 && (
                <Select value={chosen} onValueChange={(id) => setParams({ plan: id })}>
                  <SelectTrigger className="w-full min-w-0 sm:w-72" aria-label="Choose a plan">
                    <SelectValue placeholder="Choose a plan" />
                  </SelectTrigger>
                  <SelectContent>
                    {plans.map((p) => (
                      <SelectItem key={p.id} value={p.id} className="font-mono text-xs">
                        {p.source.redacted || p.source_key || p.id}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              {/* Always reachable, and not only when there is a second plan to
                  switch to: a screen a link dropped somebody into needs a way
                  back to the question it answered. */}
              <Button variant="ghost" size="sm" onClick={() => setParams({})} className="self-start">
                All plans
              </Button>
            </div>
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Choose a plan</CardTitle>
          </CardHeader>
          <CardContent>
            {!plans && !plansError && <Skeleton className="h-24 w-full" />}
            {plans && !plans.length && (
              <Empty>
                No live-database plan has been proposed on this server yet, so nothing has been imported
                from one.
              </Empty>
            )}
            <div className="space-y-2">
              {plans?.map((p) => (
                <Link
                  key={p.id}
                  to={`/import/runs?plan=${encodeURIComponent(p.id)}`}
                  className="hover:bg-accent flex items-center gap-3 rounded-lg border p-3 transition-colors"
                >
                  <div className="min-w-0 flex-1">
                    <p className="font-mono text-xs break-all">
                      {p.source.redacted || p.source_key || p.id}
                    </p>
                    <p className="text-muted-foreground mt-1 text-xs break-all">{p.id}</p>
                    <div className="mt-1.5 flex flex-wrap items-center gap-2">
                      <Badge variant="outline" className="text-xs">
                        {p.state}
                      </Badge>
                      <span className="text-muted-foreground text-xs tabular-nums">
                        {p.counts.columns} columns · {p.counts.personal} personal
                      </span>
                    </div>
                  </div>
                  <ChevronRight className="text-muted-foreground size-4 shrink-0" aria-hidden />
                </Link>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {chosen && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Runs, newest first</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {runsError && <LoadFailed what="This plan's runs" error={runsError} />}
            {!newestFirst && !runsError && <Skeleton className="h-32 w-full" />}
            {newestFirst && !newestFirst.length && (
              <Empty>
                Nothing has been imported under this plan. A plan that is signed but never run has read
                nothing — and a plan id this server does not know reads the same way here, so check the id
                if you expected runs.
              </Empty>
            )}
            {drifting > 0 && (
              <p className="text-held text-sm text-pretty">
                {drifting} of these {plural(drifting, "run", "runs")} met a column the signature does not
                cover. A re-signing is owed.
              </p>
            )}
            {newestFirst?.map((r) => <Run key={r.id} run={r} />)}
          </CardContent>
        </Card>
      )}
    </div>
  );
}
