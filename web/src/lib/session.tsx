import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { api } from "@/lib/api";
import type { Session } from "@/lib/types";

/**
 * Who is using this browser.
 *
 * Read once at startup and kept, because every screen needs it to decide
 * whether to offer a write at all: a read-only key should not be shown a Sign
 * button that will come back 403. The server still refuses independently —
 * this is what the interface shows, never what decides.
 */
interface SessionState {
  session: Session | null;
  loading: boolean;
  signIn: (key: string) => Promise<void>;
  signOut: () => Promise<void>;
}

const Ctx = createContext<SessionState | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api
      .get<Session>("/api/session")
      .then(setSession)
      .catch(() => setSession(null))
      .finally(() => setLoading(false));
  }, []);

  const signIn = useCallback(async (key: string) => {
    setSession(await api.post<Session>("/api/session", { key }));
  }, []);

  const signOut = useCallback(async () => {
    setSession(await api.del<Session>("/api/session"));
  }, []);

  return <Ctx.Provider value={{ session, loading, signIn, signOut }}>{children}</Ctx.Provider>;
}

export function useSession() {
  const v = useContext(Ctx);
  if (!v) throw new Error("useSession outside SessionProvider");
  return v;
}
