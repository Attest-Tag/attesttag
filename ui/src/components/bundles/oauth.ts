import { toast } from "sonner";
import { api, errorMessage, type OAuthStart } from "@/lib/api";

export type OAuthStartInput = {
  client_id?: string;
  client_secret?: string;
  scopes?: string;
};

/**
 * Begin the OAuth sign-in for an MCP connection. On success the browser
 * leaves for the provider; the server sends it back to
 * /admin/bundles/?connected=<id>, which the bundles page turns into a toast.
 * Returns false when nothing happened (error shown as a toast).
 */
export async function startOAuthSignIn(connectionId: number, input: OAuthStartInput = {}): Promise<boolean> {
  const body: OAuthStartInput = {};
  if (input.client_id?.trim()) body.client_id = input.client_id.trim();
  if (input.client_secret) body.client_secret = input.client_secret;
  if (input.scopes?.trim()) body.scopes = input.scopes.trim();
  try {
    const res = await api.post<OAuthStart>(`/api/connections/${connectionId}/oauth/start`, body);
    if (!res.ok) {
      toast.error(res.error || "Could not start sign-in");
      return false;
    }
    window.location.href = res.url;
    return true;
  } catch (err) {
    toast.error(errorMessage(err));
    return false;
  }
}

/** The query parameter the OAuth callback lands on; cleared once acknowledged. */
export const CONNECTED_PARAM = "connected";
