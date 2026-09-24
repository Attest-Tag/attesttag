import type { Metadata } from "next";
import { MemoryPage } from "@/components/memory/memory-page";

export const metadata: Metadata = { title: "Memory" };

export default function Page() {
  return <MemoryPage />;
}
