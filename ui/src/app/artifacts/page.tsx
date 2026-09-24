import type { Metadata } from "next";
import { ArtifactsPage } from "@/components/artifacts/artifacts-page";

export const metadata: Metadata = { title: "Artifacts" };

export default function Page() {
  return <ArtifactsPage />;
}
