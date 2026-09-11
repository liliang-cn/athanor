import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { Loader2 } from "lucide-react";
import "@/index.css";
import { SessionProvider, useSession } from "@/lib/session";
import { Layout } from "@/components/layout";
import { SignIn } from "@/routes/sign-in";
import { Shelf } from "@/routes/shelf";
import { Decisions } from "@/routes/decisions";
import { DecisionChain } from "@/routes/decision-chain";
import { Import } from "@/routes/import";
import { LiveDb } from "@/routes/livedb";
import { Runs } from "@/routes/runs";
import { Follows } from "@/routes/follows";

/**
 * Nothing behind the door is rendered until the session is known: a flash of
 * the sign-in form for somebody already signed in reads as being logged out,
 * which is alarming in a product that holds an audit trail.
 */
function App() {
  const { session, loading } = useSession();
  if (loading) {
    return (
      <div className="text-muted-foreground flex min-h-full items-center justify-center">
        <Loader2 className="size-5 animate-spin" />
      </div>
    );
  }
  if (!session?.signed_in) return <SignIn />;
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Shelf />} />
        <Route path="decisions" element={<Decisions />} />
        <Route path="decisions/*" element={<DecisionChain />} />
        <Route path="import" element={<Import />} />
        <Route path="import/livedb" element={<LiveDb />} />
        <Route path="import/runs" element={<Runs />} />
        <Route path="import/follows" element={<Follows />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <SessionProvider>
        <App />
      </SessionProvider>
    </BrowserRouter>
  </StrictMode>,
);
