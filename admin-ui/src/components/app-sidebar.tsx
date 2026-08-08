import { NavLink } from "react-router";
import {
  Bot,
  Clock,
  LayoutDashboard,
  LogOut,
  MessagesSquare,
  Package,
  Plug,
  Shield,
  Webhook as WebhookIcon,
  Workflow,
} from "lucide-react";
import { cn } from "../lib/utils";
import { useAuth } from "../auth/AuthContext";
import { Button } from "./ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "./ui/tooltip";

interface NavItem {
  to: string;
  label: string;
  icon: typeof LayoutDashboard;
  end?: boolean;
}

interface NavGroup {
  label: string;
  items: NavItem[];
}

export const NAV_GROUPS: NavGroup[] = [
  { label: "", items: [{ to: "/admin/", label: "Overview", icon: LayoutDashboard, end: true }] },
  {
    label: "Resources",
    items: [
      { to: "/admin/agents", label: "Agents", icon: Bot },
      { to: "/admin/registry", label: "Registry", icon: Package },
      { to: "/admin/threads", label: "Threads", icon: MessagesSquare },
      { to: "/admin/runs", label: "Runs", icon: Workflow },
    ],
  },
  {
    label: "Platform",
    items: [
      { to: "/admin/connectors", label: "Connectors", icon: Plug },
      { to: "/admin/cron", label: "Cron", icon: Clock },
      { to: "/admin/webhooks", label: "Webhooks", icon: WebhookIcon },
      { to: "/admin/audit", label: "Audit", icon: Shield },
    ],
  },
];

export function NavLinks({ onNavigate }: { onNavigate?: () => void }) {
  return (
    <nav className="flex-1 space-y-5 overflow-y-auto px-3 py-3">
      {NAV_GROUPS.map((group) => (
        <div key={group.label || "root"}>
          {group.label && (
            <p className="mb-1.5 px-2.5 text-[11px] font-medium tracking-wide text-muted-foreground/70 uppercase">
              {group.label}
            </p>
          )}
          <div className="space-y-0.5">
            {group.items.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                onClick={onNavigate}
                className={({ isActive }) =>
                  cn(
                    "group flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-sm font-medium transition-colors",
                    isActive
                      ? "bg-sidebar-primary text-sidebar-primary-foreground shadow-sm"
                      : "text-sidebar-foreground/70 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground",
                  )
                }
              >
                <item.icon className="size-4 shrink-0" />
                {item.label}
              </NavLink>
            ))}
          </div>
        </div>
      ))}
    </nav>
  );
}

export function SidebarBrand() {
  return (
    <div className="flex h-14 items-center gap-2 px-4">
      <img
        src={`${import.meta.env.BASE_URL}logo.svg`}
        alt=""
        width={28}
        height={28}
        className="size-7 rounded-md"
      />
      <span className="text-sm font-semibold tracking-tight">Runkite</span>
      <span className="ml-auto rounded-full border border-sidebar-border px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
        Admin
      </span>
    </div>
  );
}

export function SignOutButton() {
  const { logout, authRequired } = useAuth();
  if (!authRequired) return null;

  return (
    <div className="border-t border-sidebar-border p-3">
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            variant="ghost"
            className="w-full justify-start gap-2.5 text-sidebar-foreground/70 hover:text-sidebar-accent-foreground"
            onClick={logout}
          >
            <LogOut className="size-4" />
            Sign out
          </Button>
        </TooltipTrigger>
        <TooltipContent side="right">Clear the stored credential and return to the login screen</TooltipContent>
      </Tooltip>
    </div>
  );
}

export function AppSidebar() {
  return (
    <aside className="hidden h-screen w-60 shrink-0 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground md:flex">
      <SidebarBrand />
      <NavLinks />
      <SignOutButton />
    </aside>
  );
}
