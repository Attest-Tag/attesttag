import type { Metadata } from "next";
import { SignUpForm } from "@/components/auth/sign-up-form";

export const metadata: Metadata = { title: "Create your organisation" };

export default function Page() {
  return <SignUpForm />;
}
