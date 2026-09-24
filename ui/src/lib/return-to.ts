// Where to go back to after signing in, for the one page that sends somebody to sign in on its
// own behalf: the MCP consent screen, which an app like Claude opens in a browser that may not be
// signed in to this console yet. Session storage, so it lives exactly as long as the tab doing the
// signing in, survives the round trip through Slack or Google sign-in, and is read once.
const KEY = "attesttag.after_sign_in";

// Only the consent page may ask to be returned to. Anything else found here is not something this
// console wrote, and following it would make the sign-in page an open redirect.
function allowed(path: string): boolean {
  return path.startsWith("/admin/oauth/") && !path.includes("//");
}

export function rememberReturn(path: string) {
  try {
    if (allowed(path)) sessionStorage.setItem(KEY, path);
  } catch {
    // Storage refused (a private window, a blocked site): signing in still works, and the app
    // that sent them will simply ask again.
  }
}

export function takeReturn(): string | null {
  try {
    const path = sessionStorage.getItem(KEY);
    sessionStorage.removeItem(KEY);
    return path && allowed(path) ? path : null;
  } catch {
    return null;
  }
}
