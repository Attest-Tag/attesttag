import type { Metadata } from "next";
import { PlaygroundPage } from "@/components/playground/playground-page";

export const metadata: Metadata = { title: "Playground" };

export default function Page() {
  return <PlaygroundPage />;
}
