import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { BookMarked, Check, Copy, Loader2, RefreshCw, Send } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Empty, LoadFailed } from "@/components/empty";
import { api } from "@/lib/api";
import type { OntologyList, OntologyState, OntologyVersion, Proposal } from "@/lib/types";

/**
 * The vocabulary this brain was extracted under, and how it got that way.
 *
 * This screen replaces a link. The nav pointed at CortexDB's own ontology page
 * — a real page, over a real store, and the wrong one: CortexDB keeps its
 * ontologies in tables Athanor never writes, so a brain with five published
 * versions of a vocabulary in it opened a screen that said no ontology was
 * saved. Nothing was broken and nothing was missing; the question was being
 * asked of a store that had never been told the answer.
 *
 * What Athanor has instead is a workflow, and a workflow wants to be seen as
 * one: draft → propose → approve → publish, each act signed, each version
 * keeping the one before it because every graph extracted under a version
 * names it in its provenance. So the versions are a column in order, the
 * published one is opened, and the two acts a person can take from here are
 * the two that need a person — approving what a run asked for, and putting a
 * version in force.
 */

const STATE_STYLE: Record<OntologyState, string> = {
  draft: "bg-muted text-muted-foreground",
  proposed: "bg-held/15 text-held",
  approved: "bg-self-consistent/15 text-self-consistent",
  published: "bg-verified/15 text-verified",
  retired: "bg-muted text-muted-foreground line-through",
};

const STATE_MEANING: Record<OntologyState, string> = {
  draft: "written, never in force",
  proposed: "a run named types this version lacks",
  approved: "somebody accepted those types; not in force yet",
  published: "what a job is extracted under right now",
  retired: "was in force; kept because graphs cite it",
};

const KIND_LABEL: Record<string, string> = {
  entity: "a kind of thing",
  relation: "a kind of link",
  relation_ends: "a link between ends it does not allow",
  attribute: "a field the type does not have",
};

export function Ontology() {
  const [versions, setVersions] = useState<OntologyVersion[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [lineage, setLineage] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const answer = await api.get<OntologyList>("/athanor/ontologies");
      setVersions(answer.versions ?? []);
      setError(null);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const lineages = useMemo(() => {
    const seen = new Set((versions ?? []).map((v) => v.lineage));
    return [...seen].sort();
  }, [versions]);

  // The lineage in force is the one worth opening on, and the first one is
  // only a fallback for a store where nothing has been published yet.
  const current =
    lineage && lineages.includes(lineage)
      ? lineage
      : ((versions ?? []).find((v) => v.state === "published")?.lineage ?? lineages[0] ?? null);

  const shown = useMemo(
    () => (versions ?? []).filter((v) => v.lineage === current),
    [versions, current],
  );

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Ontology</h1>
          <p className="text-muted-foreground mt-1 max-w-2xl text-sm text-pretty">
            The closed vocabulary a job is extracted under. A run that meets a type this does not
            declare says so instead of inventing one, and what it asks for arrives here as a
            proposal somebody has to accept by name.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {lineages.length > 0 && (
            <Select value={current ?? ""} onValueChange={setLineage}>
              <SelectTrigger className="w-56" aria-label="Which vocabulary">
                <SelectValue placeholder="a vocabulary" />
              </SelectTrigger>
              <SelectContent>
                {lineages.map((l) => (
                  <SelectItem key={l} value={l}>
                    {l}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          <Button variant="ghost" size="sm" onClick={load} disabled={loading}>
            {loading ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
            Refresh
          </Button>
        </div>
      </div>

      {error && <LoadFailed what="The vocabularies" error={error} />}
      {loading && !versions && <Skeleton className="h-64 w-full" />}
      {versions && !error && versions.length === 0 && (
        <Empty>
          No vocabulary is saved here yet. One arrives the first time a job is created with an
          ontology document, or by POSTing that document to <code>/athanor/ontologies</code>.
        </Empty>
      )}

      {current && shown.length > 0 && <Lineage versions={shown} onChanged={load} />}
    </div>
  );
}

/** One lineage: what is in force, and every version behind it. */
function Lineage({ versions, onChanged }: { versions: OntologyVersion[]; onChanged: () => void }) {
  // Newest first. created_at is the only ordering that holds across every
  // state: an id's version half increments on approval, so a draft written
  // after a publication sorts before it by name and after it by clock.
  const ordered = useMemo(
    () => [...versions].sort((a, b) => b.created_at.localeCompare(a.created_at)),
    [versions],
  );
  const inForce = ordered.find((v) => v.state === "published");

  return (
    <div className="space-y-6">
      {inForce ? (
        <Document version={inForce} />
      ) : (
        <Empty>
          Nothing in this lineage is in force. A job naming it will be refused until one of the
          versions below is published.
        </Empty>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Every version, newest first</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          {ordered.map((v) => (
            <VersionRow key={v.id} version={v} onChanged={onChanged} />
          ))}
        </CardContent>
      </Card>
    </div>
  );
}

/** What the vocabulary in force actually says, and the bytes to paste. */
function Document({ version }: { version: OntologyVersion }) {
  const [copied, setCopied] = useState(false);
  const raw = useMemo(() => JSON.stringify(version.document, null, 2), [version.document]);
  const parts = useMemo(() => readDocument(version.document), [version.document]);

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-3 space-y-0">
        <CardTitle className="flex items-center gap-2 text-base">
          <BookMarked className="size-4" />
          <span className="font-mono">{version.id}</span>
          <StateBadge state={version.state} />
        </CardTitle>
        <Button
          variant="outline"
          size="sm"
          onClick={() => {
            // Clipboard access is denied often enough — an insecure origin, a
            // browser that asks — that the failure has to be visible rather
            // than a button that silently does nothing.
            navigator.clipboard
              .writeText(raw)
              .then(() => setCopied(true))
              .catch(() => setCopied(false));
          }}
        >
          {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
          {copied ? "Copied" : "Copy JSON"}
        </Button>
      </CardHeader>
      <CardContent className="space-y-4">
        {parts.length === 0 ? (
          <pre className="bg-muted max-h-80 overflow-auto rounded-md p-3 font-mono text-xs">
            {raw}
          </pre>
        ) : (
          parts.map((part) => (
            <div key={part.name} className="space-y-2">
              {parts.length > 1 && (
                <p className="text-muted-foreground text-xs tracking-wide uppercase">{part.name}</p>
              )}
              <div className="flex flex-wrap gap-1.5">
                {part.entities.map((t) => (
                  <Badge key={t} variant="secondary" className="font-mono text-xs">
                    {t}
                  </Badge>
                ))}
              </div>
              <div className="space-y-1">
                {part.relations.map((r) => (
                  <p key={r.name} className="font-mono text-xs break-words">
                    <span className="text-muted-foreground">{r.from.join(" | ") || "?"}</span>
                    {" —"}
                    {r.name}
                    {"→ "}
                    <span className="text-muted-foreground">{r.to.join(" | ") || "?"}</span>
                  </p>
                ))}
              </div>
            </div>
          ))
        )}
      </CardContent>
    </Card>
  );
}

function VersionRow({ version, onChanged }: { version: OntologyVersion; onChanged: () => void }) {
  const proposals = version.proposals ?? [];
  return (
    <div className="space-y-3 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm">{version.id}</span>
        <StateBadge state={version.state} />
        <span className="text-muted-foreground flex-1 text-xs">
          {version.parent ? `from ${version.parent}` : ""}
        </span>
        <Act version={version} onChanged={onChanged} />
      </div>

      <p className="text-muted-foreground text-xs">
        {signature("drafted", version.created_by, version.created_at)}
        {version.approved_at && ` · ${signature("approved", version.approved_by, version.approved_at)}`}
        {version.published_at && ` · ${signature("published", undefined, version.published_at)}`}
        {version.retired_at && ` · ${signature("retired", undefined, version.retired_at)}`}
      </p>
      {version.note && <p className="text-sm text-pretty">{version.note}</p>}

      {proposals.length > 0 && (
        <div className="space-y-1.5 border-l-2 pl-3">
          <p className="text-muted-foreground text-xs">
            {proposals.length} asked for by{" "}
            <span className="font-mono">{version.proposed_from ?? "a run"}</span>
          </p>
          {proposals.map((p) => (
            <ProposalLine key={`${p.kind}:${p.type}`} proposal={p} />
          ))}
        </div>
      )}
    </div>
  );
}

function ProposalLine({ proposal: p }: { proposal: Proposal }) {
  const widening = p.kind === "relation_ends";
  return (
    <p className="text-xs break-words">
      <span className="font-mono">{p.type}</span>
      <span className="text-muted-foreground"> — {KIND_LABEL[p.kind] ?? p.kind}</span>
      {widening && p.declared_to && (
        <span className="text-muted-foreground">
          {": declared "}
          <span className="font-mono">{p.declared_to.join(" | ")}</span>
          {", used "}
          <span className="font-mono">{(p.to ?? []).join(" | ")}</span>
        </span>
      )}
      {!widening && p.kind === "attribute" && p.from?.length ? (
        // An attribute is stated about one type and has no far end. Rendering
        // it with the relation's arrow drew "City →" with nothing after it,
        // which reads as a link somebody forgot to finish.
        <span className="text-muted-foreground">
          {" on "}
          <span className="font-mono">{p.from.join(" | ")}</span>
        </span>
      ) : null}
      {!widening && p.kind !== "attribute" && (p.from?.length || p.to?.length) ? (
        <span className="text-muted-foreground font-mono">
          {" "}
          {(p.from ?? []).join(" | ")} → {(p.to ?? []).join(" | ")}
        </span>
      ) : null}
      <span className="text-muted-foreground"> · {p.records} record{p.records === 1 ? "" : "s"}</span>
    </p>
  );
}

/**
 * The two acts that need a person, and nothing else.
 *
 * Drafting and proposing are not here on purpose. A draft is a document
 * somebody wrote elsewhere and a proposal is what a finished run observed —
 * neither is a judgement, and neither is made by clicking. Approving is a
 * judgement about what a word means and publishing puts a vocabulary in force
 * over every job after it, so both are signed, and approving is signed by name
 * rather than by whoever holds the key.
 */
function Act({ version, onChanged }: { version: OntologyVersion; onChanged: () => void }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);
  const proposals = version.proposals ?? [];
  const canApprove = version.state === "proposed" && proposals.length > 0;
  const canPublish = version.state === "approved" || version.state === "draft";

  if (!canApprove && !canPublish) return null;

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = new FormData(e.currentTarget);
    const by = String(form.get("by") ?? "").trim();
    if (!by) return;
    const note = String(form.get("note") ?? "").trim();
    setBusy(true);
    setFailed(null);
    try {
      if (canApprove) {
        const accept = proposals.map((p) => p.type).filter((t) => form.get(`accept:${t}`) === "on");
        await api.post(`/athanor/ontologies/${encodeURIComponent(version.id)}:approve`, {
          accept, by, note,
        });
      } else {
        await api.post(`/athanor/ontologies/${encodeURIComponent(version.id)}:publish`, { by, note });
      }
      setOpen(false);
      onChanged();
    } catch (err) {
      setFailed((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) setFailed(null);
      }}
    >
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        <Send className="size-4" />
        {canApprove ? "Approve" : "Publish"}
      </Button>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {canApprove ? "Accept which of these?" : "Put this vocabulary in force?"}
          </DialogTitle>
          <DialogDescription>
            {canApprove
              ? "Each one is a type a run met and this vocabulary does not declare. Accepting writes a new version declaring the ones you tick; the rest stay refused, and a later run will ask again."
              : "Every job created after this is extracted under it, and the version it replaces is retired rather than deleted — graphs already in the brain name it in their provenance."}
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-4">
          {canApprove && (
            <div className="max-h-60 space-y-2 overflow-auto">
              {proposals.map((p) => (
                <label
                  key={p.type}
                  className="hover:bg-accent flex items-start gap-2 rounded-md border p-2 text-sm"
                >
                  <input type="checkbox" name={`accept:${p.type}`} className="mt-1" />
                  <span className="min-w-0 flex-1">
                    <ProposalLine proposal={p} />
                  </span>
                </label>
              ))}
            </div>
          )}
          <div className="space-y-2">
            <Label htmlFor="by">Who decided</Label>
            <Input id="by" name="by" autoFocus required placeholder="a person's name" />
            <p className="text-muted-foreground text-xs">
              {canApprove
                ? "In the record for as long as the vocabulary lasts. “Whoever held the operator key” is not an answer to who decided what a type means."
                : "Publishing is an operational act; the key's own id is used if this is left empty."}
            </p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="note">Why</Label>
            <Input id="note" name="note" placeholder="optional" />
          </div>
          {failed && <p className="text-destructive text-sm break-words">{failed}</p>}
          <DialogFooter>
            <Button type="submit" disabled={busy}>
              {busy && <Loader2 className="size-4 animate-spin" />}
              {canApprove ? "Approve" : "Publish"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function StateBadge({ state }: { state: OntologyState }) {
  return (
    <span
      title={STATE_MEANING[state]}
      className={`rounded px-1.5 py-0.5 text-xs font-medium ${STATE_STYLE[state] ?? ""}`}
    >
      {state}
    </span>
  );
}

function signature(verb: string, who: string | undefined, at: string) {
  const when = at ? new Date(at).toLocaleString() : "";
  return who ? `${verb} by ${who} ${when}` : `${verb} ${when}`;
}

/**
 * Read the document well enough to show it, and give up visibly rather than
 * guess.
 *
 * The ontology document is alchemy's shape and this file does not own it, so
 * anything unrecognised falls back to the raw JSON above — a screen that
 * rendered half of a vocabulary as if it were all of it would be worse than
 * one that shows the text.
 */
type ReadPart = {
  name: string;
  entities: string[];
  relations: { name: string; from: string[]; to: string[] }[];
};

function readDocument(doc: unknown): ReadPart[] {
  if (!doc || typeof doc !== "object") return [];
  const root = doc as Record<string, unknown>;
  const parts = root.parts;
  if (parts && typeof parts === "object" && !Array.isArray(parts)) {
    return Object.entries(parts as Record<string, unknown>)
      .map(([name, body]) => readPart(name, body))
      .filter((p): p is ReadPart => p !== null);
  }
  const one = readPart("", root);
  return one ? [one] : [];
}

function readPart(name: string, body: unknown): ReadPart | null {
  if (!body || typeof body !== "object") return null;
  const b = body as Record<string, unknown>;
  const entities = names(b.entities);
  const relations = Array.isArray(b.relations)
    ? b.relations
        .map((r) => {
          if (!r || typeof r !== "object") return null;
          const rel = r as Record<string, unknown>;
          const n = typeof rel.name === "string" ? rel.name : typeof rel.type === "string" ? rel.type : "";
          if (!n) return null;
          return { name: n, from: names(rel.from), to: names(rel.to) };
        })
        .filter((r): r is { name: string; from: string[]; to: string[] } => r !== null)
    : [];
  if (entities.length === 0 && relations.length === 0) return null;
  return { name: name || "types", entities, relations };
}

/** A list of type names, however the document spells them: strings, or objects
 *  with a name. */
function names(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  return v
    .map((x) => {
      if (typeof x === "string") return x;
      if (x && typeof x === "object") {
        const n = (x as Record<string, unknown>).name;
        if (typeof n === "string") return n;
      }
      return "";
    })
    .filter(Boolean);
}
