import type { Metadata } from "next";
import { ResetForm } from "@/components/auth/recovery-forms";

export const metadata: Metadata = { title: "Choose a new password" };

export default function Page() {
  return <ResetForm />;
}
