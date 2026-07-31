import { Link, useLocation } from "react-router-dom";
import { Menu, Search } from "lucide-react";
import { NAV_GROUPS } from "./app-sidebar";
import { ModeToggle } from "./mode-toggle";
import { Button } from "./ui/button";

function currentCrumb(pathname: string): { section?: string; sectionTo?: string; page: string } {
  for (const group of NAV_GROUPS) {
    for (const item of group.items) {
      const base = item.to.replace(/\/$/, "");
      if (pathname === item.to || pathname === base) {
        return { section: group.label || undefined, page: item.label };
      }
      if (!item.end && pathname.startsWith(base + "/")) {
        return {
          section: item.label,
          sectionTo: item.to,
          page: `${item.label} detail`,
        };
      }
    }
  }
  const parts = pathname.split("/").filter(Boolean);
  const resource = parts[1];
  const match = NAV_GROUPS.flatMap((g) => g.items).find((i) => i.to.includes(`/${resource}`));
  return {
    section: match?.label,
    sectionTo: match?.to,
    page: match ? `${match.label} detail` : "Runkite Admin",
  };
}

function shortcutLabel(): string {
  if (typeof navigator === "undefined") return "⌘K";
  const platform = navigator.platform || "";
  const ua = navigator.userAgent || "";
  if (/Mac|iPhone|iPad|iPod/i.test(platform) || /Mac OS/i.test(ua)) return "⌘K";
  return "Ctrl+K";
}

export function AppHeader({
  onOpenCommandPalette,
  onOpenMobileNav,
}: {
  onOpenCommandPalette: () => void;
  onOpenMobileNav: () => void;
}) {
  const { pathname } = useLocation();
  const { section, sectionTo, page } = currentCrumb(pathname);

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b border-border/60 bg-background/80 px-4 backdrop-blur-sm sm:gap-4 sm:px-6">
      <Button
        variant="ghost"
        size="icon"
        className="md:hidden"
        onClick={onOpenMobileNav}
        aria-label="Open navigation"
      >
        <Menu className="size-4" />
      </Button>

      <div className="flex min-w-0 items-center gap-1.5 text-sm">
        {section && sectionTo ? (
          <Link to={sectionTo} className="truncate text-muted-foreground hover:text-foreground">
            {section}
          </Link>
        ) : (
          section && <span className="truncate text-muted-foreground">{section}</span>
        )}
        {section && <span className="text-muted-foreground/40">/</span>}
        <span className="truncate font-medium">{page}</span>
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
            {shortcutLabel()}
          </kbd>
        </Button>
        <Button variant="ghost" size="icon" onClick={onOpenCommandPalette} className="sm:hidden" aria-label="Search">
          <Search className="size-4" />
        </Button>
        <ModeToggle />
      </div>
    </header>
  );
}
