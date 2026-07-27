import { NavLink, Outlet } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";

const NAV_ITEMS = [
  { to: "/admin/", label: "Overview", end: true },
  { to: "/admin/agents", label: "Agents" },
  { to: "/admin/threads", label: "Threads" },
  { to: "/admin/runs", label: "Runs" },
  { to: "/admin/connectors", label: "Connectors" },
  { to: "/admin/cron", label: "Cron" },
  { to: "/admin/webhooks", label: "Webhook dead-letters" },
];

function linkClass(isActive: boolean): string {
  return `block rounded-md px-3 py-2 text-sm font-medium transition ${
    isActive ? "bg-indigo-600 text-white" : "text-slate-300 hover:bg-slate-800 hover:text-white"
  }`;
}

export function Layout() {
  const { logout } = useAuth();

  return (
    <div className="flex min-h-screen bg-slate-950 text-slate-100">
      <aside className="flex w-56 flex-col border-r border-slate-800 bg-slate-900 p-4">
        <h1 className="mb-6 px-3 text-lg font-semibold">Runkite</h1>
        <nav className="flex-1 space-y-1">
          {NAV_ITEMS.map((item) => (
            <NavLink key={item.to} to={item.to} end={item.end} className={({ isActive }) => linkClass(isActive)}>
              {item.label}
            </NavLink>
          ))}
        </nav>
        <button
          onClick={logout}
          className="rounded-md px-3 py-2 text-left text-sm text-slate-400 transition hover:bg-slate-800 hover:text-white"
        >
          Sign out
        </button>
      </aside>
      <main className="flex-1 overflow-y-auto p-8">
        <Outlet />
      </main>
    </div>
  );
}
