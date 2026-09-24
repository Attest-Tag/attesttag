import type { Metadata } from "next";
import { JoinPanel } from "@/components/auth/join-panel";

export const metadata: Metadata = { title: "Join an organisation" };

export default function Page() {
  return <JoinPanel />;
}
