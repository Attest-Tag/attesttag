import type { Metadata } from "next";
import { McpConsent } from "@/components/developer/mcp-consent";

export const metadata: Metadata = { title: "Connect an app" };

export default function Page() {
  return <McpConsent />;
}
