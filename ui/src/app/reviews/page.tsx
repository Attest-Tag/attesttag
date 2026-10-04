import type { Metadata } from "next";
import { ReviewsPage } from "@/components/reviews/reviews-page";

export const metadata: Metadata = { title: "Reviews" };

export default function Page() {
  return <ReviewsPage />;
}
