package app

import (
	"context"
	"log/slog"
	"net/http"
)

// Deleting the account.
//
// "Account" is what the console calls an organisation — Settings names it, renames it and shows
// what it is called — so this is the screen a customer looks for when they want to be gone, and
// what it takes away is everything: the connected workspaces, the credentials, the documents,
// the threads the bot answered in, the members' consoles, and the sign-ins of anybody who had no
// other organisation to be in.
//
// Who may press it is not a permission, and that is deliberate. Permissions are things you hand
// to a role — an admin can invent a custom role holding any of them, which is right for every
// other act in the console and wrong for the single one that cannot be undone. So it rests on
// ownership instead: the person who signed the account up. They are the only one who can delete
// it while they are still a member, and when they are not — they left, and somebody removed them
// from Users — it falls to whoever administers the account now, so a departure does not leave an
// account nobody on earth can close.
//
// Deleting it still needs the same proof as taking the second factor off an account: a console
// left signed in on a desk is not authority to end the company's.

// whyNotDelete answers "may this person delete this account", in the words the console shows
// when the answer is no. Empty means yes.
func (b *Bot) whyNotDelete(ctx context.Context, me *AdminUser, org *Org) string {
	// An administrator in the sense the rest of the console uses: not the literal role name,
	// which stops being the question the moment somebody defines a role of their own, but the
	// capabilities an account cannot be left without.
	if me == nil || !covers(me.Permissions, criticalPermissions) {
		return "Deleting the account needs an administrator — someone who can manage both its people and its credentials."
	}
	if org == nil || org.CreatedBy == 0 || org.CreatedBy == me.ID {
		return ""
	}
	if m, _ := b.store.Membership(ctx, org.CreatedBy, org.ID); m == nil {
		return "" // the owner is no longer here; the administrators inherit this
	}
	owner := "the person who created it"
	if u, _ := b.store.User(ctx, org.CreatedBy); u != nil {
		owner = nonEmpty(u.Name, u.Email)
	}
	return "Only " + owner + " can delete this account: it was theirs to create. Ask them, or remove them from Users first if they have left."
}

// accountOwner is who made the account, for the screen that says so. A deployment that predates
// created_by, or an organisation whose founder's account has since gone, has none — the console
// says that rather than guessing at the oldest admin.
func (b *Bot) accountOwner(ctx context.Context, me *AdminUser, org *Org) map[string]any {
	if org == nil || org.CreatedBy == 0 {
		return nil
	}
	u, _ := b.store.User(ctx, org.CreatedBy)
	if u == nil {
		return nil
	}
	m, _ := b.store.Membership(ctx, org.CreatedBy, org.ID)
	return map[string]any{
		"id": u.PublicID, "name": u.Name, "email": u.Email,
		"is_you":    me != nil && me.ID == u.ID,
		"is_member": m != nil,
	}
}

// handleOrg is the account itself: what it is called, when it was made and by whom, how much of
// it there is, and whether the person asking may delete it. The Users tab already shows every
// member to every member, so naming the owner here tells nobody anything new.
func (b *Bot) handleOrg(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	org, err := b.store.Org(ctx, me.OrgID)
	if err != nil {
		fail(w, err)
		return
	}
	if org == nil {
		writeJSON(w, 404, map[string]any{"error": "no such organisation"})
		return
	}
	members, _ := b.store.MembersOf(ctx, org.ID)
	teams, _ := b.store.Teams(ctx, org.ID)
	live := 0
	for _, t := range teams {
		if t.Status == "active" {
			live++
		}
	}
	why := b.whyNotDelete(ctx, me, org)
	writeJSON(w, 200, map[string]any{
		"id": org.PublicID, "name": org.Name, "slug": org.Slug, "created_at": org.CreatedAt,
		"owner":      b.accountOwner(ctx, me, org),
		"members":    len(members),
		"workspaces": live,
		"can_delete": why == "",
		"why_not":    why,
		// What the delete will ask for, decided the same way proveIdentity decides it, so the
		// form puts up the field that will actually be accepted.
		"proof": b.proofKind(ctx, me),
	})
}

// handleDeleteOrg ends the account. Three things stand in front of it: the ownership rule, the
// account's name typed out in full, and proof that the person at the keyboard is the one the
// session belongs to.
func (b *Bot) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	org, err := b.store.Org(ctx, me.OrgID)
	if err != nil {
		fail(w, err)
		return
	}
	if org == nil {
		writeJSON(w, 404, map[string]any{"error": "no such organisation"})
		return
	}
	if why := b.whyNotDelete(ctx, me, org); why != "" {
		writeJSON(w, 403, map[string]any{"error": why})
		return
	}
	var in struct{ Confirm, Password, Code string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if !sameName(in.Confirm, org.Name) {
		writeJSON(w, 400, map[string]any{"error": "Type the account's name — " + org.Name + " — to confirm."})
		return
	}
	if ok, why := b.proveIdentity(ctx, me, in.Password, in.Code); !ok {
		writeJSON(w, 403, map[string]any{"error": why})
		return
	}

	// Stop what is still running before the rows it runs on go. A fix job carries on inside a
	// worker container of its own, and finishing one after this would push a branch to the
	// repository of somebody who has just asked to be forgotten.
	if n := b.jobs.CancelForOrg(ctx, org.ID, "the account was deleted"); n > 0 {
		slog.Info("cancelled jobs before deleting an account", "org", org.ID, "jobs", n)
	}
	// Hand the Slack tokens back. The rows are about to disappear either way; this is what makes
	// the app actually stop being installed in the workspace rather than merely forgotten here.
	teams, _ := b.store.Teams(ctx, org.ID)
	for _, t := range teams {
		if t.Status != "active" {
			continue
		}
		if err := b.disconnectTeam(ctx, t.TeamID, "the account was deleted"); err != nil {
			slog.Warn("workspace not disconnected before deletion", "team", t.TeamID, "err", err)
		}
	}
	// The documents are files in a folder or a bucket, not rows, so the sweep below cannot reach
	// them.
	b.eraseDocuments(ctx, org.ID)

	erased, err := b.store.DeleteOrg(ctx, org.ID)
	if err != nil {
		fail(w, err)
		return
	}
	// Only in-memory state from here: b.changed would write a settings row back for an
	// organisation that no longer exists.
	b.settings.Invalidate(org.ID)
	b.resolver.Invalidate(org.ID)
	if b.ix != nil {
		b.ix.Forget(org.ID)
	}
	slog.Warn("account deleted", "org", org.ID, "name", org.Name, "slug", org.Slug,
		"by", me.Email, "rows", erased.Rows, "accounts", erased.Accounts)

	// Every console this account had open is now a session row that no longer exists, including
	// this one. Clearing the cookies is what turns the next page load into the sign-in card
	// instead of a request that fails.
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true, "rows": erased.Rows, "accounts": erased.Accounts})
}

// eraseDocuments empties one organisation's document folder, wherever documents live for this
// deployment. Best effort by design: a file the bucket will not let go of is worth a line in the
// log, not a refusal to delete the account it belongs to.
func (b *Bot) eraseDocuments(ctx context.Context, orgID int64) {
	if b.docs == nil {
		return
	}
	store := b.docs.For(orgID)
	docs, err := store.List(ctx)
	if err != nil {
		slog.Warn("documents not erased", "org", orgID, "err", err)
		return
	}
	for _, d := range docs {
		if err := store.Delete(ctx, d.Path); err != nil {
			slog.Warn("document not erased", "org", orgID, "path", d.Path, "err", err)
		}
	}
}
