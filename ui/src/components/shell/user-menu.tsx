"use client";

import { Building2, Check, LogOut, Moon, Sun } from "lucide-react";
import { useTheme } from "next-themes";
import { useAuth } from "@/components/shell/auth-provider";
import { api } from "@/lib/api";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

function initials(name: string): string {
  return name
    .split(/\s+/)
    .map((p) => p[0])
    .filter(Boolean)
    .slice(0, 2)
    .join("")
    .toUpperCase();
}

// Who is signed in, with the theme toggle and sign-out. One home: the topbar avatar. It used to
// be said twice — once here and once as a name-and-email chip in the sidebar footer — and the
// footer is a better home for settings than for a second copy of your own name.
export function UserMenu() {
  const { me, signOut } = useAuth();
  const { resolvedTheme, setTheme } = useTheme();
  const name = me?.user?.name || "Admin";
  const email = me?.user?.email || "";
  const role = me?.user?.role || "";
  const dark = resolvedTheme === "dark";
  const orgs = me?.orgs ?? [];
  const currentOrg = me?.user?.org_id;

  // Belonging to one organisation is the common case, and a switcher with a single entry is
  // furniture. The section appears only when there is somewhere to switch to.
  const switchTo = async (orgID: string) => {
    if (orgID === currentOrg) return;
    await api.post("/api/auth/switch-org", { org_id: orgID });
    // A full reload rather than a refetch: every page's data belongs to the old organisation.
    window.location.reload();
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          aria-label="Account menu"
          className="flex size-8 items-center justify-center rounded-md bg-accent text-xs font-medium text-accent-foreground"
        >
          {initials(name)}
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" side="bottom" className="w-56">
        <DropdownMenuLabel>
          <p className="text-sm font-medium">{name}</p>
          {email && <p className="text-xs font-normal text-muted-foreground">{email}</p>}
          {role && <p className="mt-1 text-xs font-normal capitalize text-muted-foreground">{role}</p>}
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        {orgs.length > 1 && (
          <>
            <DropdownMenuLabel className="eyebrow text-muted-foreground">
              Organisations
            </DropdownMenuLabel>
            {orgs.map((o) => (
              <DropdownMenuItem key={o.org_id} onClick={() => switchTo(o.org_id)}>
                {o.org_id === currentOrg ? (
                  <Check className="size-4" />
                ) : (
                  <Building2 className="size-4 opacity-60" />
                )}
                <span className="truncate">{o.org_name}</span>
                <span className="ml-auto text-xs capitalize text-muted-foreground">{o.role}</span>
              </DropdownMenuItem>
            ))}
            <DropdownMenuSeparator />
          </>
        )}
        <DropdownMenuItem onClick={() => setTheme(dark ? "light" : "dark")}>
          {dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
          {dark ? "Light mode" : "Dark mode"}
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={signOut}>
          <LogOut className="size-4" />
          Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
