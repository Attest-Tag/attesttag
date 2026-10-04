// Where the GitHub App install comes back to. The server sends every install back to Access
// bundles (/admin/bundles/?github_install=<id>), which is where installing began until code review
// offered it too; an install started from Reviews › Settings marks this tab first, and the Bundles
// page forwards the answer here, so the new installation opens ready to add — the way a Slack
// install lands back on Workspaces with ?team=.
//
// Session storage, like return-to.ts: it lives exactly as long as the tab doing the install,
// survives the round trip through GitHub, and is read once. It holds a flag and the time it was
// set, never a URL, so nothing written into it can send the Bundles page anywhere but here. The
// time is what keeps an install somebody backed out of at GitHub from claiming the next one: an
// install begun from Bundles later in the same tab, past MAX_AGE_MS or after forgetInstallFrom,
// lands where it began.

const KEY = "attesttag.github_install_from";
const FROM = "reviews";
/** Long enough to pick an account and its repositories at GitHub, and approve a sign-in on the way. */
const MAX_AGE_MS = 15 * 60 * 1000;

export function markInstallFromReviews() {
  try {
    sessionStorage.setItem(KEY, `${FROM}:${Date.now()}`);
  } catch {
    // Storage refused: the install still works, and lands on Access bundles as it always has.
  }
}

/** An install started anywhere else: whatever Reviews marked before is spent. */
export function forgetInstallFrom() {
  try {
    sessionStorage.removeItem(KEY);
  } catch {
    // Nothing kept, nothing to forget.
  }
}

/** Whether the install that just came back was started from Reviews; reading it clears it. */
export function takeInstallFromReviews(): boolean {
  try {
    const v = sessionStorage.getItem(KEY) ?? "";
    sessionStorage.removeItem(KEY);
    const [from, at] = v.split(":");
    const age = Date.now() - Number(at);
    return from === FROM && Number.isFinite(age) && age >= 0 && age < MAX_AGE_MS;
  } catch {
    return false;
  }
}
