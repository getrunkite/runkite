import { Toaster as Sonner, type ToasterProps } from "sonner";
import { useTheme } from "../theme-provider";

/** Toasts for write-action feedback (cancel run, delete thread, redeliver
 * webhook, etc.) instead of an inline <ErrorMessage> that only ever
 * showed up-or-not depending on scroll position -- a toast is visible
 * regardless of where on the page the action was triggered from, and
 * auto-dismisses success feedback instead of leaving stale text on
 * screen. Wired to the same ThemeProvider as the rest of the app so
 * toasts always match light/dark, not sonner's own OS-preference guess. */
function Toaster(props: ToasterProps) {
  const { theme } = useTheme();

  return (
    <Sonner
      theme={theme}
      className="toaster group"
      position="bottom-right"
      toastOptions={{
        classNames: {
          toast:
            "group toast group-[.toaster]:bg-popover group-[.toaster]:text-popover-foreground group-[.toaster]:border-border/60 group-[.toaster]:shadow-lg group-[.toaster]:rounded-lg",
          description: "group-[.toast]:text-muted-foreground",
          actionButton: "group-[.toast]:bg-primary group-[.toast]:text-primary-foreground",
          cancelButton: "group-[.toast]:bg-muted group-[.toast]:text-muted-foreground",
        },
      }}
      {...props}
    />
  );
}

export { Toaster };
