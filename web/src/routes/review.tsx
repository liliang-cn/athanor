import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { CheckCircle2, ChevronsUpDown, ClipboardCheck, Loader2, PencilLine, RefreshCw, XCircle } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Empty, LoadFailed } from "@/components/empty";
import { api, ApiError } from "@/lib/api";
import type { Decision, DecideAnswer, Finding, FindingsAnswer, JobState, ReviewVerb } from "@/lib/types";

/**
 * The queue, as a screen rather than as a wall.
 *
 * What this replaces asked for a job ID on an otherwise empty page, before it
 * would show anything at all — a modal dialog painted as a whole application.
 * Picking which job to look at is a small decision and it is made here the way
 * small decisions are made everywhere else in this app: a dropdown of the ones
 * this browser has opened, and a dialog behind it for an ID that is not in the
 * list yet.
 *
 * # Why the dropdown remembers rather than asks the server
 *
 * alchemy holds work in progress and not a catalogue, which is a real position
 * and not an omission: a job lives an hour, and a service that listed them
 * would be publishing a queue it does not own. So the list here is this
 * browser's own history of jobs it has opened. It is a convenience and is
 * treated as one — it is not authority, it is not shared, and every entry is
 * checked against the server before anything is drawn.
 */

const RECENT_KEY = "athanor.review.recent";
const RECENT_MAX = 8;

function recentJobs(): string[] {
  try {
    const raw = localStorage.getItem(RECENT_KEY);
    const ids: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(ids) ? ids.filter((x): x is string => typeof x === "string") : [];
  } catch {
    // A private window, or site data somebody cleared. The screen works
    // without the list; it just cannot offer one.
    return [];
  }
}

function rememberJob(id: string) {
  try {
    const next = [id, ...recentJobs().filter((x) => x !== id)].slice(0, RECENT_MAX);
    localStorage.setItem(RECENT_KEY, JSON.stringify(next));
  } catch {
    /* see recentJobs */
  }
}

/** The state, in the words the banner uses. */
const STATE_LABEL: Record<JobState, string> = {
  JOB_STATE_PENDING: "waiting to start",
  JOB_STATE_RUNNING: "running",
  JOB_STATE_NEEDS_REVIEW: "held — needs review",
  JOB_STATE_SUCCEEDED: "delivered",
  JOB_STATE_FAILED: "failed",
};

const KIND_LABEL: Record<string, string> = {
  conflict: "two sources disagree",
  duplicate: "may be one node",
  violation: "the vocabulary does not declare this",
  guess: "something was guessed",
  low_confidence: "the model was unsure",
};

/** The verb somebody already used on this item, as the screen spells verbs.
 *  `always` is an accept that also made a rule; the queue shows the act. */
function answerVerb(f: Finding): ReviewVerb | undefined {
  switch (f.answer?.verb) {
    case "REVIEW_VERB_ACCEPT":
    case "REVIEW_VERB_ALWAYS":
      return "accept";
    case "REVIEW_VERB_EDIT":
      return "edit";
    case "REVIEW_VERB_REJECT":
      return "reject";
    default:
      return undefined;
  }
}

export function Review() {
  const { jobId } = useParams();
  const navigate = useNavigate();
  const [recent, setRecent] = useState<string[]>(recentJobs);

  const pick = useCallback(
    (id: string) => {
      const trimmed = id.trim();
      if (!trimmed) return;
      rememberJob(trimmed);
      setRecent(recentJobs());
      navigate(`/review/${trimmed}`);
    },
    [navigate],
  );

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Review</h1>
          <p className="text-muted-foreground mt-1 max-w-2xl text-sm">
            A job is held when its graph contradicts itself, and in review mode for everything else
            it was asked to ask about. Nothing reaches the brain until every question here has an
            answer, and every answer names who gave it.
          </p>
        </div>
        <JobPicker current={jobId} recent={recent} onPick={pick} />
      </div>
      {jobId ? <Queue jobId={jobId} /> : <NoJobYet onPick={pick} />}
    </div>
  );
}

/** The dropdown, and the dialog behind it for an id that is not in it. */
function JobPicker({
  current, recent, onPick,
}: {
  current?: string;
  recent: string[];
  onPick: (id: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const options = useMemo(
    () => (current && !recent.includes(current) ? [current, ...recent] : recent),
    [current, recent],
  );

  return (
    <div className="flex items-center gap-2">
      {options.length > 0 && (
        <Select value={current ?? ""} onValueChange={onPick}>
          <SelectTrigger className="w-[22rem] font-mono text-xs" aria-label="Which job">
            <SelectValue placeholder="a job this browser has opened" />
          </SelectTrigger>
          <SelectContent>
            {options.map((id) => (
              <SelectItem key={id} value={id} className="font-mono text-xs">
                {id}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogTrigger asChild>
          <Button variant="outline" size="sm">
            <ChevronsUpDown className="size-4" />
            Another job
          </Button>
        </DialogTrigger>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Which job?</DialogTitle>
            <DialogDescription>
              The pipeline holds work in progress and not a catalogue, so there is no list to offer
              beyond the ones this browser has opened. Paste the id <code>CreateJob</code> returned.
            </DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(e: FormEvent<HTMLFormElement>) => {
              e.preventDefault();
              const id = new FormData(e.currentTarget).get("job");
              if (typeof id === "string" && id.trim()) {
                onPick(id);
                setOpen(false);
              }
            }}
            className="space-y-4"
          >
            <div className="space-y-2">
              <Label htmlFor="job">Job id</Label>
              <Input id="job" name="job" autoFocus spellCheck={false} className="font-mono text-xs" />
            </div>
            <DialogFooter>
              <Button type="submit">Open</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function NoJobYet({ onPick }: { onPick: (id: string) => void }) {
  const recent = recentJobs();
  if (recent.length === 0) {
    return (
      <Empty>
        No job open. Pick one with the button above — a job id is what <code>CreateJob</code>{" "}
        answered with, and it is also in the ledger entry of anything already loaded.
      </Empty>
    );
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Jobs this browser has opened</CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        {recent.map((id) => (
          <button
            key={id}
            onClick={() => onPick(id)}
            className="hover:bg-accent w-full rounded-md border px-3 py-2 text-left font-mono text-xs"
          >
            {id}
          </button>
        ))}
      </CardContent>
    </Card>
  );
}

/** One job's queue. */
function Queue({ jobId }: { jobId: string }) {
  const [answer, setAnswer] = useState<FindingsAnswer | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [answered, setAnswered] = useState<Record<string, ReviewVerb>>({});

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setAnswer(await api.get<FindingsAnswer>(`/v1/jobs/${encodeURIComponent(jobId)}/findings`));
      setError(null);
    } catch (e) {
      setError(e as Error);
    } finally {
      setLoading(false);
    }
  }, [jobId]);

  useEffect(() => {
    setAnswered({});
    void load();
  }, [load]);

  const send = useCallback(
    async (d: Decision, verb: ReviewVerb) => {
      const res = await api.post<DecideAnswer>(`/v1/jobs/${encodeURIComponent(jobId)}/decisions`, {
        decisions: [d],
      });
      // Marked from the server's answer and not from the click: a decision the
      // service rejected is not an answer, and a screen that ticked it anyway
      // would be the reviewer's record disagreeing with the store's.
      if ((res.rejected?.length ?? 0) === 0) {
        setAnswered((prev) => ({ ...prev, [d.item_id]: verb }));
      }
      await load();
      return res;
    },
    [jobId, load],
  );

  if (loading && !answer) return <Skeleton className="h-64 w-full" />;
  if (error) {
    if (error instanceof ApiError && error.isNotFound) {
      return (
        <Empty>
          No such job. A job lives about an hour and this one is not there any more, or the id is
          not one this pipeline issued.
        </Empty>
      );
    }
    return <LoadFailed what="This job's queue" error={error.message} />;
  }
  if (!answer) return null;

  // Answered means the server says so, with this session's own sends as the
  // optimistic half between a click and the reload after it. The map used to
  // be the only record, so a reviewer who reloaded the page got their answered
  // questions back looking untouched — a browser's private opinion of what the
  // store holds, which is the disagreement this product exists to prevent.
  const verbOf = (f: Finding): ReviewVerb | undefined =>
    answerVerb(f) ?? answered[f.id];
  const left = answer.items.filter((f) => !verbOf(f)).length;
  const held = answer.state === "JOB_STATE_NEEDS_REVIEW";

  return (
    <div className="space-y-4">
      <Alert variant={held ? "destructive" : "default"}>
        <AlertTitle className="flex items-center gap-2">
          {held ? <ClipboardCheck className="size-4" /> : <CheckCircle2 className="size-4" />}
          {STATE_LABEL[answer.state] ?? answer.state}
        </AlertTitle>
        <AlertDescription>
          {held
            ? `${left} of ${answer.items.length} unanswered. The graph is not delivered — and cannot be loaded into the brain — until every one has an answer.`
            : "Every question was answered and the graph was delivered. The answers below are kept so the record of them outlives the queue."}
        </AlertDescription>
      </Alert>

      <div className="flex items-center justify-between">
        <p className="text-muted-foreground font-mono text-xs">{answer.job_id}</p>
        <Button variant="ghost" size="sm" onClick={load} disabled={loading}>
          {loading ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
          Refresh
        </Button>
      </div>

      {answer.items.length === 0 ? (
        <Empty>Nothing to answer: this job&rsquo;s queue is empty.</Empty>
      ) : (
        <div className="space-y-3">
          {answer.items.map((f) => (
            <FindingCard key={f.id} finding={f} answeredAs={verbOf(f)} onSend={send} />
          ))}
        </div>
      )}
    </div>
  );
}

function FindingCard({
  finding, answeredAs, onSend,
}: {
  finding: Finding;
  answeredAs?: ReviewVerb;
  onSend: (d: Decision, verb: ReviewVerb) => Promise<DecideAnswer>;
}) {
  const kind = String(finding.kind).replace(/^REVIEW_KIND_/, "").toLowerCase();
  return (
    <Card className={answeredAs ? "opacity-60" : undefined}>
      <CardHeader className="gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant={kind === "conflict" || kind === "violation" ? "destructive" : "secondary"}>
            {KIND_LABEL[kind] ?? kind}
          </Badge>
          {answeredAs && (
            <Badge variant="outline" className="gap-1">
              <CheckCircle2 className="size-3" />
              {finding.answer
                ? `${answeredAs} · ${finding.answer.by}${finding.answer.at ? ` · ${new Date(finding.answer.at).toLocaleString()}` : ""}`
                : `answered · ${answeredAs}`}
            </Badge>
          )}
        </div>
        {/* The subject, which is the record an answer acts on — deliberately the
            heading, because the buttons underneath act on it and a heading that
            named anything else would be the panel and the verb disagreeing. */}
        <CardTitle className="font-mono text-sm leading-relaxed break-all">{finding.subject}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-muted-foreground text-sm leading-relaxed">{finding.summary}</p>
        {finding.provenance?.source && (
          <p className="text-muted-foreground text-xs">
            {finding.provenance.source}
            {finding.provenance.chunk !== undefined && ` · chunk ${finding.provenance.chunk}`}
            {finding.provenance.producer && ` · ${finding.provenance.producer.replace(/^PRODUCER_/, "").toLowerCase()}`}
            {finding.provenance.model && ` · ${finding.provenance.model}`}
          </p>
        )}
        {finding.answer?.note && (
          <p className="text-sm text-pretty">{finding.answer.note}</p>
        )}
        {!answeredAs && <Answers finding={finding} onSend={onSend} />}
      </CardContent>
    </Card>
  );
}

/** The three verbs, each behind a dialog that asks for the signature. */
function Answers({
  finding, onSend,
}: {
  finding: Finding;
  onSend: (d: Decision, verb: ReviewVerb) => Promise<DecideAnswer>;
}) {
  return (
    <div className="flex flex-wrap gap-2">
      <AnswerDialog
        verb="accept"
        finding={finding}
        onSend={onSend}
        icon={CheckCircle2}
        label="Keep it"
        blurb="The record stays exactly as it is and the answer names you — which is what the contract grades as verified. For a pair that may be one node, this is the answer that says they are two things."
      />
      <AnswerDialog
        verb="edit"
        finding={finding}
        onSend={onSend}
        icon={PencilLine}
        label="Correct it"
        blurb="Retype, rename, redirect — or merge, by naming the record this one is the same record as."
      />
      <AnswerDialog
        verb="reject"
        finding={finding}
        onSend={onSend}
        icon={XCircle}
        label="Take it out"
        blurb="The record is removed before the graph is delivered. Nothing else is touched."
      />
    </div>
  );
}

const VERB_WIRE = {
  accept: "REVIEW_VERB_ACCEPT",
  edit: "REVIEW_VERB_EDIT",
  reject: "REVIEW_VERB_REJECT",
} as const;

function AnswerDialog({
  verb, finding, onSend, icon: Icon, label, blurb,
}: {
  verb: ReviewVerb;
  finding: Finding;
  onSend: (d: Decision, verb: ReviewVerb) => Promise<DecideAnswer>;
  icon: typeof CheckCircle2;
  label: string;
  blurb: string;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = new FormData(e.currentTarget);
    const by = String(form.get("by") ?? "").trim();
    if (!by) return;
    const into = String(form.get("into") ?? "").trim();
    const note = String(form.get("note") ?? "").trim();
    setBusy(true);
    setFailed(null);
    try {
      const res = await onSend(
        {
          item_id: finding.id,
          verb: VERB_WIRE[verb],
          by,
          ...(note ? { note } : {}),
          ...(verb === "edit" && into ? { edit: { into } } : {}),
        },
        verb,
      );
      const refused = res.rejected?.[0];
      if (refused) {
        setFailed(refused.reason);
        return;
      }
      setOpen(false);
    } catch (e) {
      setFailed((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant={verb === "reject" ? "destructive" : verb === "accept" ? "default" : "outline"} size="sm">
          <Icon className="size-4" />
          {label}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{label}</DialogTitle>
          <DialogDescription>{blurb}</DialogDescription>
        </DialogHeader>
        <p className="bg-muted rounded-md p-3 font-mono text-xs break-all">{finding.subject}</p>
        <form onSubmit={submit} className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor={`by-${verb}-${finding.id}`}>
              Who — required; a judgement nobody signed is refused
            </Label>
            <Input id={`by-${verb}-${finding.id}`} name="by" autoFocus required />
          </div>
          {verb === "edit" && (
            <div className="space-y-2">
              <Label htmlFor={`into-${finding.id}`}>
                Into — the record this one is the same record as (merge)
              </Label>
              <Input id={`into-${finding.id}`} name="into" spellCheck={false} className="font-mono text-xs" />
            </div>
          )}
          <div className="space-y-2">
            <Label htmlFor={`note-${verb}-${finding.id}`}>Why</Label>
            <Input id={`note-${verb}-${finding.id}`} name="note" />
          </div>
          {failed && (
            <Alert variant="destructive">
              <AlertDescription>{failed}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button type="submit" disabled={busy}>
              {busy && <Loader2 className="size-4 animate-spin" />}
              Send decision
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
