import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { GradeBadge } from "@/components/grade-badge";
import { Empty, LoadFailed } from "@/components/empty";
import { api } from "@/lib/api";
import { GRADES, GRADE_MEANING, type Shelf as ShelfData } from "@/lib/types";

/**
 * What the brain holds, and how much of it anybody checked.
 *
 * The reference screen: fetch once, render one flat view, and show a failed
 * question as failed rather than as empty. The numbers lead because the
 * product's claim is that it can report its own quality including the bad
 * parts — a tally that hid `refused` would be marketing.
 */
export function Shelf() {
  const [data, setData] = useState<ShelfData | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .get<ShelfData>("/api/shelf")
      .then(setData)
      .catch((e: Error) => setError(e.message));
  }, []);

  return (
    <div className="space-y-6">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Shelf</h1>
        <p className="text-muted-foreground mt-1 text-sm text-pretty">
          Every record carries which file, which chunk and which producer it came from, and a grade.
          This is the count, including the bad ones.
        </p>
      </header>

      {error && <LoadFailed what="The shelf" error={error} />}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">What the shelf stands on</CardTitle>
        </CardHeader>
        <CardContent>
          {!data && !error && <Skeleton className="h-40 w-full" />}
          {data?.tally_error && <LoadFailed what="The tally" error={data.tally_error} />}
          {data?.tally && (
            <div className="grid gap-2 sm:grid-cols-2">
              {GRADES.map((g) => {
                const slice = data.tally![g];
                return (
                  <div key={g} className="flex items-baseline justify-between gap-3 rounded-lg border p-3">
                    <div className="min-w-0">
                      <GradeBadge grade={g} />
                      <p className="text-muted-foreground mt-1.5 text-xs text-pretty">{GRADE_MEANING[g]}</p>
                    </div>
                    <div className="shrink-0 text-right tabular-nums">
                      <div className="text-lg leading-none font-medium">{slice?.nodes ?? 0}</div>
                      <div className="text-muted-foreground mt-1 text-xs">{slice?.edges ?? 0} edges</div>
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Wants a person</CardTitle>
        </CardHeader>
        <CardContent>
          {!data && !error && <Skeleton className="h-20 w-full" />}
          {data?.attention_error && <LoadFailed what="This list" error={data.attention_error} />}
          {data && !data.attention_error && !data.attention?.length && (
            <Empty>Nothing is held or refused.</Empty>
          )}
          <div className="space-y-2">
            {data?.attention?.map((r) => (
              <div key={r.id} className="rounded-lg border p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <GradeBadge grade={r.grade} />
                  <span className="min-w-0 flex-1 text-sm break-words">
                    {r.edge ? `${r.from} —${r.type}→ ${r.to}` : r.content || r.id}
                  </span>
                </div>
                {r.why && <p className="text-held mt-1.5 text-sm text-pretty">{r.why}</p>}
                {r.source && (
                  <p className="text-muted-foreground mt-1 font-mono text-xs break-words">{r.source}</p>
                )}
              </div>
            ))}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
