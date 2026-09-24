package app

// An organisation's own model key.
//
// The deployment answers every organisation on one key of its own (LLM_API_KEY), and pays for all
// of it. An organisation may instead bring its own: an OpenAI-compatible base URL and key — OpenAI
// itself, its own OpenRouter account, Azure OpenAI's v1 endpoint, anything that speaks the Chat
// Completions API on a public https address — stored in org_model_keys and managed from
// Settings → Models by whoever holds settings.manage there. While it is in use, every model call
// made on the organisation's behalf goes to that endpoint on that key: its turns in every
// channel, the side calls around them, the console assistant, its documents' embeddings and its
// fix jobs. The spend is the organisation's own, so none of it is charged to credit and none of
// the deployment's ceilings apply to it.
//
// Two rules hold everything else up:
//
//   - Nothing falls back. A key that is refused, out of quota, unreadable, or no longer allowed on
//     the organisation's plan stops the organisation's model calls with a sentence that says why;
//     it never quietly sends them to the deployment's key instead. An organisation brings a key so
//     that its conversations go where it chose, and a fallback would send them where it did not,
//     at the deployment's expense, without anybody deciding that.
//   - The key never leaves the server. It is sealed at rest, it is not in Settings (every member
//     of the organisation can read those), and what the console, the audit log and the operator
//     are shown is the host, the last four characters and a fingerprint.
//
// Who may bring one is ORG_MODEL_KEYS: every organisation on a deployment that runs its own
// (all — the default, so a self-host can rotate its key without a redeploy), only accounts on the
// enterprise plan (the hosted service), or nobody.

import "strings"

// Whose key paid for a model call, as the usage and jobs tables record it (migration 0021).
const (
	keyOwnerPlatform = "platform"
	keyOwnerOrg      = "org"
)

const (
	OrgModelKeysAll        = "all"
	OrgModelKeysEnterprise = "enterprise"
	OrgModelKeysOff        = "off"
)

// orgModelKeysOf reads ORG_MODEL_KEYS. Anything it does not recognise reads as enterprise — the
// narrowest setting under which the accounts that were sold a key keep it — and LoadConfig says so
// at startup rather than letting a typo decide in silence.
func orgModelKeysOf(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", OrgModelKeysAll:
		return OrgModelKeysAll
	case OrgModelKeysOff:
		return OrgModelKeysOff
	}
	return OrgModelKeysEnterprise
}

// ownKeyAllowed says whether this deployment lets an organisation on this plan use a key of its
// own. It is asked on every settings load, so moving an account off the enterprise plan takes its
// key out of use within fifteen seconds — and, because nothing falls back, stops its model calls.
func (c Config) ownKeyAllowed(plan string) bool {
	switch c.OrgModelKeys {
	case OrgModelKeysAll, "": // a Config built in code rather than loaded reads as the default
		return true
	case OrgModelKeysEnterprise:
		return plan == PlanEnterprise
	}
	return false
}

// OwnKey is what the process knows about an organisation's own model key without opening it.
type OwnKey struct {
	// Present: a key is stored.
	Present bool
	// Allowed: this deployment and the organisation's plan let it be used (ownKeyAllowed).
	Allowed bool
	// Unknown: the row could not be read. Every model call refuses on it: reading "no key" into
	// an error would send an organisation that brought one to the deployment's provider.
	Unknown bool
	Ref     ModelKeyRef
}

// Active reports that the organisation's model calls go to its own endpoint.
func (k OwnKey) Active() bool { return k.Present && k.Allowed && !k.Unknown }

// Blocked reports that the organisation's model calls may go nowhere: a key is stored, or may be,
// and cannot be used. The deployment's key is not the answer to either.
func (k OwnKey) Blocked() bool { return k.Unknown || (k.Present && !k.Allowed) }

// refusal is why a blocked organisation's model calls stop, as the sentence it is told; nil when
// they may go ahead.
func (k OwnKey) refusal() error {
	switch {
	case k.Unknown:
		return &ModelKeyError{Kind: keyUnavailable}
	case k.Present && !k.Allowed:
		return &ModelKeyError{Kind: keyNotAllowed, Host: k.Ref.Host()}
	}
	return nil
}
