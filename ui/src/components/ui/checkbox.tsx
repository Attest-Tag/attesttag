"use client"

import * as React from "react"
import { CheckIcon, MinusIcon } from "lucide-react"
import { Checkbox as CheckboxPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"

function Checkbox({
  className,
  ...props
}: React.ComponentProps<typeof CheckboxPrimitive.Root>) {
  // Half of a group is ticked: Radix carries that as its own state, and it has to read as
  // "some", not as "all" — the filled box with a tick is what a group header shows when every
  // row under it is picked, so indeterminate gets the fill and a dash instead.
  const some = props.checked === "indeterminate"
  return (
    <CheckboxPrimitive.Root
      data-slot="checkbox"
      className={cn(
        // size-5 (15px), not the registry's size-4: on the 3px grid size-4 is
        // 12px, which undershoots both the 16px checkbox convention and a
        // usable hit target. A box the user has to hit has a floor the density
        // token shouldn't scale it below.
        //
        // rounded-[2px], not the registry's 4px: at 15px a 4px radius reads
        // as a bubble, and the flat skin squares its small controls — the
        // switch fork made the same correction (see DESIGN.md).
        "peer size-5 shrink-0 rounded-[2px] border border-input shadow-xs transition-shadow outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-destructive/20 data-[state=checked]:border-primary data-[state=checked]:bg-primary data-[state=checked]:text-primary-foreground data-[state=indeterminate]:border-primary data-[state=indeterminate]:bg-primary data-[state=indeterminate]:text-primary-foreground dark:bg-input/30 dark:aria-invalid:ring-destructive/40 dark:data-[state=checked]:bg-primary dark:data-[state=indeterminate]:bg-primary",
        className
      )}
      {...props}
    >
      <CheckboxPrimitive.Indicator
        data-slot="checkbox-indicator"
        className="grid place-content-center text-current transition-none"
      >
        {/* size-3.5 with a heavier stroke: the registry's size-4 glyph fills
            the 15px box edge-to-edge and overflows any caller that sizes the
            box down; a smaller, bolder tick stays crisp and contained.
            text-current is spelled out rather than left to the indicator: a
            menu or command item paints every icon inside it muted-foreground
            unless the class already names a colour, which turned the tick grey
            on a filled purple box — a ticked box that looked empty. */}
        {some ? (
          <MinusIcon className="size-3.5 text-current" strokeWidth={3} />
        ) : (
          <CheckIcon className="size-3.5 text-current" strokeWidth={2.5} />
        )}
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  )
}

export { Checkbox }
