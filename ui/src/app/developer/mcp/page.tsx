import type { Metadata } from "next";
import { McpPanel } from "@/components/developer/mcp-panel";

export const metadata: Metadata = { title: "MCP" };

export default function Page() {
  return <McpPanel />;
}
