import { cn } from "@/lib/utils";

/**
 * attest_tag brand mark: a squared @ drawn as one continuous monoline stroke.
 * The outer loop (ink) curls inward at the top right and hands off, at a
 * single perpendicular seam, to a check mark (brand accent) sitting where the
 * @'s inner "a" would be. Same 256-unit master as the Slack app icon.
 */
export function BrandMark({ className, size = "md" }: { className?: string; size?: "sm" | "md" | "lg" }) {
  const dims = { sm: "size-6", md: "size-7", lg: "size-10" };
  return (
    <svg
      viewBox="0 0 256 256"
      aria-hidden="true"
      focusable="false"
      fill="none"
      strokeWidth={30}
      strokeLinecap="butt"
      strokeLinejoin="round"
      className={cn("shrink-0 select-none", dims[size], className)}
    >
      {/* verification: the check, brand accent */}
      <path d="M97.6 121.6 L140 172.2 L210.9 112.7" className="stroke-primary" />
      {/* mention: the squared @ loop, ink */}
      <path d="M210.9 112.7 A48 48 0 0 0 180 28 L76 28 A48 48 0 0 0 28 76 L28 180 A48 48 0 0 0 76 228 L180 228 A48 48 0 0 0 223.5 200.3" className="stroke-foreground" />
    </svg>
  );
}
