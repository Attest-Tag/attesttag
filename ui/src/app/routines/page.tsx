import type { Metadata } from "next";
import { RoutinesPage } from "@/components/routines/routines-page";

export const metadata: Metadata = { title: "Routines" };

export default function Page() {
  return <RoutinesPage />;
}
