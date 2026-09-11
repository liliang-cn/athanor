import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { ChevronRight, Search, X } from "lucide-react";
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
import { Skeleton } from "@/components/ui/skeleton";
import { Empty, LoadFailed } from "@/components/empty";
import { api } from "@/lib/api";
import type { DecisionRecord } from "@/lib/types";

/**
 * The ledger: what this server did, and who did it.
 *
 * The filter lives in the query string and nowhere else. That is the whole
 * reason this screen exists as a screen rather than as a scroll: a person who
 * has narrowed the ledger to one actor's live-database acts has found
 * something, and the thing they do next is send it to somebody. A filter held
 * in component state would make that link a link to the unfiltered ledger,
 * which is a different and much less useful claim.
 *
 * Only the first line of the note is shown. A ledger note is two lines by
 * construction (pkg/server/ledger.go): a sentence a person reads, then the
 * structured detail on its own line. The detail is exact and it is long, and a
 * list that rendered it would be a list of JSON with prose hidden inside it.
 * It belongs on the chain page, where there is room to label it.
 *
 * There is no grade here. A decision is `verified` by construction — a named
 * key signed it — so a column that reported that for every row would teach a
 * reader to ignore the one place grades vary, which is the premises.
 */

/** The three free-text filters, in the order the door documents them. Free
 *  text and not a Select, because a kind is whatever the act that recorded
 *  itself called itself: Athanor writes six of them and an agent recording
 *  through decision_record may write any word at all. */
const FILTERS = [
  { name: "kind", label: "Kind", placeholder: "review, load, livedb.run" },
  { name: "subject", label: "Subject", placeholder: "a node id" },
  { name: "actor", label: "Actor", placeholder: "a key id" },
] as const;

/** What the door will actually honour: ledgerDefaultLimit up to ledgerMaxLimit. */
const LIMITS = ["20", "50", "100", "200"];
const DEFAULT_LIMIT = "20";

interface Draft {
  kind: string;
  subject: string;
  actor: string;
  limit: string;
}

function draftFrom(params: URLSearchParams): Draft {
  return {
    kind: params.get("kind") ?? "",
    subject: params.get("subject") ?? "",
    actor: params.get("actor") ?? "",
    limit: params.get("limit") ?? DEFAULT_LIMIT,
  };
}

/** The question, as the server takes it. Empty fields are left out rather than
 *  sent blank, so that two spellings of "no filter" are one request. */
function queryFrom(draft: Draft): string {
  const q = new URLSearchParams();
  for (const { name } of FILTERS) {
    const value = draft[name].trim();
    if (value) q.set(name, value);
  }
  if (draft.limit && draft.limit !== DEFAULT_LIMIT) q.set("limit", draft.limit);
  const s = q.toString();
  return s ? `?${s}` : "";
}

/** Whether anything is narrowing the ledger. The limit is not a filter: a
 *  ledger showing its twenty newest entries and holding nothing are different
 *  answers, and only the three named fields can turn one into the other. */
function isFiltered(draft: Draft): boolean {
  return FILTERS.some(({ name }) => draft[name].trim() !== "");
}

/** The sentence a person reads. See the file comment on why the rest is not
 *  here. */
function lead(note?: string): string {
  const text = (note ?? "").trim();
  const nl = text.indexOf("\n");
  return nl < 0 ? text : text.slice(0, nl).trim();
}

/** When, in the reader's own zone, falling back to what the server said rather
 *  than to "Invalid Date" — a ledger that cannot render a timestamp should
 *  show the timestamp. */
function when(at?: string): string {
  if (!at) return "";
  const d = new Date(at);
  return Number.isNaN(d.getTime()) ? at : d.toLocaleString();
}

/** A decision id goes into the path whole. Segment by segment, because an id
 *  is a caller's own string — a load is named by whoever ran it — and the one
 *  character that would otherwise change the route's meaning is a slash. */
export function decisionPath(id: string): string {
  return `/decisions/${id.split("/").map(encodeURIComponent).join("/")}`;
}

interface LedgerPage {
  decisions: DecisionRecord[];
  count: number;
}

export function Decisions() {
  const [params, setParams] = useSearchParams();
  const [draft, setDraft] = useState<Draft>(() => draftFrom(params));
  const [page, setPage] = useState<LedgerPage | null>(null);
  const [error, setError] = useState<string | null>(null);

  // The URL is the state; the inputs are a draft of the next one. A link
  // somebody opened, or the back button, has to win over whatever is typed.
  const query = queryFrom(draftFrom(params));
  useEffect(() => {
    setDraft(draftFrom(new URLSearchParams(query)));
  }, [query]);

  useEffect(() => {
    let live = true;
    setPage(null);
    setError(null);
    api
      .get<LedgerPage>(`/athanor/decisions${query}`)
      .then((p) => live && setPage(p))
      .catch((e: Error) => live && setError(e.message));
    return () => {
      live = false;
    };
  }, [query]);

  const commit = (next: Draft) => {
    const q = queryFrom(next);
    setParams(new URLSearchParams(q.slice(1)), { replace: false });
  };

  const filtered = isFiltered(draftFrom(params));
  const entries = page?.decisions ?? [];

  return (
    <div className="space-y-6">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Decisions</h1>
        <p className="text-muted-foreground mt-1 text-sm text-pretty">
          Every act this server performed, signed by the key that performed it. Nothing writes an
          entry here: an entry is written by doing the thing it describes.
        </p>
      </header>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Narrow it</CardTitle>
        </CardHeader>
        <CardContent>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              commit(draft);
            }}
            className="space-y-4"
          >
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              {FILTERS.map(({ name, label, placeholder }) => (
                <div key={name} className="space-y-1.5">
                  <Label htmlFor={`filter-${name}`}>{label}</Label>
                  <Input
                    id={`filter-${name}`}
                    value={draft[name]}
                    placeholder={placeholder}
                    autoComplete="off"
                    spellCheck={false}
                    onChange={(e) => setDraft({ ...draft, [name]: e.target.value })}
                  />
                </div>
              ))}
              <div className="space-y-1.5">
                <Label htmlFor="filter-limit">Most</Label>
                <Select
                  value={draft.limit}
                  onValueChange={(limit) => {
                    const next = { ...draft, limit };
                    setDraft(next);
                    commit(next);
                  }}
                >
                  <SelectTrigger id="filter-limit" className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {LIMITS.map((n) => (
                      <SelectItem key={n} value={n}>
                        {n} entries
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Button type="submit">
                <Search className="size-4" aria-hidden /> Apply
              </Button>
              {filtered && (
                <Button
                  type="button"
                  variant="ghost"
                  onClick={() => commit({ kind: "", subject: "", actor: "", limit: draft.limit })}
                >
                  <X className="size-4" aria-hidden /> Clear
                </Button>
              )}
              <p className="text-muted-foreground ml-auto text-xs">
                This view is its own link — the filter is in the address bar.
              </p>
            </div>
          </form>
        </CardContent>
      </Card>

      {error && <LoadFailed what="The ledger" error={error} />}

      {!page && !error && (
        <div className="space-y-2">
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-24 w-full" />
        </div>
      )}

      {page && !entries.length && !filtered && (
        <Empty>
          The ledger holds nothing yet. It fills as this server acts — a load, a review decision, a
          vocabulary published, a plan signed and run.
        </Empty>
      )}
      {page && !entries.length && filtered && (
        <Empty>No entry matches that filter. The ledger may still hold others.</Empty>
      )}

      {entries.length > 0 && (
        <div className="space-y-2">
          <p className="text-muted-foreground text-xs">
            {entries.length} {entries.length === 1 ? "entry" : "entries"}, newest first.
          </p>
          {entries.map((d) => (
            <Link
              key={d.id}
              to={decisionPath(d.id)}
              className="hover:bg-accent/50 focus-visible:ring-ring block rounded-lg border p-3 transition-colors focus-visible:ring-2 focus-visible:outline-none"
            >
              <div className="flex items-start gap-3">
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
                    {lead(d.note) || <span className="text-muted-foreground">no note</span>}
                  </p>
                  <div className="text-muted-foreground mt-1.5 flex flex-col gap-0.5 text-xs sm:flex-row sm:flex-wrap sm:gap-x-3">
                    <span className="break-words">
                      by <span className="font-mono">{d.actor || "nobody"}</span>
                    </span>
                    <span className="tabular-nums">
                      <time dateTime={d.at}>{when(d.at)}</time>
                    </span>
                  </div>
                </div>
                <ChevronRight
                  className="text-muted-foreground mt-0.5 size-4 shrink-0"
                  aria-hidden
                />
              </div>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
