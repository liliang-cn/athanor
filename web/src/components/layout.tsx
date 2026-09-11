import { NavLink, Outlet } from "react-router-dom";
import { Boxes, ClipboardCheck, Download, Network, BookMarked, Gavel, LogOut, Moon, Sun } from "lucide-react";
import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { useSession } from "@/lib/session";
import { cn } from "@/lib/utils";

/**
 * The shell.
 *
 * A rail on a desk and a bar under a thumb, from one list — six destinations
 * do not fit a readable top strip at 390px, and a strip that scrolls hides
 * destinations behind an affordance nobody finds. Sign out is deliberately
 * not in the bar: the control that ends a session should not sit a mis-tap
 * away from the one used to move between screens.
 *
 * Two of these are not Athanor's own screens — Review is alchemy's queue and
 * Graph is CortexDB's live view — and they are in the same list because a
 * person navigating a product does not care which repository drew the page.
 */
const NAV = [
  { to: "/", label: "Shelf", icon: Boxes, end: true },
  { to: "/ui/", label: "Review", icon: ClipboardCheck, external: true },
  { to: "/import", label: "Import", icon: Download },
  { to: "/graph/", label: "Graph", icon: Network, external: true },
  { to: "/graph/ontology", label: "Ontology", icon: BookMarked, external: true },
  { to: "/decisions", label: "Decisions", icon: Gavel },
];

function useTheme() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    try {
      localStorage.setItem("athanor-theme", dark ? "dark" : "light");
    } catch {
      // A browser that refuses storage still gets the theme, just not the memory of it.
    }
  }, [dark]);
  return [dark, setDark] as const;
}

export function Layout() {
  const { session, signOut } = useSession();
  const [dark, setDark] = useTheme();

  const items = NAV.map(({ to, label, icon: Icon, end, external }) => {
    const content = (
      <>
        <Icon className="size-4 shrink-0" aria-hidden />
        <span className="truncate">{label}</span>
      </>
    );
    const base =
      "flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors hover:bg-accent hover:text-accent-foreground";
    return external ? (
      <a key={to} href={to} className={cn(base, "text-foreground/80")}>
        {content}
      </a>
    ) : (
      <NavLink
        key={to}
        to={to}
        end={end}
        className={({ isActive }) =>
          cn(base, isActive ? "bg-accent text-accent-foreground font-medium" : "text-foreground/80")
        }
      >
        {content}
      </NavLink>
    );
  });

  return (
    <div className="bg-background flex min-h-full flex-col md:flex-row">
      <header className="bg-card flex items-center gap-3 border-b px-4 py-3 md:hidden">
        <span className="text-base font-semibold tracking-tight">Athanor</span>
        <div className="ml-auto flex items-center gap-1">
          <Button variant="ghost" size="icon" onClick={() => setDark(!dark)} aria-label="Toggle theme">
            {dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
          </Button>
          {session?.signed_in && !session.open && (
            <Button variant="ghost" size="icon" onClick={signOut} aria-label="Sign out">
              <LogOut className="size-4" />
            </Button>
          )}
        </div>
      </header>

      <aside className="bg-card hidden w-56 shrink-0 flex-col border-r p-3 md:flex">
        <div className="px-3 py-2 text-base font-semibold tracking-tight">Athanor</div>
        <nav className="mt-2 flex flex-col gap-0.5">{items}</nav>
        <div className="text-muted-foreground mt-auto space-y-2 border-t px-3 pt-3 text-xs">
          {session?.actor && (
            <p>
              {session.actor}
              {session.clearance && <span className="opacity-70"> · {session.clearance}</span>}
            </p>
          )}
          {session?.describe && <p className="font-mono break-words opacity-70">{session.describe}</p>}
          <div className="flex items-center gap-1">
            <Button variant="ghost" size="sm" onClick={() => setDark(!dark)} className="h-8 px-2">
              {dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
            </Button>
            {session?.signed_in && !session.open && (
              <Button variant="ghost" size="sm" onClick={signOut} className="h-8 px-2">
                <LogOut className="size-4" /> Sign out
              </Button>
            )}
          </div>
        </div>
      </aside>

      <main className="min-w-0 flex-1 px-4 py-6 pb-24 md:px-8 md:pb-10">
        <div className="mx-auto max-w-5xl">
          <Outlet />
        </div>
      </main>

      <nav className="bg-card fixed inset-x-0 bottom-0 z-30 grid grid-cols-6 border-t md:hidden">
        {NAV.map(({ to, label, icon: Icon, end, external }) => {
          const inner = (
            <>
              <Icon className="size-4" aria-hidden />
              <span className="truncate text-[10px] leading-tight">{label}</span>
            </>
          );
          const base = "flex min-h-14 flex-col items-center justify-center gap-1 px-1";
          return external ? (
            <a key={to} href={to} className={cn(base, "text-foreground/70")}>
              {inner}
            </a>
          ) : (
            <NavLink
              key={to}
              to={to}
              end={end}
              className={({ isActive }) =>
                cn(base, isActive ? "text-primary bg-accent font-medium" : "text-foreground/70")
              }
            >
              {inner}
            </NavLink>
          );
        })}
      </nav>
    </div>
  );
}
