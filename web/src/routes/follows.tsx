import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { Database, KeyRound, Loader2, Play, RefreshCw, Square, TriangleAlert } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Empty, LoadFailed } from "@/components/empty";
import { api, ApiError } from "@/lib/api";
import { useSession } from "@/lib/session";
import type { Follow, Plan } from "@/lib/types";

/**
 * What is keeping the brain in step with a database somebody else runs.
 *
 * A follow is not a request: the server owns it, it outlives the response
 * that started it, and it ends when it is deleted, when the server shuts
 * down, or when the change stream fails. The last case is why a stopped
 * follow stays on this screen with its reason — an operator has to be able to
 * learn why the brain stopped keeping up, rather than find the entry simply
 * gone and conclude somebody stopped it on purpose.
 *
 * # The credential
 *
 * Starting one needs the live connection string, and this screen is the only
 * place in the product that holds it. It lives in React state for the length
 * of the form and goes out in the POST body; it is never rendered back, never
 * put in the URL or in history (hence a form that posts rather than a link),
 * and it is scrubbed out of any message shown here before that message
 * reaches the screen — the server scrubs its own errors already, and this is
 * the second wall, because the cost of one leak is a password in a
 * screenshot. What names a database on this screen is the redacted string the
 * plan already carries.
 */

interface FollowsAnswer {
  follows: Follow[];
}
interface PlansAnswer {
  plans: Plan[];
}
/** POST answers 202 with the follow and the sentence about what 202 means. */
interface StartedAnswer {
  follow: Follow;
  note?: string;
}

function when(iso?: string): string {
  if (!iso) return "—";
  const at = new Date(iso);
  if (Number.isNaN(at.getTime()) || at.getUTCFullYear() < 1971) return "—";
  return at.toLocaleString();
}

/**
 * The second wall around the credential.
 *
 * The server scrubs the DSN out of its own messages; this removes it again on
 * the way to the screen, including from an error this browser produced
 * locally — a fetch failure quoting the request, say, which the server never
 * saw and therefore never scrubbed.
 */
function scrub(message: string, dsn: string): string {
  const secret = dsn.trim();
  if (!secret) return message;
  let out = message.split(secret).join("«credential»");
  const at = secret.lastIndexOf("@");
  const colon = at > 0 ? secret.indexOf(":", secret.indexOf("//") + 2) : -1;
  if (at > 0 && colon > 0 && colon < at) {
    const password = secret.slice(colon + 1, at);
    if (password) out = out.split(password).join("«credential»");
  }
  return out;
}

export function Follows() {
  const { session } = useSession();
  const mayWrite = session?.can_write ?? false;

  const [follows, setFollows] = useState<Follow[] | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [plans, setPlans] = useState<Plan[] | null>(null);
  const [plansError, setPlansError] = useState<string | null>(null);

  const [plan, setPlan] = useState("");
  // The credential. State, for the length of this form, and nowhere else.
  const [dsn, setDsn] = useState("");
  const [namespace, setNamespace] = useState("");
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);
  const [accepted, setAccepted] = useState<string | null>(null);
  const [stopping, setStopping] = useState<string | null>(null);
  const [stopError, setStopError] = useState<string | null>(null);

  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);

  const load = useCallback(async () => {
    try {
      const answer = await api.get<FollowsAnswer>("/athanor/livedb/follows");
      if (!live.current) return;
      setFollows(answer.follows ?? []);
      setListError(null);
    } catch (e) {
      if (live.current) setListError((e as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
    // A follow can stop on its own seconds after it started — the change
    // stream is opened after the first pass and that is where it usually
    // fails. A screen that showed the start and never the stop would be the
    // opposite of what this one is for, so it re-asks.
    const timer = setInterval(() => void load(), 5000);
    return () => clearInterval(timer);
  }, [load]);

  useEffect(() => {
    api
      .get<PlansAnswer>("/athanor/livedb/plans")
      .then((a) => setPlans(a.plans ?? []))
      .catch((e: Error) => setPlansError(e.message));
  }, []);

  const signed = plans?.filter((p) => p.state === "signed") ?? [];

  async function start(e: FormEvent) {
    e.preventDefault();
    setStarting(true);
    setStartError(null);
    setAccepted(null);
    const secret = dsn;
    try {
      const answer = await api.post<StartedAnswer>("/athanor/livedb/follows", {
        plan,
        dsn: secret,
        ...(namespace.trim() ? { namespace: namespace.trim() } : {}),
      });
      setAccepted(
        answer.note ??
          "accepted; the first pass runs before the change stream opens, so this may be importing for some time yet",
      );
      // Gone from this browser the moment the request is answered.
      setDsn("");
      await load();
    } catch (err) {
      const message =
        err instanceof ApiError
          ? err.isForbidden
            ? "this key may read follows but not start one: starting an import is a write, and the server refused it"
            : err.message
          : "could not reach the server";
      setStartError(scrub(message, secret));
    } finally {
      setStarting(false);
    }
  }

  async function stop(id: string) {
    setStopping(id);
    setStopError(null);
    try {
      await api.del<{ follow: Follow }>(`/athanor/livedb/follows/${encodeURIComponent(id)}`);
      await load();
    } catch (err) {
      if (err instanceof ApiError && err.isForbidden) {
        setStopError("this key may read follows but not stop one: the server refused it as a write");
      } else if (err instanceof ApiError && err.isNotFound) {
        setStopError("this server is no longer following anything under that id");
        await load();
      } else {
        setStopError(err instanceof Error ? err.message : "could not reach the server");
      }
    } finally {
      setStopping(null);
    }
  }

  return (
    <div className="space-y-6">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Follows</h1>
        <p className="text-muted-foreground mt-1 text-sm text-pretty">
          A signed plan can be kept in step with its database instead of imported once. The server owns
          the job, holds the credential in memory for the life of the follow and nowhere else, and stops
          following when it restarts — so a restart is somebody supplying the credential again.
        </p>
      </header>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Start following</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          {plansError && <LoadFailed what="The plans" error={plansError} />}
          {!plans && !plansError && <Skeleton className="h-24 w-full" />}
          {plans && !signed.length && (
            <Empty>
              No plan is signed on this server. Only a signed plan is followed — propose one, read what it
              does to every column, and sign it first.
            </Empty>
          )}

          {!!signed.length && (
            <form onSubmit={start} className="space-y-4">
              <div className="space-y-2">
                <Label htmlFor="follow-plan">Plan</Label>
                <Select value={plan} onValueChange={setPlan} disabled={!mayWrite}>
                  <SelectTrigger id="follow-plan" className="w-full min-w-0">
                    <SelectValue placeholder="a signed plan" />
                  </SelectTrigger>
                  <SelectContent>
                    {signed.map((p) => (
                      <SelectItem key={p.id} value={p.id} className="font-mono text-xs">
                        {p.source.redacted || p.source_key || p.id}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>

              <div className="space-y-2">
                <Label htmlFor="follow-dsn">Connection string</Label>
                <Input
                  id="follow-dsn"
                  type="password"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="postgres://user:password@host:5432/db"
                  value={dsn}
                  disabled={!mayWrite}
                  onChange={(e) => setDsn(e.target.value)}
                />
                <p className="text-muted-foreground flex items-start gap-2 text-xs text-pretty">
                  <KeyRound className="mt-0.5 size-3.5 shrink-0" aria-hidden />
                  The store never kept it, so a follow supplies it again. It is sent in the body of this
                  request, held in the server's memory for as long as the follow runs, and shown back
                  nowhere — the listing below names each database by its redacted form.
                </p>
              </div>

              <div className="space-y-2">
                <Label htmlFor="follow-namespace">Namespace</Label>
                <Input
                  id="follow-namespace"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="optional — the plan id by default"
                  value={namespace}
                  disabled={!mayWrite}
                  onChange={(e) => setNamespace(e.target.value)}
                />
              </div>

              {!mayWrite && (
                <p className="text-muted-foreground text-sm text-pretty">
                  This key is read-only. It reads this screen and the imports; starting and stopping a
                  follow are writes, and the server refuses them.
                </p>
              )}

              {startError && (
                <Alert variant="destructive">
                  <TriangleAlert />
                  <AlertDescription className="break-words">{startError}</AlertDescription>
                </Alert>
              )}

              {accepted && (
                <Alert>
                  <Loader2 className="animate-spin" />
                  <AlertDescription className="text-pretty">
                    Accepted, not finished. {accepted}
                  </AlertDescription>
                </Alert>
              )}

              <Button type="submit" disabled={!mayWrite || starting || !plan || !dsn}>
                {starting ? <Loader2 className="animate-spin" /> : <Play />}
                Follow
              </Button>
            </form>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="gap-2">
          <CardTitle className="text-base">What this server is following</CardTitle>
          <Button variant="ghost" size="sm" onClick={() => void load()} className="w-fit">
            <RefreshCw /> Refresh
          </Button>
        </CardHeader>
        <CardContent className="space-y-3">
          {listError && <LoadFailed what="The follows" error={listError} />}
          {!follows && !listError && <Skeleton className="h-24 w-full" />}
          {follows && !follows.length && !listError && (
            <Empty>
              This server is following nothing. A follow does not survive a restart, because the
              credential it needs was never stored — so an empty list after a restart means somebody has
              to start it again, not that it failed.
            </Empty>
          )}
          {stopError && (
            <Alert variant="destructive">
              <TriangleAlert />
              <AlertDescription className="break-words">{stopError}</AlertDescription>
            </Alert>
          )}

          {follows?.map((f) => {
            const running = f.state === "running";
            return (
              <div key={f.id} className="rounded-lg border p-3 sm:p-4">
                <div className="flex flex-wrap items-center gap-2">
                  <Badge variant={running ? "default" : "outline"} className="text-xs">
                    {running ? "running" : "stopped"}
                  </Badge>
                  {f.namespace && (
                    <Badge variant="outline" className="max-w-full font-mono text-xs break-words whitespace-normal">
                      {f.namespace}
                    </Badge>
                  )}
                  <span className="text-muted-foreground min-w-0 font-mono text-xs break-all">{f.id}</span>
                </div>

                <p className="mt-2 font-mono text-xs break-all">
                  <Link to={`/import/runs?plan=${encodeURIComponent(f.plan)}`} className="hover:underline">
                    {f.plan}
                  </Link>
                </p>

                {f.redacted && (
                  <p className="text-muted-foreground mt-1.5 flex items-start gap-2 font-mono text-xs break-all">
                    <Database className="mt-0.5 size-3.5 shrink-0" aria-hidden />
                    {f.redacted}
                  </p>
                )}

                <p className="text-muted-foreground mt-1.5 text-sm text-pretty">
                  Started {when(f.started_at)}
                  {f.stopped_at && ` · stopped ${when(f.stopped_at)}`}
                </p>

                {f.error && (
                  <div className="border-destructive/40 bg-destructive/5 mt-3 rounded-md border p-3">
                    <p className="text-destructive text-sm text-pretty">
                      It stopped for a reason, and it stays listed with the reason until somebody deletes
                      it.
                    </p>
                    <p className="text-destructive/90 mt-1.5 font-mono text-xs break-words">{f.error}</p>
                  </div>
                )}

                {!f.error && !running && (
                  <p className="text-muted-foreground mt-3 text-sm text-pretty">
                    It stopped without an error: somebody ended it, or this server did on its way down.
                  </p>
                )}

                {f.first_pass && (
                  <p className="text-muted-foreground mt-3 text-sm text-pretty tabular-nums">
                    Its first pass read {f.first_pass.rows_read.toLocaleString()} rows into{" "}
                    {f.first_pass.chunks.toLocaleString()} chunks and{" "}
                    {f.first_pass.triples.toLocaleString()} triples
                    {f.first_pass.drift?.length
                      ? `, and met ${f.first_pass.drift.length} ${
                          f.first_pass.drift.length === 1 ? "column" : "columns"
                        } the signature does not cover`
                      : ""}
                    .
                  </p>
                )}

                {running && (
                  <p className="text-muted-foreground mt-3 text-sm text-pretty">
                    The first pass and the change stream are one job underneath, so this screen cannot say
                    which of them it is in.{" "}
                    <Link
                      to={`/import/runs?plan=${encodeURIComponent(f.plan)}`}
                      className="underline underline-offset-4"
                    >
                      A run under this plan
                    </Link>{" "}
                    is the first pass having finished.
                  </p>
                )}

                <div className="mt-3">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!mayWrite || stopping === f.id}
                    onClick={() => void stop(f.id)}
                  >
                    {stopping === f.id ? <Loader2 className="animate-spin" /> : <Square />}
                    {running ? "Stop" : "Clear"}
                  </Button>
                </div>
              </div>
            );
          })}
        </CardContent>
      </Card>
    </div>
  );
}
