import { useNavigate } from "react-router-dom";
import { LogOut, Moon, Sun } from "lucide-react";
import { useAuth } from "../auth/AuthContext";
import { useTheme } from "./theme-provider";
import { NAV_GROUPS } from "./app-sidebar";
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "./ui/command";

/** Global Cmd+K / Ctrl+K quick-navigation palette -- the single most
 * recognizable "this is a serious, fast tool" signal in Linear/Vercel/
 * GitHub/Raycast-caliber products. Jumping straight to any page (or
 * toggling the theme, or signing out) without ever touching the mouse
 * matters more here than it looks: this dashboard is meant to be kept
 * open and referred back to constantly while watching live runs, and a
 * keyboard-first "where do I click" answer removes friction every
 * single time, not just once.
 *
 * open/onOpenChange are lifted to Layout (not owned here) so the
 * header's own visible "Search ⌘K" button and the global keyboard
 * shortcut control the exact same dialog instance instead of each
 * needing their own. */
export function CommandPalette({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const navigate = useNavigate();
  const { logout, authRequired } = useAuth();
  const { theme, toggleTheme } = useTheme();

  function go(to: string) {
    onOpenChange(false);
    navigate(to);
  }

  return (
    <CommandDialog open={open} onOpenChange={onOpenChange}>
      <CommandInput placeholder="Search pages, or run an action..." />
      <CommandList>
        <CommandEmpty>No results found.</CommandEmpty>
        {NAV_GROUPS.map((group) => (
          <CommandGroup key={group.label || "root"} heading={group.label || "Navigate"}>
            {group.items.map((item) => (
              <CommandItem key={item.to} onSelect={() => go(item.to)}>
                <item.icon />
                {item.label}
              </CommandItem>
            ))}
          </CommandGroup>
        ))}
        <CommandSeparator />
        <CommandGroup heading="Actions">
          <CommandItem
            onSelect={() => {
              onOpenChange(false);
              toggleTheme();
            }}
          >
            {theme === "dark" ? <Sun /> : <Moon />}
            Switch to {theme === "dark" ? "light" : "dark"} theme
          </CommandItem>
          {authRequired && (
            <CommandItem
              onSelect={() => {
                onOpenChange(false);
                logout();
              }}
            >
              <LogOut />
              Sign out
            </CommandItem>
          )}
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  );
}
