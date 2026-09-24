import type { Metadata } from "next";
import { ForgotForm } from "@/components/auth/recovery-forms";

export const metadata: Metadata = { title: "Reset your password" };

export default function Page() {
  return <ForgotForm />;
}
