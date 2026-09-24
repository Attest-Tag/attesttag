import { AlertTriangle } from "lucide-react";
import { Button } from "@/components/ui/button";

// A load failure the page can't do anything about except try again. Shown in
// place of the content, not as a toast, because there is nothing behind it.
export function ErrorBanner({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div className="flex items-center gap-3 rounded-xl border border-danger/30 bg-danger-soft px-4 py-3 text-sm">
      <AlertTriangle className="size-4 shrink-0 text-danger" />
      <p className="min-w-0 flex-1 text-foreground">{message}</p>
      {onRetry && (
        <Button variant="outline" size="sm" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}
