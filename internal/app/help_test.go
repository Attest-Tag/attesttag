package app

import (
	"context"
	"strings"
	"testing"
)

func manualFor(t *testing.T) string {
	t.Helper()
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
	return a.botManual(context.Background(), &Call{TeamID: "T1", OrgID: 1, Channel: "C1", ThreadTS: "1", UserID: "U1", Kind: "channel"})
}

// The question that started this, asked in a channel: "how do I change the model?". The manual
// has to carry the real answer — the thread command, the Configure page, and the fact that the
// page is not an admin-only door — because what the model says instead is a guess.
func TestManualAnswersHowToChangeTheModel(t *testing.T) {
	m := manualFor(t)
	for _, want := range []string{"!model advanced", "!model default", "Configure", "no workspace admin needed", "under Settings"} {
		if !strings.Contains(m, want) {
			t.Errorf("the manual never mentions %q", want)
		}
	}
}

// A manual read on Teams says where it is. Found live: it told the model it was in a Slack
// workspace, and offered a Slack search the Teams turn does not have.
func TestTheManualSaysWhichPlatformItIsOn(t *testing.T) {
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
	m := a.botManual(context.Background(), &Call{SL: &Chat{Platform: platformMSTeams}, TeamID: "msteams:t", OrgID: 1,
		Channel: "a:chat", ThreadTS: "chat", UserID: "u", Kind: "dm"})
	if !strings.Contains(m, "Microsoft Teams organisation") || strings.Contains(m, "Slack") {
		t.Errorf("the Teams manual reads:\n%s", m)
	}
}

// Everything else somebody asks a bot about itself, in the one document, so the answer is read
// rather than invented: how to quiet it, what it remembers, whose account it acts on, what it costs.
func TestManualCoversTheRestOfTheBot(t *testing.T) {
	m := manualFor(t)
	for _, want := range []string{"!mute", "this one thread", "!restart", "remember for this channel", "!notes", "!connect", "!usage", "!routines"} {
		if !strings.Contains(m, want) {
			t.Errorf("the manual never mentions %q", want)
		}
	}
}

// Two doors on one table: a command added to botCommands reaches the person who types the bang
// and the model answering for everybody who does not, without either list being updated by hand.
func TestBothDoorsListEveryCommand(t *testing.T) {
	help, manual := helpText(), manualFor(t)
	for _, c := range botCommands {
		for _, use := range c.Use {
			if !strings.Contains(help, "`"+use+"`") {
				t.Errorf("!help is missing %q", use)
			}
			if !strings.Contains(manual, "`"+use+"`") {
				t.Errorf("the manual is missing %q", use)
			}
		}
	}
}

// The prompt has to name the tool, or a model that has one is no better off than one that does not.
func TestPromptSendsSelfQuestionsToTheManual(t *testing.T) {
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
	c := &Call{TeamID: "T1", OrgID: 1, Channel: "C1", ThreadTS: "1", UserID: "U1", Kind: "channel", HumanTurn: true}
	if p := a.systemPrompt(context.Background(), c); !strings.Contains(p, "about_me") {
		t.Error("the system prompt never tells the model its manual exists")
	}
}

// "how do we change the model" is a "how do we…", which used to send it into the document corpus
// — where it found a Kubernetes capacity report and nothing else. The manual is checked first.
func TestForcedToolPrefersTheManualOverTheCorpus(t *testing.T) {
	for in, want := range map[string]string{
		"how to change model?":                          "about_me",
		"how do we change the model?":                   "about_me",
		"what can you do?":                              "about_me",
		"how do I mute you":                             "about_me",
		"how much do you cost":                          "about_me",
		"What is our refund policy? Check our docs.":    "search_docs",
		"Summarize what happened in this channel today": "read_channel_history",
		"which model of vacuum cleaner should I buy":    "",
	} {
		if got := forcedTool(in); got != want {
			t.Errorf("forcedTool(%q)=%q want %q", in, got, want)
		}
	}
}

// A scheduled run has nobody in the room to ask how the bot works, and pays for every definition
// it carries on every round, forever.
func TestManualIsNotCarriedByRoutines(t *testing.T) {
	_, _, chat := promptBudget(t, "channel", false)
	_, _, run := promptBudget(t, "routine", true)
	has := func(names []string) bool {
		for _, n := range names {
			if n == "about_me" {
				return true
			}
		}
		return false
	}
	if !has(chat) {
		t.Error("a channel turn cannot answer questions about the bot: about_me is not advertised")
	}
	if has(run) {
		t.Error("a routine is carrying the manual it has nobody to read it to")
	}
}
