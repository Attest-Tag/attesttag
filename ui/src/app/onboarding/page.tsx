import type { Metadata } from "next";
import { OnboardingPage } from "@/components/onboarding/onboarding-page";

export const metadata: Metadata = { title: "Set up" };

export default function Page() {
  return <OnboardingPage />;
}
