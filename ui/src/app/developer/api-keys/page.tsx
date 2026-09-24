import type { Metadata } from "next";
import { ApiKeysPanel } from "@/components/developer/api-keys-panel";

export const metadata: Metadata = { title: "API keys" };

export default function Page() {
  return <ApiKeysPanel />;
}
