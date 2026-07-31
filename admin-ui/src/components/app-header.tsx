import { useLocation } from "react-router-dom";
import { Search } from "lucide-react";
import { NAV_GROUPS } from "./app-sidebar";
import { ModeToggle } from "./mode-toggle";
import { Button } from "./ui/button";

function currentLabel(pathname: string): { section?: string; page: string } {
  for (const group of NAV_GROUPS) {
    for (const item of group.items) {
      const base = item.to.replace(/\/$/, "");
      if (pathname === item.to || pathname === base || (!item.end && pathname.startsWith(base + "/"))) {
        return { section: group.label || undefined, page: item.label };
      }
    }
  }
  // Detail routes (e.g. /admin/threads/:id) fall through to a generic
  // label derived from whichever resource segment they're nested under.
  const parts = pathname.split("/").filter(Boolean);
  const resource = parts[1];
  const match = NAV_GROUPS.flatMap((g) => g.items).find((i) => i.to.includes(`/${resource}`));
  return { section: "Resources", page: match ? `${match.label} detail` : "Runkite Admin" };
}

export function AppHeader({ onOpenCommandPalette }: { onOpenCommandPalette: () => void }) {
  const { pathname } = useLocation();
  const { section, page } = currentLabel(pathname);

  return (
    <header className="flex h-14 shrink-0 items-center gap-4 border-b border-border/60 bg-background/80 px-6 backdrop-blur-sm">
      <div className="flex items-center gap-1.5 text-sm">
        {section && <span className="text-muted-foreground">{section}</span>}
        {section && <span className="text-muted-foreground/40">/</span>}
        <span className="font-medium">{page}</span>
      </div>

      <div className="ml-auto flex items-center gap-2">
        <Button
          variant="outline"
          size="sm"
          onClick={onOpenCommandPalette}
          className="hidden gap-2 text-muted-foreground sm:flex"
        >
          <Search className="size-3.5" />
          Search
          <kbd className="ml-2 rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-[10px] font-medium">
            ⌘K
          </kbd>
        </Button>
        <Button variant="ghost" size="icon" onClick={onOpenCommandPalette} className="sm:hidden">
          <Search className="size-4" />
        </Button>
        <ModeToggle />
      </div>
    </header>
  );
}
