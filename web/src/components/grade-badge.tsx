import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { GRADE_CLASS, GRADE_MEANING, type Grade } from "@/lib/types";

/**
 * A grade, everywhere it appears.
 *
 * One component so the shelf, the ledger and the plan cannot teach a reader
 * three different greens — the contract's ladder is the product's vocabulary,
 * and a reader who learns it once should read every screen faster.
 */
export function GradeBadge({ grade, className }: { grade: Grade | string; className?: string }) {
  const known = grade in GRADE_MEANING ? (grade as Grade) : undefined;
  return (
    <Badge
      variant="outline"
      title={known ? GRADE_MEANING[known] : "a grade the contract does not define"}
      className={cn("font-mono text-xs", known ? GRADE_CLASS[known] : "text-refused", className)}
    >
      {grade || "untagged"}
    </Badge>
  );
}
