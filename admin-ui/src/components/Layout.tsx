import { useEffect, useState } from "react";
import { Outlet } from "react-router";
import { AppSidebar, NavLinks, SidebarBrand, SignOutButton } from "./app-sidebar";
import { AppHeader } from "./app-header";
import { CommandPalette } from "./command-palette";
import { isPublicDemoHost } from "./common";
import { Dialog, DialogContent, DialogTitle } from "./ui/dialog";

function DemoStrip() {
  if (!isPublicDemoHost()) return null;
  return (
    <div className="flex h-9 shrink-0 items-center justify-between gap-3 border-b border-border bg-muted/50 px-4 font-mono text-[11px] tracking-wide text-muted-foreground">
      <span className="flex min-w-0 items-center gap-2 truncate">
        <span className="inline-block size-1.5 shrink-0 rounded-full bg-primary" />
        Live sandbox plane · same binary as runkite serve · fake agents
      </span>
      <a href="/" className="shrink-0 text-primary no-underline hover:underline">
        Product
      </a>
    </div>
  );
}

export function Layout() {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [mobileNavOpen, setMobileNavOpen] = useState(false);

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      }
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, []);

  return (
    <div className="flex h-screen flex-col overflow-hidden bg-background text-foreground">
      <DemoStrip />
      <div className="flex min-h-0 flex-1 overflow-hidden">
        <AppSidebar />

        <Dialog open={mobileNavOpen} onOpenChange={setMobileNavOpen}>
          <DialogContent
            className="fixed inset-y-0 left-0 top-0 flex h-full w-72 max-w-[85vw] translate-x-0 translate-y-0 flex-col gap-0 rounded-none border-y-0 border-l-0 p-0 data-[state=closed]:slide-out-to-left data-[state=open]:slide-in-from-left data-[state=closed]:zoom-out-100 data-[state=open]:zoom-in-100 sm:max-w-72"
            aria-describedby={undefined}
          >
            <DialogTitle className="sr-only">Navigation</DialogTitle>
            <div className="flex h-full flex-col bg-sidebar text-sidebar-foreground">
              <SidebarBrand />
              <NavLinks onNavigate={() => setMobileNavOpen(false)} />
              <SignOutButton />
            </div>
          </DialogContent>
        </Dialog>

        <div className="flex min-w-0 flex-1 flex-col">
          <AppHeader
            onOpenCommandPalette={() => setPaletteOpen(true)}
            onOpenMobileNav={() => setMobileNavOpen(true)}
          />
          <main className="flex-1 overflow-y-auto px-4 py-5 sm:px-6 sm:py-6 lg:px-8 lg:py-8">
            <div className="mx-auto max-w-7xl">
              <Outlet />
            </div>
          </main>
        </div>
        <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
      </div>
    </div>
  );
}
