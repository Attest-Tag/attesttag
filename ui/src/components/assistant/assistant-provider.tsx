"use client";

import { createContext, useContext, useEffect } from "react";
import { useAuth } from "@/components/shell/auth-provider";
import { useStickyFlags } from "@/hooks/use-sticky";
import { OVERLAY, TYPING } from "@/hooks/use-search-shortcut";

// Open or closed, in one place, because the button that toggles it lives in the topbar and the
// panel is a sibling of the whole inset — neither can own the state the other reads.
//
// It sticks per browser rather than per account: whether somebody keeps the assistant open is
// how they use this window, not something the organisation has an opinion about.

type AssistantCtx = { open: boolean; setOpen: (on: boolean) => void; allowed: boolean };

const Ctx = createContext<AssistantCtx>({ open: false, setOpen: () => {}, allowed: false });

export function useAssistant() {
  return useContext(Ctx);
}

export function AssistantProvider({ children }: { children: React.ReactNode }) {
  const { me } = useAuth();
  const flags = useStickyFlags("assistant");
  // Every tool the assistant has is about a channel's settings, and its one staging tool
  // proposes a scopes.manage write, so the endpoint is gated on that — the same gate the
  // playground carries. /api/me sends only the keys the role holds, so the check is for a
  // present true; until it has answered there is nothing to check against and the assistant
  // stays available rather than flickering out from under a keystroke.
  const perms = me?.user?.permissions;
  const allowed = !perms || perms["scopes.manage"] === true;
  const open = allowed && flags.get("open", false);
  const setOpen = (on: boolean) => flags.set("open", on);

  // Cmd/Ctrl+I. Not Cmd+B — SidebarProvider binds that for the rail — and quiet wherever the
  // keystroke already belongs to something else, over the selectors the search shortcut uses.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "i" || !(e.metaKey || e.ctrlKey) || e.altKey || e.isComposing) return;
      const target = e.target as HTMLElement | null;
      if (target && (target.isContentEditable || TYPING.test(target.tagName))) return;
      if (target?.closest(OVERLAY)) return;
      e.preventDefault();
      setOpen(!open);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  });

  return <Ctx.Provider value={{ open, setOpen, allowed }}>{children}</Ctx.Provider>;
}
