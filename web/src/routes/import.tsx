import { Link } from "react-router-dom";
import { ArrowRight, Braces, Database, FileText, Table2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

/**
 * Where a graph can come from, ordered by how checkable the result is.
 *
 * The ordering is the argument. A schema and an existing graph are read by
 * code: the same input gives the same triples every time, and a person
 * checking the output is checking a transformation. Prose is read by a model:
 * the same paragraph can give two different answers, and a person checking
 * the output is checking a claim. A table sits between them — deterministic
 * if you say how the rows map, a model's guess if you decline to.
 *
 * So every card says whether a model is called, in the same place, in the
 * same words. That difference is the product's whole case and a chooser that
 * buried it would be selling the wrong thing.
 *
 * Only the live-database path has a screen. The rest are reachable over the
 * API today, and each card says how — except where it is not: CortexDB's
 * importflow toolbox (DDL and CSV) is a library capability this server does
 * not mount, and a card that offered a route for it would be offering a 404.
 * Saying "not on this server" is worth more than a link that lies.
 */

interface Path {
  icon: typeof Database;
  title: string;
  /** Whether a model is called, and what that costs in checkability. */
  model: string;
  determinism: "no model" | "a model, conditionally" | "a model per chunk";
  what: string;
  /** The screen, when there is one. */
  to?: string;
  cta?: string;
  /** The API route, when the capability is mounted on this server. */
  endpoint?: string[];
  /** Why there is no route to name. */
  absent?: string;
}

const PATHS: Path[] = [
  {
    icon: Database,
    title: "A database schema (DDL)",
    determinism: "no model",
    model: "No model is called. Tables become classes, columns become properties, foreign keys become relations — a reading of the DDL, not an opinion about it.",
    what: "The most checkable import there is: run it twice on the same schema and the same triples come out, and a reviewer checks a transformation rather than a claim.",
    absent:
      "This server does not mount CortexDB's importflow toolbox, so there is no endpoint here to name. The capability is in the library; the door is not built.",
  },
  {
    icon: Braces,
    title: "An existing graph",
    determinism: "no model",
    model: "No model is called. Triples in, triples out, in a namespace you name.",
    what: "Turtle, N-Triples or JSON-LD somebody else already curated. Nothing is inferred, so nothing can be inferred wrongly.",
    endpoint: ["POST /brain/v1/tools/knowledge_graph_import"],
  },
  {
    icon: Table2,
    title: "A table (CSV, or a live database)",
    determinism: "a model, conditionally",
    model: "A model only if you decline to state the mapping. Say which column is the identity and which are edges and nothing is guessed; leave it unsaid and something has to guess.",
    what: "Rows become chunks and triples. A live database adds the part a CSV does not need: the rows are somebody else's, still in their database, and this screen does not read one until a person has signed for what leaves it.",
    to: "/import/livedb",
    cta: "Import from a live database",
    absent:
      "The CSV half of this path is importflow's, and this server does not mount that toolbox. The live-database half is built, and it is the button above.",
  },
  {
    icon: FileText,
    title: "Prose",
    determinism: "a model per chunk",
    model: "A model per chunk, and every triple it produces is asserted until somebody checks it.",
    what: "It cannot run without a vocabulary: an extractor with no ontology invents its own predicates and the graph becomes a pile of synonyms. Publish one first, then load.",
    endpoint: ["POST /v1/sources", "POST /v1/jobs", "GET /athanor/loads"],
  },
];

export function Import() {
  return (
    <div className="space-y-6">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Import</h1>
        <p className="text-muted-foreground mt-1 text-sm text-pretty">
          Four ways into the graph, in order of how checkable the result is. Each one says whether a
          model is called, because that is the difference between checking a transformation and
          checking a claim.
        </p>
      </header>

      <div className="grid gap-4 lg:grid-cols-2">
        {PATHS.map(({ icon: Icon, title, determinism, model, what, to, cta, endpoint, absent }) => (
          <Card key={title} className="flex flex-col">
            <CardHeader>
              <CardTitle className="flex items-start gap-2 text-base">
                <Icon className="text-muted-foreground mt-0.5 size-4 shrink-0" aria-hidden />
                <span className="min-w-0 text-pretty">{title}</span>
              </CardTitle>
              <Badge variant={determinism === "no model" ? "secondary" : "outline"}>
                {determinism}
              </Badge>
            </CardHeader>
            <CardContent className="flex flex-1 flex-col gap-3">
              <p className="text-sm text-pretty">{model}</p>
              <p className="text-muted-foreground text-sm text-pretty">{what}</p>

              <div className="mt-auto space-y-3">
                {to && (
                  <Button asChild>
                    <Link to={to}>
                      {cta} <ArrowRight className="size-4" />
                    </Link>
                  </Button>
                )}

                {endpoint && (
                  <div className="space-y-1 border-t pt-3">
                    <p className="text-muted-foreground text-xs">
                      No screen yet. Reachable over the API today:
                    </p>
                    {endpoint.map((e) => (
                      <p key={e} className="font-mono text-xs break-all">
                        {e}
                      </p>
                    ))}
                  </div>
                )}

                {absent && (
                  <p className="text-muted-foreground border-t pt-3 text-xs text-pretty">{absent}</p>
                )}
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  );
}
