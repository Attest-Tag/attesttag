import type { Metadata } from "next";
import { VerifyPanel } from "@/components/auth/recovery-forms";

export const metadata: Metadata = { title: "Confirm your email" };

export default function Page() {
  return <VerifyPanel />;
}
