"use client";

import { ThemeProvider } from "next-themes";
import { AuthProvider } from "@/components/shell/auth-provider";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";

// Client-side context for the whole console: theme (light by default; a `dark`
// class on <html>, remembered in localStorage), the signed-in user, tooltips, and toasts.
export function Providers({ children }: { children: React.ReactNode }) {
  return (
    <ThemeProvider
      attribute="class"
      defaultTheme="light"
      enableSystem
      storageKey="attest-tag-theme"
      disableTransitionOnChange
    >
      <AuthProvider>
        <TooltipProvider delayDuration={0}>{children}</TooltipProvider>
      </AuthProvider>
      <Toaster position="bottom-right" />
    </ThemeProvider>
  );
}
