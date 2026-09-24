import type { Metadata } from "next";
import { ApiReference } from "@/components/developer/api-reference";

export const metadata: Metadata = { title: "API reference" };

export default function Page() {
  return <ApiReference />;
}
