import type { Metadata } from "next";
import { AuditPage } from "@/components/audit/audit-page";

export const metadata: Metadata = { title: "Audit log" };

export default function Page() {
  return <AuditPage />;
}
