import type { Metadata } from "next";
import { AccessRequestsPage } from "@/components/access/access-requests-page";

export const metadata: Metadata = { title: "Access requests" };

export default function Page() {
  return <AccessRequestsPage />;
}
