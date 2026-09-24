import type { Metadata } from "next";
import { ScopesPage } from "@/components/scopes/scopes-page";

export const metadata: Metadata = { title: "Workspaces" };

export default function Page() {
  return <ScopesPage />;
}
