/**
 * The shapes the Go side actually returns. Mirrored by hand from the structs
 * rather than generated, because there are few of them and a generator is a
 * second build step; when one of these drifts the compiler cannot catch it, so
 * a change to a Go response type has to change this file in the same commit.
 */

/** The knowledge contract's closed set. Every screen that colours something
 *  colours it by one of these, and nothing else in the product is saturated. */
export const GRADES = ["verified", "self_consistent", "asserted", "held", "refused", "untagged"] as const;
export type Grade = (typeof GRADES)[number];

/** What each rung means, in a sentence short enough to sit beside a number.
 *  Here rather than in a component so the shelf, the graph legend and the
 *  plan cannot teach three different meanings. */
export const GRADE_MEANING: Record<Grade, string> = {
  verified: "a named person kept it",
  self_consistent: "derived from something that stated it",
  asserted: "a model or a person said so; nobody checked",
  held: "waiting on a person",
  refused: "the vocabulary declined it",
  untagged: "no contract at all",
};

export const GRADE_CLASS: Record<Grade, string> = {
  verified: "text-verified",
  self_consistent: "text-self-consistent",
  asserted: "text-asserted",
  held: "text-held",
  refused: "text-refused",
  untagged: "text-untagged",
};

export interface Session {
  signed_in: boolean;
  actor?: string;
  clearance?: string;
  can_write: boolean;
  describe?: string;
  /** No key policy at all: show no sign-in form rather than one that would
   *  accept anything. */
  open: boolean;
}

export interface TallySlice {
  nodes: number;
  edges: number;
}
export interface ContractTally {
  verified: TallySlice;
  self_consistent: TallySlice;
  asserted: TallySlice;
  held: TallySlice;
  refused: TallySlice;
  untagged: TallySlice;
}

export interface GradedRecord {
  id: string;
  edge: boolean;
  type?: string;
  content?: string;
  from?: string;
  to?: string;
  grade: Grade;
  state?: string;
  why?: string;
  source?: string;
  producer?: string;
  at?: string;
}

export interface Shelf {
  tally?: ContractTally;
  tally_error?: string;
  attention?: GradedRecord[];
  attention_error?: string;
}

export interface DecisionRecord {
  id: string;
  kind: string;
  actor: string;
  verdict: string;
  subject?: string;
  note?: string;
  at: string;
}

/** A live-database plan: what is read, what is masked, what never leaves. */
export interface Treatment {
  table: string;
  column: string;
  type?: string;
  pii_kind: string;
  sensitivity: number;
  action: string;
  reason?: string;
  by?: string;
  /** Shown MASKED for a column the classifier calls personal: the sample
   *  exists so a person can check the guess, not so the value reaches a
   *  screen before anybody agreed it may leave the database. */
  sample?: string;
  enters?: string;
  scan?: boolean;
}

export interface PlanCounts {
  columns: number;
  personal: number;
  dropped: number;
  masked: number;
  generalized: number;
  hashed: number;
  redacted: number;
  passed: number;
  reversible: number;
  /** Kept prose with nothing looking inside it — this plan's quietest leak. */
  unscanned_text: number;
}

export interface PlanSource {
  driver: string;
  /** The connection string with the password gone. The credential itself is
   *  never in a response and never in this type. */
  redacted?: string;
  schema?: string;
  tables?: string[];
}

export type PlanState = "draft" | "signed" | "superseded";

export interface Plan {
  id: string;
  source_key: string;
  source: PlanSource;
  columns: Treatment[];
  /** What a signature names. Signing quotes it back, so a plan amended in
   *  another tab is refused rather than silently signed. */
  hash: string;
  state: PlanState;
  counts: PlanCounts;
  created_by?: string;
  created_at: string;
  signed_by?: string;
  signed_at?: string;
  supersedes?: string;
  note?: string;
}

export interface RunReport {
  id: string;
  plan: string;
  source_key: string;
  started_at: string;
  ended_at: string;
  dry_run?: boolean;
  rows_read: number;
  chunks: number;
  triples: number;
  skipped: number;
  /** Columns the database has that the signed plan does not name. They were
   *  dropped; a non-empty list means a re-signing is owed. */
  drift?: string[];
  gone?: string[];
  errors?: string[];
}

export interface Follow {
  id: string;
  plan: string;
  source_key: string;
  redacted?: string;
  started_at: string;
  stopped_at?: string;
  running: boolean;
  error?: string;
}
