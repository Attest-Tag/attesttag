import Link from "next/link";
import { SearchX } from "lucide-react";
import { EmptyState } from "@/components/core/empty-state";
import { Button } from "@/components/ui/button";

export default function NotFound() {
  return (
    <EmptyState
      icon={SearchX}
      title="No such page"
      description="Nothing lives at this address. The sidebar has everything the console can show."
      action={
        <Button asChild>
          <Link href="/">Back to overview</Link>
        </Button>
      }
    />
  );
}
