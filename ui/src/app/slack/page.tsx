"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";

// Workspaces lived at /admin/slack back when Slack was the only thing that
// could be connected. Bookmarks, older Slack messages and the server's own
// startup warnings still point here, and the SPA fallback would land them on
// Overview without saying why, so the old path stays as a redirect. The query
// string comes along: an install returns with ?team= or ?install_error=.
export default function Page() {
  const router = useRouter();
  useEffect(() => {
    router.replace("/workspaces/" + window.location.search);
  }, [router]);
  return null;
}
