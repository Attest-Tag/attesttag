import type { Metadata } from "next";
import { ApproversPage } from "@/components/access/approvers-page";

export const metadata: Metadata = { title: "Approvers" };

export default function Page() {
  return <ApproversPage />;
}
