package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The manual.
//
// A bot that cannot answer questions about itself sends people looking for it in the company's
// own documents. Asked "how do I change the model?", this one searched the corpus, found a
// Kubernetes capacity report, said the docs held nothing — and then guessed: that the model is
// set per workspace and that the Configure page needs a Slack admin. Both are wrong, and the
// right answer was one branch away in commands.go, behind a `!help` that most people will never
// type. So the commands live in one table here, `!help` prints it for whoever does type the
// bang, and about_me hands the same facts to the model for everybody who does not.

// botCommand is one bang command: the spellings that reach it, and what it does. The text is
// plain — no Slack emphasis — because two readers render it: `!help` posts it into a channel,
// and the manual passes it to a model that is asked to write standard Markdown back.
type botCommand struct {
	Use  []string
	What string
}

var botCommands = []botCommand{
	{[]string{"!help"}, "this list"},
	{[]string{"!whoami"}, "what I am here: my identity and workspace, my models, the time zone I schedule in, and the console's address"},
	{[]string{"!restart"}, "start this thread afresh: from then on I read only the messages after it"},
	{[]string{"!stop"}, "stop what I'm doing in this thread right now"},
	{[]string{"!mute", "!unmute"}, "stop/resume my replies in this thread only; every other thread and channel is unaffected"},
	{[]string{"!model <name>", "!model advanced", "!model default"}, "override the model for this thread"},
	{[]string{"!memory"}, "what I remember here"},
	{[]string{"!forget <words>"}, "delete matching memories"},
	{[]string{"!notes"}, "the notes I keep for you alone, sent to you privately"},
	{[]string{"!note <text>"}, "add one of those notes"},
	{[]string{"!routines"}, "scheduled routines in this channel"},
	{[]string{"!routine off <id>"}, "turn one off (on <id> turns it back on)"},
	{[]string{"!jobs"}, "fix jobs in this channel"},
	{[]string{"!job cancel <id>"}, "cancel one"},
	{[]string{"!ingest"}, "re-index the docs library"},
	{[]string{"!docs"}, "index stats"},
	{[]string{"!usage"}, "this month's spend by channel"},
	{[]string{"!access"}, "access requests you have waiting; !access cancel withdraws them"},
	{[]string{"!connect"}, "services here that run on your own account (calendar, mail, contacts): what each one lets you ask for, and a private link to connect it"},
	{[]string{"!personal_instructions"}, "how I should go about things when I act as your account (\"only ever look at my inbox\", \"sign my drafts off with my first name\"); clear removes them"},
}

// commandLines renders the table, one bullet per command.
func commandLines(bullet string) string {
	var b strings.Builder
	for _, c := range botCommands {
		uses := make([]string, len(c.Use))
		for i, u := range c.Use {
			uses[i] = "`" + u + "`"
		}
		fmt.Fprintf(&b, "%s %s — %s\n", bullet, strings.Join(uses, " / "), c.What)
	}
	return b.String()
}

// helpText is the reply to `!help`: the table, and the two things people do without a command.
func helpText() string {
	return "*Commands*\n" + commandLines("•") +
		"Mention me with a question in any channel I'm in, or DM me. Say *remember for this channel: …* to save a fact, " +
		"or *every weekday at 9am post …* to create a routine."
}

// botManual is what about_me returns: every fact the model would otherwise invent about itself,
// written for it to answer from. It is assembled per turn because half of it is this
// organisation's own — its name for the bot, whether an advanced model exists, which models a
// channel may pick, whether the fix worker is on, and the Configure link for this very channel.
func (a *Agent) botManual(ctx context.Context, c *Call) string {
	st := a.settings.Get(ctx, c.OrgID)
	name := st.BotName
	if name == "" {
		name = "the bot"
	}
	// Which platform this turn is on changes three sentences here, and saying Slack in Teams is the
	// one thing a manual must not get wrong about where it is.
	onTeams := c.SL != nil && c.SL.Platform == platformMSTeams
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: how I work\n\n", name)
	if onTeams {
		fmt.Fprintf(&b, "I am an AI teammate in this Microsoft Teams organisation. Mention me in a channel of a team I am in, "+
			"or message me in a chat; I read the conversation I am in before I answer, and I can search this organisation's "+
			"documents, read the web and use the services this channel is connected to. A message starting with `!` is a "+
			"command I handle myself, before any model sees it; everything else is an ordinary question. A team owner adds me "+
			"to a team from Apps in Teams, and removes me the same way.\n\n")
	} else {
		fmt.Fprintf(&b, "I am an AI teammate in this Slack workspace. Mention me in any channel I am in, or send me a direct "+
			"message; I read the thread I am in before I answer, and I can search this organisation's documents, search Slack, "+
			"read the web and use the services this channel is connected to. A message starting with `!` is a command I handle "+
			"myself, before any model sees it; everything else is an ordinary question. Somebody brings me into a channel by "+
			"inviting me to it in Slack, and takes me out the same way.\n\n")
	}
	where := "in Slack"
	if onTeams {
		where = "in Teams, by removing me from the team"
	}

	b.WriteString("## Which model answers\n")
	b.WriteString("- This thread, right now: `!model advanced` moves it to the stronger model, `!model default` puts it back, `!model <name>` names one.\n")
	if st.HeavyModel == "" {
		b.WriteString("  No advanced model is configured for this organisation yet, so `!model advanced` has nothing to switch to until an admin sets one in the console under Settings.\n")
	}
	if len(st.ChannelModels) > 0 {
		fmt.Fprintf(&b, "  Admins have also offered these by name here: %s.\n", strings.Join(st.ChannelModels, ", "))
	}
	b.WriteString("- This channel, for everybody: the **Configure** link at the bottom of my replies, under General → Model. " +
		"It opens without a sign-in and anyone who can read the channel can change it — no workspace admin needed — unless admins " +
		"have restricted edits to themselves, in which case the page says so and the fields are greyed out.\n")
	b.WriteString("- The whole organisation's default and advanced models: an admin sets both in the console, under Settings.\n")
	b.WriteString("Which model answered a turn is not something I print in the channel; the console's activity log has it.\n\n")

	b.WriteString("## Going quiet, stopping, starting over\n")
	b.WriteString("- `!mute` silences me in this one thread until somebody says `!unmute`. Every other thread and channel carries on. " +
		"There is no channel-wide mute and no global one: to quiet a whole channel, somebody removes me from it " + where + ".\n")
	b.WriteString("- `!stop` stops whatever I am doing in this thread right now. `!restart` forgets the thread's context so I only read messages from there on.\n\n")

	b.WriteString("## What I remember\n")
	b.WriteString("- Say \"remember for this channel: …\" and I keep the fact for everyone here; \"remember for the workspace: …\" " +
		"shares it with every conversation in this workspace, direct messages included, and can only be said in a public channel. `!memory` lists what I hold, `!forget <words>` deletes what matches, and the " +
		"Configure link's Memory tab edits it by hand.\n")
	b.WriteString("- `!notes` is separate and private: notes I keep for one person, sent to them in a DM and never shown in the channel. `!note <text>` adds one.\n")
	b.WriteString("- `!personal_instructions` is how you want me to go about things when I act as your own account.\n\n")

	b.WriteString("## Routines\n")
	b.WriteString("Ask in plain words — \"every weekday at 9am post the overnight errors\" — and I schedule it for this channel. " +
		"`!routines` lists them, `!routine off <id>` disables one, and the Configure link's Routines tab shows the same list.\n\n")

	b.WriteString("## Your own accounts\n")
	b.WriteString("Mail, calendar and contacts run on each person's own account, never a shared login. `!connect` tells you which " +
		"of those this channel has and sends you a private link to sign in with. An admin sets a connection up once; signing in is yours to do.\n\n")

	b.WriteString("## Documents\n")
	b.WriteString("What I search with search_docs is the library this organisation uploaded in the console, under Documents — " +
		"its own policies and runbooks. Nothing about me is in there, so a question about how I work is answered from this manual, not from a search. " +
		"`!docs` says how much is indexed and `!ingest` re-indexes it.\n\n")

	b.WriteString("## Asking for access\n")
	b.WriteString("Tag an approver in a channel with me and ask for the access you need: I write down the exact calls it would " +
		"take and send them a card to approve or refuse — I never grant it myself. `!access` lists what you have waiting, `!access cancel` withdraws it.\n\n")

	if a.jobs != nil && a.jobs.Enabled() {
		b.WriteString("## Changing code\n")
		b.WriteString("In a channel with a repository connected, ask me to fix a bug or make a change and I write the brief and " +
			"hand it to a worker: it clones the repository, makes the change and opens a draft pull request, and it waits for a " +
			"human to press Confirm before it starts. `!jobs` lists them here, `!job cancel <id>` stops one.\n\n")
	}

	b.WriteString("## What this costs\n")
	raise := "ask whoever runs this deployment"
	if e := a.cfg.SupportEmail; e != "" {
		raise = "email " + e + " or use the Upgrade button in the console"
	}
	fmt.Fprintf(&b, "Every turn costs the organisation a fraction of a cent. `!usage` shows this month's spend by channel against "+
		"the budget, and the console's Overview has the same in detail. On the free plan the budget is fixed; to raise it, %s.\n\n", raise)

	if link := a.configureURL(ctx, c.OrgID, c.TeamID, c.Channel); link != "" {
		fmt.Fprintf(&b, "## The Configure page for this channel\n%s — the same link that sits at the bottom of my replies. "+
			"Model, channel instructions, tools and access, memory and routines, no sign-in.\n\n", link)
	}

	b.WriteString("## Every command\n" + commandLines("-"))
	return b.String()
}

// registerHelpTools carries the manual to the model. It is one small definition against a
// failure that is otherwise certain: nothing in a model's training knows this bot's commands,
// so asked about them it answers plausibly and wrongly, in a channel, to somebody who then tries
// what it said.
func (a *Agent) registerHelpTools() {
	a.register(Tool{
		Name: "about_me",
		Desc: "Your own manual: what you can do, every `!` command, how to change the model you answer on, muting and stopping, " +
			"what you remember, routines, connecting a personal account, access requests, what a turn costs, and the Configure " +
			"page for this channel. Call it whenever someone asks about you or how to steer you — none of this is in search_docs.",
		Params: schema(map[string]any{}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			return a.botManual(ctx, c), nil
		},
	})
}
