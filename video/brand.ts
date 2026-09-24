/**
 * The three brand strings the cards and the API scene print on screen.
 *
 * They were imported from the marketing site's `src/config/brand.ts` while the
 * site was a folder in this repository. It is its own repository now
 * (`Attest-Tag/attesttag-website`), and a recorder that cannot start without a
 * sibling checkout on the same disk is one that stops working on the next
 * machine — so the three strings it actually renders are copied here instead.
 *
 * Only these three. If the product is renamed or moves domain they change in
 * both places, and a recording is the one place a stale one is visible: it is
 * burned into the title card rather than re-read on the next page load.
 */
export const BRAND = {
  name: "attest_tag",
  domain: "attesttag.com",
  appUrl: "https://app.attesttag.com",
} as const;
