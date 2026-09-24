import type { Metadata } from "next";
import { JobsPage } from "@/components/jobs/jobs-page";

export const metadata: Metadata = { title: "Jobs" };

export default function Page() {
  return <JobsPage />;
}
