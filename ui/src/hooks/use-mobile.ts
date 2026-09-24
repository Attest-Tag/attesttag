import * as React from "react"

const MOBILE_BREAKPOINT = 768

// Diverges from the shadcn registry version, which seeds `undefined` and then
// setStates the real answer inside an effect — one wasted render on every page,
// since the sidebar calls this on every signed-in screen, and the shape the
// react-hooks/set-state-in-effect rule exists to flag. useSyncExternalStore is
// what this hook was always describing: a subscription to a browser value.
//
// Behaviour is unchanged. The server snapshot is `false`, which is what the old
// `!!undefined` rendered on the server and through hydration, so the markup
// React commits is identical — it just arrives without the extra pass.

function subscribe(onStoreChange: () => void) {
  const mql = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT - 1}px)`)
  mql.addEventListener("change", onStoreChange)
  return () => mql.removeEventListener("change", onStoreChange)
}

export function useIsMobile() {
  return React.useSyncExternalStore(
    subscribe,
    () => window.innerWidth < MOBILE_BREAKPOINT,
    () => false,
  )
}
