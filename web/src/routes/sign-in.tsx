import { useState, type FormEvent } from "react";
import { Flame, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useSession } from "@/lib/session";
import { ApiError } from "@/lib/api";

/**
 * The first thing anybody sees.
 *
 * One field, because there is one credential: the key from the policy file.
 * The sentence under it is the product in a line — a buyer who lands here
 * without context should learn what this is before being asked for a secret.
 */
export function SignIn() {
  const { signIn, session } = useSession();
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await signIn(key);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "could not reach the server");
      setBusy(false);
    }
  }

  return (
    <div className="bg-background flex min-h-full items-center justify-center px-4 py-12">
      <div className="w-full max-w-sm">
        <div className="mb-8 flex flex-col items-center text-center">
          <div className="bg-primary/10 text-primary mb-4 flex size-11 items-center justify-center rounded-xl">
            <Flame className="size-5" aria-hidden />
          </div>
          <h1 className="text-2xl font-semibold tracking-tight">Athanor</h1>
          <p className="text-muted-foreground mt-2 text-sm leading-relaxed text-balance">
            Files go in under a vocabulary, a disagreement stops the job, a person answers it, and the
            answer survives into a store that can still say how it knows.
          </p>
        </div>

        <form onSubmit={submit} className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="key">Key</Label>
            <Input
              id="key"
              type="password"
              autoComplete="off"
              autoFocus
              placeholder="from the policy file"
              value={key}
              onChange={(e) => setKey(e.target.value)}
            />
          </div>
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <Button type="submit" className="w-full" disabled={busy || !key}>
            {busy && <Loader2 className="size-4 animate-spin" />}
            Enter
          </Button>
        </form>

        {session?.describe && (
          <p className="text-muted-foreground mt-8 text-center font-mono text-xs break-words opacity-70">
            {session.describe}
          </p>
        )}
      </div>
    </div>
  );
}
