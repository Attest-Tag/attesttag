import type { Metadata } from "next";
import { BundlesPage } from "@/components/bundles/bundles-page";

export const metadata: Metadata = { title: "Access bundles" };

export default function Page() {
  return <BundlesPage />;
}
