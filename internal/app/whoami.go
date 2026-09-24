package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// whoami answers `!whoami`: what the bot is in this workspace — its Slack identity, the model
// it answers with, and the time zone it schedules in.
//
// It deliberately says nothing about the machine. Anyone in any connected workspace can type
// !whoami, and in a multi-tenant deployment those people are other companies' employees: the
// host, the kernel, the build, the database path, the storage bucket, the model key's
// fingerprint and the count of every workspace on the deployment are the operator's facts, not
// this tenant's.
func (a *Agent) whoami(ctx context.Context, c *Call) string {
	var b strings.Builder
	st := a.settings.Get(ctx, c.OrgID)
	b.WriteString("*What I am*\n")
	ws := c.TeamID
	if c.SL != nil && c.SL.TeamName != "" {
		ws = c.SL.TeamName + " (`" + c.TeamID + "`)"
	}
	bot := ""
	if c.SL != nil {
		bot = c.SL.BotUserID
	}
	// This organisation's workspaces. ActiveTeams counts the whole deployment, which told every
	// tenant how many other companies are on it.
	if teams, _ := a.store.Teams(ctx, c.OrgID); len(teams) > 1 {
		ws += fmt.Sprintf(", one of %d workspaces you have connected", len(teams))
	}
	fmt.Fprintf(&b, "• Bot: %s (<@%s>) in workspace %s\n", a.cfg.BotName, bot, ws)
	fmt.Fprintf(&b, "• Model: `%s`", st.Model)
	if st.HeavyModel != "" {
		fmt.Fprintf(&b, " · heavy `%s`", st.HeavyModel)
	}
	b.WriteString("\n")
	// Where the models above are asked, when that is somewhere this organisation chose. The host
	// only: a channel can hold guests, and the rest of the key is nobody's business here.
	switch k := st.OwnKey; {
	case k.Active():
		fmt.Fprintf(&b, "• Model key: this organisation's own, at %s\n", k.Ref.Host())
	case k.Present:
		fmt.Fprintf(&b, "• Model key: this organisation's own, at %s — not in use: %s\n", k.Ref.Host(), k.refusal())
	}
	loc := a.locFor(st)
	fmt.Fprintf(&b, "• Time zone: %s · now %s\n", loc, time.Now().In(loc).Format("Mon 2 Jan 15:04 MST"))
	if u := os.Getenv("ADMIN_BASE_URL"); u != "" {
		fmt.Fprintf(&b, "• Console: %s/admin/", strings.TrimRight(u, "/"))
	} else if u := a.store.PublicOrigin(context.Background()); u != "" {
		fmt.Fprintf(&b, "• Console: %s/admin/ (learned from the console; ADMIN_BASE_URL overrides)", u)
	} else if u := localBaseURL(a.cfg.HealthAddr); u != "" {
		fmt.Fprintf(&b, "• Console: %s/admin/ (no public origin yet: footer links only work on this machine until someone signs in to the console)", u)
	} else {
		fmt.Fprintf(&b, "• Console: listening on `%s`", a.cfg.HealthAddr)
	}
	return b.String()
}
