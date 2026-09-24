"use client"

import * as React from "react"
import { Switch as SwitchPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"

/* Forked from the registry switch. The stock one is sized for a 4px spacing
   grid (`w-8` + a hard-coded `h-[1.15rem]`); against our 3px `--spacing` the
   track collapsed to 24×18.4 — a stubby pill whose thumb had 3px of track
   above and below but none at the ends. Geometry here is explicit and on the
   grid: the track carries a 2px pad, the thumb is exactly the content box, and
   the travel is what's left over. Squared to `rounded-sm` like the rest of the
   skin — the switch was the last real pill in the app. */

const TRACK = {
  default: "h-5 w-9",
  sm: "h-4 w-7",
} as const

function Switch({
  className,
  size = "default",
  ...props
}: React.ComponentProps<typeof SwitchPrimitive.Root> & {
  size?: "sm" | "default"
}) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      data-size={size}
      className={cn(
        "peer group/switch relative inline-flex shrink-0 items-center rounded-sm p-[2px] shadow-xs transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 data-[state=checked]:bg-primary data-[state=unchecked]:bg-input dark:data-[state=unchecked]:bg-input/80",
        // The track is deliberately shorter than the 24px control floor, so an
        // invisible bleed carries the hit target back over it.
        "before:absolute before:inset-x-0 before:-inset-y-[5px] before:content-['']",
        TRACK[size],
        className
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        data-slot="switch-thumb"
        className={cn(
          "pointer-events-none block rounded-[2px] bg-background ring-0 transition-transform",
          "group-data-[size=default]/switch:size-[11px] group-data-[size=default]/switch:data-[state=checked]:translate-x-[12px]",
          "group-data-[size=sm]/switch:size-[8px] group-data-[size=sm]/switch:data-[state=checked]:translate-x-[9px]",
          "data-[state=unchecked]:translate-x-0 dark:data-[state=checked]:bg-primary-foreground dark:data-[state=unchecked]:bg-foreground"
        )}
      />
    </SwitchPrimitive.Root>
  )
}

export { Switch }
