import { cn } from "@/lib/utils";

/**
 * The difference between "nothing is here" and "I could not ask".
 *
 * A screen whose query failed must not render as empty: telling a reader the
 * brain holds nothing of a kind is a different and worse claim than admitting
 * the question did not get through. Every list on every screen uses this, and
 * the `error` form is visibly not the `empty` form.
 */
export function Empty({ children, className }: { children: React.ReactNode; className?: string }) {
  return <p className={cn("text-muted-foreground py-6 text-sm", className)}>{children}</p>;
}

export function LoadFailed({ what, error }: { what: string; error: string }) {
  return (
    <p className="text-destructive py-6 text-sm">
      {what} could not be read. <span className="font-mono text-xs opacity-80">{error}</span>
    </p>
  );
}
