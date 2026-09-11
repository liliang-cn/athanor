import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ArrowLeft, Info, Loader2, Lock, ShieldAlert, TriangleAlert } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Empty, LoadFailed } from "@/components/empty";
import { ApiError, api } from "@/lib/api";
import { useSession } from "@/lib/session";
import type { Plan, PlanCounts, RunReport, Treatment } from "@/lib/types";

/**
 * Importing from a database somebody else runs.
 *
 * Three steps, and the order is the whole product: connect, read the plan,
 * sign it. Nothing reads a row until a person has signed for what leaves the
 * database, and the signature names the hash of the plan they read — so a
 * plan amended in another tab is refused rather than silently signed.
 *
 * # The credential
 *
 * The DSN is a secret this screen holds for the length of the wizard and
 * never puts back on the page. It is in React state, it is sent in a POST
 * body, and that is the entire list: never in the path, never in a query
 * string, never in a hidden input, never rendered back masked, and never in
 * an error message — the last one is enforced here as well as on the server,
 * because a driver's error is the classic place a connection string escapes
 * and the cost of being wrong once is a password in somebody's browser
 * history for ever. What a person is shown instead is `source.redacted`,
 * which is the plan's own record of which database this was.
 *
 * The form is deliberately not a <form>: a form with no submit handler
 * navigates, and navigating would put every field in this screen — the DSN
 * among them — into the address bar and the session history.
 *
 * # Refusals
 *
 * A read-only key may read a plan and may not propose, amend, sign or run.
 * The controls are disabled up front from the session's clearance, and the
 * 403 is still handled, because the server decides and this screen only
 * predicts. A 409 on signing is not a failure — it is the workflow working —
 * so it is presented as a thing that happened rather than a thing that broke.
 *
 * The host allow-list refusal is rendered exactly as the server wrote it.
 * Nothing here inspects the DSN to guess whether the host or the syntax was
 * the problem: a screen that told those apart would be an oracle for the list.
 */

const PLANS = "/athanor/livedb/plans";

/** The desensitizer's closed set, with what each one does to a real value.
 *  A treatment nobody can describe is a treatment nobody can sign for. */
const ACTIONS = [
  { value: "keep", what: "the value, as it is" },
  { value: "mask", what: "partly hidden: 138****1234" },
  { value: "generalize", what: "widened: 34 becomes 30-40" },
  { value: "hash", what: "a one-way token; never turned back" },
  { value: "pseudonymize", what: "a token the vault can turn back" },
  { value: "redact", what: "[REDACTED]" },
  { value: "drop", what: "never leaves the database" },
] as const;

/** The summary the plan leads with. Order is the reviewer's order: how big,
 *  how much of it is personal, then what was done about it, then the two
 *  numbers that are about what could still go wrong. */
const COUNTS: { key: keyof PlanCounts; label: string; hint: string; always: boolean }[] = [
  { key: "columns", label: "columns", hint: "what this plan covers", always: true },
  { key: "personal", label: "personal", hint: "the classifier called these people", always: true },
  { key: "dropped", label: "dropped", hint: "never leave the database", always: true },
  { key: "masked", label: "masked", hint: "partly hidden", always: true },
  { key: "generalized", label: "generalized", hint: "widened until they stop identifying", always: true },
  { key: "hashed", label: "hashed", hint: "one-way tokens", always: false },
  { key: "redacted", label: "redacted", hint: "replaced wholesale", always: false },
  { key: "passed", label: "passed", hint: "enter the graph unchanged", always: true },
  { key: "reversible", label: "reversible", hint: "the vault can turn these back", always: true },
  {
    key: "unscanned_text",
    label: "unscanned text",
    hint: "prose kept whole, with nothing looking inside it",
    always: true,
  },
];

/** What kind of refusal this was, so each gets its own presentation. */
type Refusal = { where: string; kind: "forbidden" | "conflict" | "plain"; message: string };

/**
 * Take the credential back out of anything about to be rendered.
 *
 * The server scrubs its own messages already. This is the second layer, and
 * it is worth the lines for the same reason the server's is: a message is the
 * one path by which a secret held in this component could reach the screen,
 * and belt and braces is cheap next to a leaked password.
 */
function scrub(message: string, dsn: string): string {
  const secret = dsn.trim();
  if (!secret) return message;
  let out = message.split(secret).join("«credential»");
  const password = passwordIn(secret);
  if (password) out = out.split(password).join("«credential»");
  return out;
}

/** The password inside a connection string, in the shapes the two drivers
 *  accept. Only ever used to remove text, never to decide anything. */
function passwordIn(dsn: string): string {
  const at = dsn.lastIndexOf("@");
  if (at > 0) {
    const head = dsn.slice(0, at);
    const scheme = head.indexOf("://");
    const colon = head.indexOf(":", scheme >= 0 ? scheme + 3 : 0);
    if (colon > 0) return head.slice(colon + 1);
  }
  for (const field of dsn.split(/\s+/)) {
    if (field.startsWith("password=") && field.length > "password=".length) {
      return field.slice("password=".length);
    }
  }
  return "";
}

function when(at?: string): string {
  if (!at) return "";
  const t = new Date(at);
  // The Go zero time is what an unsigned plan carries in signed_at.
  if (Number.isNaN(t.getTime()) || t.getUTCFullYear() < 1970) return "";
  return t.toLocaleString();
}

export function LiveDb() {
  const { session } = useSession();
  const canWrite = !!session?.can_write;

  // The credential, and nothing else that touches it.
  const [dsn, setDsn] = useState("");
  const [driver, setDriver] = useState("postgres");
  const [schema, setSchema] = useState("");
  const [tables, setTables] = useState("");
  const [defaultAction, setDefaultAction] = useState("redact");
  const [scanText, setScanText] = useState("no");

  const [plan, setPlan] = useState<Plan | null>(null);
  const [signer, setSigner] = useState(session?.actor ?? "");
  const [note, setNote] = useState("");
  const [namespace, setNamespace] = useState("");
  const [report, setReport] = useState<RunReport | null>(null);

  const [busy, setBusy] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  // Plans this brain already holds. Read on load, because reading one is a
  // read: a read-only key can open the plan somebody signed and check it,
  // which is the whole point of a signature, and it would never see one if
  // the only way into this screen were the propose button it may not press.
  const [plans, setPlans] = useState<Plan[] | null>(null);
  const [plansError, setPlansError] = useState<string | null>(null);

  const listPlans = useCallback(() => {
    api
      .get<{ plans: Plan[] }>(PLANS)
      .then((r) => setPlans(r.plans ?? []))
      .catch((e: Error) => setPlansError(e.message));
  }, []);

  useEffect(listPlans, [listPlans]);

  const fail = useCallback(
    (where: string, e: unknown) => {
      const err = e instanceof ApiError ? e : null;
      // The credential never reaches a rendered byte, including this one.
      const message = scrub((e as Error)?.message ?? String(e), dsn);
      const kind = err?.isForbidden ? "forbidden" : err?.isConflict ? "conflict" : "plain";
      setRefusal({ where, kind, message });
    },
    [dsn],
  );

  const act = useCallback(
    async (where: string, run: () => Promise<void>) => {
      setBusy(where);
      setRefusal(null);
      try {
        await run();
      } catch (e) {
        fail(where, e);
      } finally {
        setBusy(null);
      }
    },
    [fail],
  );

  const propose = () =>
    act("connect", async () => {
      const proposed = await api.post<Plan>(PLANS, {
        source: {
          driver,
          dsn,
          schema: schema.trim() || undefined,
          tables: tables
            .split(",")
            .map((t) => t.trim())
            .filter(Boolean),
        },
        options: { default_action: defaultAction, scan_text: scanText === "yes" },
      });
      setReport(null);
      setPlan(proposed);
      listPlans();
    });

  const override = (t: Treatment, action: string) =>
    act("plan", async () => {
      setPlan(
        await api.patch<Plan>(`${PLANS}/${plan!.id}`, {
          changes: [
            {
              table: t.table,
              column: t.column,
              action,
              reason: "changed on the review screen before signing",
            },
          ],
          by: signer.trim() || undefined,
        }),
      );
    });

  // The signature quotes back the hash this page rendered, not whatever the
  // plan happens to hold when the button is pressed. That is what makes a 409
  // possible, and a 409 is the only thing standing between "I read this" and
  // "I clicked a box".
  const sign = (hash: string) =>
    act("sign", async () => {
      setPlan(
        await api.post<Plan>(`${PLANS}/${plan!.id}/signature`, {
          hash,
          by: signer.trim() || undefined,
          note: note.trim() || undefined,
        }),
      );
    });

  const open = (id: string) =>
    act("plan", async () => {
      setReport(null);
      setPlan(await api.get<Plan>(`${PLANS}/${id}`));
    });

  const reload = () =>
    act("sign", async () => {
      setPlan(await api.get<Plan>(`${PLANS}/${plan!.id}`));
    });

  const runPlan = (dryRun: boolean) =>
    act("run", async () => {
      setReport(
        await api.post<RunReport>("/athanor/livedb/runs", {
          plan: plan!.id,
          dsn,
          namespace: namespace.trim() || undefined,
          dry_run: dryRun,
        }),
      );
    });

  const startOver = () => {
    setPlan(null);
    setReport(null);
    setRefusal(null);
    setDsn("");
  };

  const signed = plan?.state === "signed";

  return (
    <div className="space-y-6">
      <header>
        <Button asChild variant="ghost" size="sm" className="-ml-2 mb-2">
          <Link to="/import">
            <ArrowLeft className="size-4" /> Import
          </Link>
        </Button>
        <h1 className="text-xl font-semibold tracking-tight">Import from a live database</h1>
        <p className="text-muted-foreground mt-1 text-sm text-pretty">
          Read the schema, decide what leaves it, sign for that decision, and only then read a row.
          Nothing on this screen touches your data until the plan below carries a signature.
        </p>
      </header>

      {!canWrite && (
        <Alert>
          <Lock />
          <AlertTitle>This key may read a plan and not make one</AlertTitle>
          <AlertDescription>
            <p>
              {session?.actor ? `${session.actor} is ` : "This key is "}
              read-only, so proposing, amending, signing and running are all refused. The controls
              below are turned off rather than left to come back 403.
            </p>
          </AlertDescription>
        </Alert>
      )}

      {/* 1 — Connect */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            <span className="text-muted-foreground mr-2 tabular-nums">1</span>Connect
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          {plan ? (
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
              <div className="min-w-0">
                <p className="text-muted-foreground text-xs">Reading</p>
                <p className="font-mono text-sm break-all">{plan.source.redacted}</p>
              </div>
              <Button variant="outline" size="sm" onClick={startOver} className="ml-auto">
                Start over
              </Button>
            </div>
          ) : (
            <>
              <p className="text-muted-foreground text-sm text-pretty">
                The connection string is used to read the schema and is never stored: the plan keeps
                the redacted form, and the run below asks for the credential again because this
                server never had it.
              </p>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-2">
                  <Label htmlFor="driver">Driver</Label>
                  <Select value={driver} onValueChange={setDriver} disabled={!canWrite}>
                    <SelectTrigger id="driver" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="postgres">postgres</SelectItem>
                      <SelectItem value="mysql">mysql</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-2">
                  <Label htmlFor="schema">Schema</Label>
                  <Input
                    id="schema"
                    value={schema}
                    onChange={(e) => setSchema(e.target.value)}
                    placeholder="public"
                    disabled={!canWrite}
                  />
                </div>
                <div className="space-y-2 sm:col-span-2">
                  <Label htmlFor="dsn">Connection string</Label>
                  <Input
                    id="dsn"
                    type="password"
                    autoComplete="off"
                    spellCheck={false}
                    value={dsn}
                    onChange={(e) => setDsn(e.target.value)}
                    placeholder="postgres://user:password@host:5432/database?sslmode=disable"
                    disabled={!canWrite}
                  />
                  <p className="text-muted-foreground text-xs text-pretty">
                    Held in this page for as long as the wizard is open, sent in a request body, and
                    shown back nowhere.
                  </p>
                </div>
                <div className="space-y-2 sm:col-span-2">
                  <Label htmlFor="tables">Tables</Label>
                  <Input
                    id="tables"
                    value={tables}
                    onChange={(e) => setTables(e.target.value)}
                    placeholder="leave empty for every base table"
                    disabled={!canWrite}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="default-action">A column nobody classified</Label>
                  <Select value={defaultAction} onValueChange={setDefaultAction} disabled={!canWrite}>
                    <SelectTrigger id="default-action" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {ACTIONS.map((a) => (
                        <SelectItem key={a.value} value={a.value}>
                          {a.value} — {a.what}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <p className="text-muted-foreground text-xs text-pretty">
                    Redact fails closed and makes a first proposal of nothing but [REDACTED]. Keep
                    is the honest choice once you have read your own schema — and you read the
                    consequence below before anything is signed.
                  </p>
                </div>
                <div className="space-y-2">
                  <Label htmlFor="scan-text">Free text</Label>
                  <Select value={scanText} onValueChange={setScanText} disabled={!canWrite}>
                    <SelectTrigger id="scan-text" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="no">keep prose whole</SelectItem>
                      <SelectItem value="yes">scan prose for what looks personal</SelectItem>
                    </SelectContent>
                  </Select>
                  <p className="text-muted-foreground text-xs text-pretty">
                    The column classifier judges the column, not the sentences, and people type
                    addresses into free text.
                  </p>
                </div>
              </div>
              <Button onClick={propose} disabled={!canWrite || !dsn.trim() || busy !== null}>
                {busy === "connect" && <Loader2 className="size-4 animate-spin" />}
                Read the schema and propose a plan
              </Button>
              {refusal?.where === "connect" && <Refused refusal={refusal} />}
            </>
          )}
        </CardContent>
      </Card>

      {/* 2 — The plan */}
      <Card>
        <CardHeader>
          <CardTitle className="flex flex-wrap items-center gap-2 text-base">
            <span>
              <span className="text-muted-foreground mr-2 tabular-nums">2</span>The plan
            </span>
            {plan && (
              <Badge variant={signed ? "secondary" : "outline"} className="ml-auto">
                {plan.state}
              </Badge>
            )}
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-5">
          {!plan && (
            <>
              <Empty className="py-3">
                No plan open. Connect above to propose one from the schema, or open one this brain
                already holds — reading a plan is a read, and checking what somebody signed is what
                a signature is for.
              </Empty>
              {plansError && <LoadFailed what="The plans" error={plansError} />}
              {plans && !plansError && !plans.length && (
                <Empty className="py-3">Nothing has been proposed against any database yet.</Empty>
              )}
              <div className="space-y-2">
                {plans?.map((p) => (
                  <button
                    key={p.id}
                    type="button"
                    onClick={() => open(p.id)}
                    disabled={busy !== null}
                    className="hover:bg-accent flex w-full flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border p-3 text-left transition-colors disabled:opacity-60"
                  >
                    <span className="min-w-0 flex-1 font-mono text-xs break-all">
                      {p.source.redacted || p.source_key}
                    </span>
                    <Badge variant={p.state === "signed" ? "secondary" : "outline"}>{p.state}</Badge>
                    <span className="text-muted-foreground w-full text-xs">
                      {p.counts.columns} columns · {p.signed_by ? `signed by ${p.signed_by}` : `proposed by ${p.created_by || "somebody"}`}
                      {when(p.signed_at) || when(p.created_at)
                        ? ` · ${when(p.signed_at) || when(p.created_at)}`
                        : ""}
                    </span>
                  </button>
                ))}
              </div>
              {refusal?.where === "plan" && <Refused refusal={refusal} />}
            </>
          )}

          {plan && (
            <>
              <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5">
                {COUNTS.filter((c) => c.always || plan.counts[c.key] > 0).map((c) => (
                  <div key={c.key} className="rounded-lg border p-3">
                    <div className="text-lg leading-none font-medium tabular-nums">
                      {plan.counts[c.key]}
                    </div>
                    <div className="mt-1 text-xs font-medium">{c.label}</div>
                    <p className="text-muted-foreground mt-1 text-xs text-pretty">{c.hint}</p>
                  </div>
                ))}
              </div>

              {plan.counts.unscanned_text > 0 && (
                <Alert>
                  <Info />
                  <AlertTitle>
                    {plan.counts.unscanned_text} free-text{" "}
                    {plan.counts.unscanned_text === 1 ? "column passes" : "columns pass"} through
                    whole
                  </AlertTitle>
                  <AlertDescription>
                    <p>
                      Nothing is looking inside them. For a hostname or a rack number that is
                      correct; for a column somebody types sentences into it means whatever they
                      typed enters the graph intact. Re-propose with prose scanning on, or accept it
                      knowingly.
                    </p>
                  </AlertDescription>
                </Alert>
              )}

              {plan.counts.reversible > 0 && (
                <Alert>
                  <ShieldAlert />
                  <AlertTitle>
                    {plan.counts.reversible}{" "}
                    {plan.counts.reversible === 1 ? "column can be" : "columns can be"} turned back
                  </AlertTitle>
                  <AlertDescription>
                    <p>
                      A pseudonymized value is a token, and the original is kept in this Athanor's
                      vault. That is reversible on purpose — somebody with the vault key can recover
                      the value the graph does not hold. It is also the reason signing is refused
                      outright on a deployment with no vault: a reversible treatment with nowhere to
                      put the original is a treatment that silently is not one.
                    </p>
                  </AlertDescription>
                </Alert>
              )}

              <div>
                <p className="text-muted-foreground mb-3 text-sm text-pretty">
                  One row per column: what is in the database, what the classifier called it, what
                  happens to it, and what that turns into. A sample of a column the classifier calls
                  personal is shown masked — enough to check the guess, not enough to be the leak
                  this screen exists to prevent.
                </p>

                {/* A desk: five columns read across. */}
                <div className="hidden md:block">
                  {/* Fixed, so a long national-id sample cannot push the one
                      column a reviewer is really here for off the edge: every
                      cell wraps instead. */}
                  <Table className="table-fixed">
                    <TableHeader>
                      <TableRow>
                        <TableHead className="w-[17%]">Column</TableHead>
                        <TableHead className="w-[20%]">In the database</TableHead>
                        <TableHead className="w-[18%]">Detected</TableHead>
                        <TableHead className="w-[10.5rem]">Treatment</TableHead>
                        <TableHead>Enters the graph</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {plan.columns.map((t) => (
                        <TableRow key={`${t.table}.${t.column}`}>
                          <TableCell className="align-top whitespace-normal">
                            <div className="font-medium break-words">{t.column}</div>
                            <div className="text-muted-foreground text-xs break-words">{t.table}</div>
                          </TableCell>
                          <TableCell className="text-muted-foreground align-top whitespace-normal">
                            <div className="font-mono text-xs break-all">{t.sample || "—"}</div>
                            <div className="text-xs">{t.type || "unknown type"}</div>
                          </TableCell>
                          <TableCell className="align-top whitespace-normal">
                            <Detected t={t} />
                          </TableCell>
                          <TableCell className="align-top whitespace-normal">
                            <TreatmentPicker
                              t={t}
                              disabled={!canWrite || signed || busy !== null}
                              onChange={(a) => override(t, a)}
                            />
                          </TableCell>
                          <TableCell className="align-top whitespace-normal">
                            <Enters t={t} />
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>

                {/* A thumb: the same five facts stacked, because a table
                    somebody has to drag sideways is a table nobody reads. */}
                <div className="space-y-3 md:hidden">
                  {plan.columns.map((t) => (
                    <div key={`${t.table}.${t.column}`} className="space-y-3 rounded-lg border p-3">
                      <div>
                        <div className="font-medium break-words">{t.column}</div>
                        <div className="text-muted-foreground text-xs break-words">
                          {t.table} · {t.type || "unknown type"}
                        </div>
                      </div>
                      <dl className="space-y-1.5 text-sm">
                        <div className="flex flex-wrap gap-x-2">
                          <dt className="text-muted-foreground text-xs">In the database</dt>
                          <dd className="min-w-0 flex-1 font-mono text-xs break-all">
                            {t.sample || "—"}
                          </dd>
                        </div>
                        <div className="flex flex-wrap items-baseline gap-x-2">
                          <dt className="text-muted-foreground text-xs">Detected</dt>
                          <dd className="min-w-0 flex-1">
                            <Detected t={t} />
                          </dd>
                        </div>
                        <div className="flex flex-wrap items-baseline gap-x-2">
                          <dt className="text-muted-foreground text-xs">Enters the graph</dt>
                          <dd className="min-w-0 flex-1">
                            <Enters t={t} />
                          </dd>
                        </div>
                      </dl>
                      <TreatmentPicker
                        t={t}
                        disabled={!canWrite || signed || busy !== null}
                        onChange={(a) => override(t, a)}
                      />
                    </div>
                  ))}
                </div>
              </div>

              {refusal?.where === "plan" && <Refused refusal={refusal} />}
            </>
          )}
        </CardContent>
      </Card>

      {/* 3 — Sign */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            <span className="text-muted-foreground mr-2 tabular-nums">3</span>Sign
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          {!plan && <Empty>A plan is signed once there is one.</Empty>}

          {plan && (
            <>
              <p className="text-sm text-pretty">
                What the ledger records is a signature, not a checkbox. It names the hash of the
                plan as it is printed above, and that hash is what is sent — so if somebody amends
                this plan in another tab between your reading it and your signing it, the signature
                is refused instead of quietly landing on a plan you never read.
              </p>
              <div>
                <p className="text-muted-foreground text-xs">The hash you are signing</p>
                <p className="font-mono text-xs break-all">{plan.hash}</p>
              </div>

              {signed ? (
                <Alert>
                  <Lock />
                  <AlertTitle>Signed{plan.signed_by ? ` by ${plan.signed_by}` : ""}</AlertTitle>
                  <AlertDescription>
                    <p>
                      {when(plan.signed_at) || "Just now"}. A signed plan is never edited in place —
                      changing it means proposing a new one, which supersedes this.
                      {plan.note ? ` "${plan.note}"` : ""}
                    </p>
                  </AlertDescription>
                </Alert>
              ) : (
                <>
                  <div className="grid gap-4 sm:grid-cols-2">
                    <div className="space-y-2">
                      <Label htmlFor="signer">Signed by</Label>
                      <Input
                        id="signer"
                        value={signer}
                        onChange={(e) => setSigner(e.target.value)}
                        placeholder={session?.actor ?? "your name"}
                        disabled={!canWrite}
                      />
                    </div>
                    <div className="space-y-2">
                      <Label htmlFor="note">Note</Label>
                      <Input
                        id="note"
                        value={note}
                        onChange={(e) => setNote(e.target.value)}
                        placeholder="what you checked"
                        disabled={!canWrite}
                      />
                    </div>
                  </div>
                  <Button onClick={() => sign(plan.hash)} disabled={!canWrite || busy !== null}>
                    {busy === "sign" && <Loader2 className="size-4 animate-spin" />}
                    Sign this plan
                  </Button>
                </>
              )}

              {refusal?.where === "sign" && (
                <Refused refusal={refusal} onReload={refusal.kind === "conflict" ? reload : undefined} />
              )}
            </>
          )}
        </CardContent>
      </Card>

      {/* Run */}
      {signed && plan && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Run</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <p className="text-muted-foreground text-sm text-pretty">
              The credential is asked for once, at the top of this screen, and sent again here: this
              server never stored it. A dry run reads and desensitizes and writes nothing, which is
              how you see the row count and the drift before committing to either.
            </p>
            <div className="space-y-2 sm:max-w-sm">
              <Label htmlFor="namespace">Namespace</Label>
              <Input
                id="namespace"
                value={namespace}
                onChange={(e) => setNamespace(e.target.value)}
                placeholder={plan.id}
                disabled={!canWrite}
              />
            </div>
            <div className="flex flex-wrap gap-2">
              <Button
                variant="outline"
                onClick={() => runPlan(true)}
                disabled={!canWrite || !dsn.trim() || busy !== null}
              >
                {busy === "run" && <Loader2 className="size-4 animate-spin" />}
                Dry run
              </Button>
              <Button
                onClick={() => runPlan(false)}
                disabled={!canWrite || !dsn.trim() || busy !== null}
              >
                Run and write
              </Button>
            </div>
            {!dsn.trim() && (
              <p className="text-muted-foreground text-sm text-pretty">
                This page no longer holds a credential. Start over to supply one — the server cannot
                give it back, because it never kept it.
              </p>
            )}

            {refusal?.where === "run" && <Refused refusal={refusal} />}
            {report && <Report report={report} />}
          </CardContent>
        </Card>
      )}
    </div>
  );
}

/** What the classifier called a column, and why. */
function Detected({ t }: { t: Treatment }) {
  return (
    <div className="min-w-0">
      <span className="text-sm">{t.pii_kind || "not personal"}</span>
      {t.reason && (
        <p className="text-muted-foreground text-xs break-words">
          {t.reason}
          {t.by ? ` · ${t.by}` : ""}
        </p>
      )}
      {t.scan && <p className="text-muted-foreground text-xs">prose scanned</p>}
    </div>
  );
}

/** The only column a reviewer really reads. */
function Enters({ t }: { t: Treatment }) {
  if (t.action === "drop") {
    return <span className="text-muted-foreground text-sm">nothing</span>;
  }
  return <span className="font-mono text-xs break-all">{t.enters || "—"}</span>;
}

/** The override. Changing it amends the draft, which changes the hash — so
 *  the signature below is against the plan as amended, never the one before. */
function TreatmentPicker({
  t,
  disabled,
  onChange,
}: {
  t: Treatment;
  disabled: boolean;
  onChange: (action: string) => void;
}) {
  return (
    <Select value={t.action} onValueChange={onChange} disabled={disabled}>
      {/* The trigger says the word and the list says what the word does: the
          column that matters — what enters the graph — does not get to be
          narrow because a dropdown wanted to explain itself in place. */}
      <SelectTrigger size="sm" className="w-full">
        <SelectValue>{t.action}</SelectValue>
      </SelectTrigger>
      <SelectContent>
        {ACTIONS.map((a) => (
          <SelectItem key={a.value} value={a.value}>
            {a.value} — {a.what}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

/** What one import did, including the parts that mean a re-signing is owed. */
function Report({ report }: { report: RunReport }) {
  return (
    <div className="space-y-4 rounded-lg border p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium">{report.dry_run ? "Dry run" : "Run"}</span>
        <span className="text-muted-foreground font-mono text-xs break-all">{report.id}</span>
      </div>
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {[
          ["rows read", report.rows_read],
          ["chunks", report.chunks],
          ["triples", report.triples],
          ["skipped", report.skipped],
        ].map(([label, n]) => (
          <div key={label as string} className="rounded-lg border p-3">
            <div className="text-lg leading-none font-medium tabular-nums">{n}</div>
            <div className="text-muted-foreground mt-1 text-xs">{label}</div>
          </div>
        ))}
      </div>
      {report.dry_run && (
        <p className="text-muted-foreground text-sm text-pretty">
          Nothing was written. The rows were read and desensitized so the counts and the drift are
          real; the graph is untouched.
        </p>
      )}

      {!!report.drift?.length && (
        <Alert>
          <TriangleAlert />
          <AlertTitle>The database has columns this plan does not name</AlertTitle>
          <AlertDescription>
            <p>
              They were dropped rather than imported, so nothing leaked — but the plan somebody
              signed no longer describes the database, and a re-signing is owed.
            </p>
            <ul className="mt-1 space-y-0.5">
              {report.drift.map((c) => (
                <li key={c} className="font-mono text-xs break-all">
                  {c}
                </li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}

      {!!report.gone?.length && (
        <Alert>
          <TriangleAlert />
          <AlertTitle>The plan names columns the database no longer has</AlertTitle>
          <AlertDescription>
            <ul className="mt-1 space-y-0.5">
              {report.gone.map((c) => (
                <li key={c} className="font-mono text-xs break-all">
                  {c}
                </li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}

      {!!report.errors?.length && (
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>
            {report.errors.length} {report.errors.length === 1 ? "row" : "rows"} failed
          </AlertTitle>
          <AlertDescription>
            <ul className="mt-1 space-y-0.5">
              {report.errors.map((e, i) => (
                <li key={i} className="font-mono text-xs break-words">
                  {e}
                </li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}
    </div>
  );
}

/**
 * A refusal, in the three flavours that mean different things.
 *
 * The message is the server's own and is never rewritten — in particular the
 * host allow-list refusal, which is one sentence for "not on the list" and
 * for "I could not read that connection string" on purpose, and which this
 * screen must not improve upon.
 */
function Refused({ refusal, onReload }: { refusal: Refusal; onReload?: () => void }) {
  if (refusal.kind === "conflict") {
    return (
      <Alert>
        <TriangleAlert />
        <AlertTitle>The plan changed since you read it</AlertTitle>
        <AlertDescription>
          <p>
            Nothing was signed. The hash on this page names a plan that is no longer the stored one
            — somebody amended it — and a signature on a plan you did not read is the one thing this
            workflow exists to refuse. Reload it, read what changed, and sign that.
          </p>
          <p className="font-mono text-xs break-words opacity-80">{refusal.message}</p>
          {onReload && (
            <Button variant="outline" size="sm" onClick={onReload} className="mt-1">
              Reload the plan
            </Button>
          )}
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <Alert variant="destructive">
      {refusal.kind === "forbidden" ? <Lock /> : <TriangleAlert />}
      <AlertTitle>{refusal.kind === "forbidden" ? "Refused" : "That did not go through"}</AlertTitle>
      <AlertDescription>
        <p className="text-destructive/90 break-words">{refusal.message}</p>
      </AlertDescription>
    </Alert>
  );
}
