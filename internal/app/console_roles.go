package app

import (
	"slices"
	"strings"
)

// What a console user may do.
//
// Two rules shape this file, and both are worth stating because the obvious alternatives are
// worse:
//
// The built-in roles live in code, not in the database. If every install held a frozen copy of
// "Admin", then a permission added in a later release would never reach it — silently, with
// nothing to notice. Custom roles are rows, because those belong to whoever wrote them.
//
// Permissions are a matrix, not a set of predicates. As functions this would be two dozen
// exported helpers each re-encoding the role list, and adding a role would mean editing every
// one; as data, a new role is one entry and the console can render "what each role can do"
// straight from it.
//
// Both lookups fail closed. A row may name a role this build has never heard of — an older
// custom role, a hand-edited database — and the answer for that is "nothing", never a real tier.

type Permission = string

const (
	PermUsersManage     Permission = "users.manage"       // invite, remove, change someone's role
	PermRolesManage     Permission = "roles.manage"       // define custom console roles
	PermSettingsManage  Permission = "settings.manage"    // models, budgets, limits, behaviour
	PermConnManage      Permission = "connections.manage" // credentials: create, rotate, delete, and their setup links
	PermConnView        Permission = "connections.view"   // see that a connection exists, never its secret
	PermBundlesManage   Permission = "bundles.manage"     // bundles, domains, skills
	PermScopesManage    Permission = "scopes.manage"      // per-channel instructions, model, budget, grants
	PermDocsManage      Permission = "documents.manage"
	PermMemoryManage    Permission = "memory.manage"
	PermRoutinesManage  Permission = "routines.manage"
	PermArtifactsView   Permission = "artifacts.view"
	PermArtifactsManage Permission = "artifacts.manage"
	PermActivityView    Permission = "activity.view"
	// The audit log: who signed in and from where, who changed what, who approved what. Held by
	// admin alone among the built-ins — it carries addresses and sign-in history, which is more
	// than "what the bot did", and a viewer is not owed that.
	PermAuditView       Permission = "audit.view"
	PermApproversManage Permission = "approvers.manage" // the tiers that can grant access
	PermAccessView      Permission = "access_requests.view"
	PermAccessClose     Permission = "access_requests.close"
	// A developer API key carries the authority of whoever minted it, so this permission is not
	// a widening: it says who may hand a script the reach they already have themselves.
	PermAPIKeysManage Permission = "api_keys.manage"
	// Fix jobs. Reading one means reading a diff and a log of somebody's repository; cancelling
	// one kills work in flight, which a read-only member should not be able to do.
	PermJobsView   Permission = "jobs.view"
	PermJobsManage Permission = "jobs.manage"
	// Spending the organisation's money: starting a checkout, buying credit, opening the card on
	// file. READING what it costs is every member's, like the API key list — the balance is the
	// answer to "why has the bot gone quiet", which is a question anybody may have. This is the
	// half that reaches a card, and among the built-ins admin alone holds it: the editor tier
	// exists to look after content without also being able to see what the bot spends.
	//
	// It does not let anybody move their own plan. That is the operator's or a verified webhook's,
	// by design, and there is a test.
	PermBillingManage Permission = "billing.manage"
)

// ALL_PERMISSIONS is the whole vocabulary. A custom role may name any of these and nothing else.
var allPermissions = []Permission{
	PermUsersManage, PermRolesManage, PermSettingsManage,
	PermConnManage, PermConnView, PermBundlesManage, PermScopesManage,
	PermDocsManage, PermMemoryManage, PermRoutinesManage,
	PermArtifactsView, PermArtifactsManage, PermActivityView, PermAuditView,
	PermApproversManage, PermAccessView, PermAccessClose, PermAPIKeysManage,
	PermJobsView, PermJobsManage, PermBillingManage,
}

func AllPermissions() []Permission { return append([]Permission{}, allPermissions...) }

// permissionKeys renders a resolved permission set as the console reads it: the keys that are
// on, in a stable order, rather than a map with every permission and a boolean.
func permissionKeys(set map[Permission]bool) []string {
	out := []string{}
	for _, p := range allPermissions {
		if set[p] {
			out = append(out, string(p))
		}
	}
	return out
}

// Built-in roles. Editor deliberately holds no credential and no user management: the point of
// having tiers at all is that somebody can look after documents, routines and channel settings
// without also being able to read what the bot spends to reach GitHub.
var builtinRoles = map[string][]Permission{
	"admin": allPermissions,
	"editor": {
		PermScopesManage, PermBundlesManage, PermDocsManage, PermMemoryManage,
		PermRoutinesManage, PermArtifactsManage, PermArtifactsView,
		PermActivityView, PermConnView, PermAccessView, PermAPIKeysManage,
		PermJobsView, PermJobsManage,
	},
	"viewer": {
		PermConnView, PermArtifactsView, PermActivityView, PermAccessView, PermJobsView,
	},
}

// The built-in role keys, named so a membership is never written as a bare string.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// BuiltinRoleKeys is the display order, most powerful first.
var BuiltinRoleKeys = []string{RoleAdmin, RoleEditor, RoleViewer}

func IsBuiltinRole(key string) bool { return builtinRoles[key] != nil }

// permissionsForRole resolves a role key to what it may do: built-in first, then the custom roles
// this install has defined, then nothing. Unknown keys grant nothing rather than falling back to
// a real tier.
func permissionsForRole(key string, custom map[string][]Permission) map[Permission]bool {
	out := map[Permission]bool{}
	list, ok := builtinRoles[key]
	if !ok {
		list, ok = custom[key]
	}
	if !ok {
		return out
	}
	for _, p := range list {
		out[p] = true
	}
	return out
}

// sanitizePermissions drops names this build does not know, so a custom role written against a
// newer version cannot smuggle a permission in as a bare string.
func sanitizePermissions(in []string) []Permission {
	out := []Permission{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if slices.Contains(allPermissions, p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// covers reports whether held includes everything in wanted.
func covers(held map[Permission]bool, wanted []Permission) bool {
	for _, p := range wanted {
		if !held[p] {
			return false
		}
	}
	return true
}

// canAssignRole is the rule behind handing out a role: you may only grant access you hold
// yourself. It takes the caller's permission set rather than their role key, because once custom
// roles exist a key no longer tells you what someone can do — and passing one was the shape that
// made "only an admin may grant admin" the only rule anybody could express. Stated as a subset it
// generalises to roles nobody has seen before: an editor cannot grant a custom role holding
// connections.manage, because they do not hold it.
func canAssignRole(held map[Permission]bool, target string, custom map[string][]Permission) bool {
	want := permissionsForRole(target, custom)
	if len(want) == 0 {
		return false // unknown role: refuse rather than grant an empty one
	}
	list := make([]Permission, 0, len(want))
	for p := range want {
		list = append(list, p)
	}
	return covers(held, list)
}

// criticalPermissions must never be left unheld: an install with nobody who can manage users, or
// nobody who can manage credentials, has locked itself out of its own console.
//
// The check is on the capability rather than on the literal role "admin", which stops being the
// right question the moment a custom role exists — an "Operations" role holding users.manage is a
// perfectly good administrator, and an install of those would have no admin to count.
var criticalPermissions = []Permission{PermUsersManage, PermConnManage}
