"use client";

import { usePathname } from "next/navigation";
import { Sparkles } from "lucide-react";
import { useAssistant } from "@/components/assistant/assistant-provider";
import { HelpButton } from "@/components/shell/help-button";
import { UserMenu } from "@/components/shell/user-menu";
import { Button } from "@/components/ui/button";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { navItemFor } from "@/lib/nav";

// Header bar: the current section's name on the left, help, the assistant and the user menu on
// the right. Below md the rail is an off-canvas sheet, so its toggle lives here.
export function Topbar() {
  const pathname = usePathname();
  const item = navItemFor(pathname);
  const assistant = useAssistant();
  return (
    <header className="flex h-12 shrink-0 items-center gap-3 border-b bg-background px-4">
      <SidebarTrigger className="size-8 text-muted-foreground md:hidden" />
      <p className="min-w-0 flex-1 truncate text-sm font-medium text-foreground">
        {item?.title ?? "attest_tag"}
      </p>
      <HelpButton />
      {assistant.allowed && (
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className={assistant.open ? "size-8 text-ai" : "size-8 text-muted-foreground"}
              onClick={() => assistant.setOpen(!assistant.open)}
              aria-label="Assistant"
              aria-pressed={assistant.open}
            >
              <Sparkles className="size-4" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>Assistant — ⌘I</TooltipContent>
        </Tooltip>
      )}
      <UserMenu />
    </header>
  );
}
