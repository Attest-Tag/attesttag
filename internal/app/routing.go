package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
)

// ---- which model answers ----

// chooseModel picks the model for a turn: the thread's override, else the default of the narrowest
// level that set one — the channel, its workspace, the organisation — else Settings' default. The
// console offers a default model at all three levels; only the channel's used to be read, so a
// default saved for a whole workspace was stored, shown back, and never answered on.
//
// Nothing here escalates on its own. The advanced model used to be routed to by heuristic — an
// ask that matched a code-shaped regexp, a thread past 25 messages or 20k characters, a thread
// that had already run eight tools — so a conversation changed voice and price partway through,
// on a rule nobody in it could see and the count behind it never fell. It is now spent only
// where someone asked for it: a fix job (which runs on worker_model and falls back to it), a
// channel whose default model is "heavy", or a thread that said !model advanced. Until one of
// those says otherwise, every turn answers on the model the channel was already answering on.
func (a *Agent) chooseModel(ctx context.Context, c *Call) (model, why string) {
	st := a.settings.Get(ctx, c.OrgID)
	if c.Session != nil && c.Session.Model != "" {
		why := "thread override"
		if c.Kind == "routine" {
			// A routine's session is made per run, carrying the model its editor picked.
			why = "routine"
		}
		return resolveHeavy(c.Session.Model, st), why
	}
	if m, level := a.store.InheritedDefaultModel(ctx, c.OrgID, c.TeamID, c.Channel); m != "" {
		return resolveHeavy(m, st), map[string]string{"channel": "channel default", "team": "workspace default",
			"workspace": "organisation default"}[level]
	}
	return st.Model, "default"
}

// inheritedDefault is what a channel that names no model of its own answers on: its workspace's
// default, else the organisation's, else Settings'. The Configure page labels its Default choice
// with it, because that is what choosing Default gives.
func inheritedDefault(ctx context.Context, store *Store, st Settings, orgID int64, teamID string) string {
	if m, _ := store.InheritedDefaultModel(ctx, orgID, teamID, ""); m != "" {
		return resolveHeavy(m, st)
	}
	return st.Model
}

// resolveHeavy turns the "heavy" sentinel into a model. With no heavy model configured it is the
// default model: the sentinel itself used to go to the provider as a model id, which no provider
// has ever served.
func resolveHeavy(model string, st Settings) string {
	if model != "heavy" {
		return model
	}
	if st.HeavyModel != "" {
		return st.HeavyModel
	}
	return st.Model
}

// modelLabel names the tier a turn answered on, for a reply everyone in the channel reads.
//
// The footer used to carry the provider's model id — "glm-5.3-flash" — which says nothing to
// the people in the thread, dates the moment the workspace switches models, and tells anyone
// who can see the channel which vendor is underneath. What a reader actually wants from it is
// how much muscle the answer was given, and that has two plain names — basic, and advanced —
// one of which the product already uses at every other switch. A model an admin pinned by name
// is neither, so it says custom rather than claiming to be the basic one.
//
// The real ids are still an admin's to see, in Settings, the Configure page and the console's
// usage tables; this is only what is said out loud in Slack.
func modelLabel(st Settings, model string) string {
	switch {
	case model != "" && model == st.HeavyModel:
		return "advanced"
	case model == st.Model || st.Model == "":
		return "basic"
	}
	return "custom"
}

// routineModelAllowed is whether a routine may be set to answer on m: the default, the
// advanced model where one is configured, or one of the models Settings offers to channels.
// The same rule the channel page applies, for the same reason — the console lets an admin
// name any model for a channel, but a routine is a standing bill, so it picks from the list.
func routineModelAllowed(st Settings, m string) bool {
	switch m {
	case "":
		return true
	case "heavy":
		return st.HeavyModel != ""
	}
	return slices.Contains(st.ChannelModels, m)
}

// errRoutineModel is what a caller is told when it names a model no routine may run on. One
// sentence in one place, because both routine writers — the editor's save and a creation —
// refuse for the same reason and should say the same thing.
func errRoutineModel(m string) error {
	return fmt.Errorf("%q is not a model routines may use: pick the default, Advanced, or one of the models offered to channels under Settings", m)
}

// routineSessionModel is what a routine's run puts on its session so chooseModel answers on
// it. "heavy" with no advanced model configured would otherwise be sent to the provider as a
// model id, so it reads as the default instead.
func routineSessionModel(st Settings, m string) string {
	if m == "heavy" && st.HeavyModel == "" {
		return ""
	}
	return m
}

// ---- how long one turn may dig ----

// A tool round is one model call plus whatever tools it asked for. Twelve was right for a
// question in a chat thread and wrong for one that means reading logs: a triage spends rounds
// three at a time — search, narrow, cross-check — and the run used to die on the cap holding
// everything it had found. Raising it costs nothing on the replies that do not need it, because
// almost every turn answers inside three rounds and stops; the cap was only ever reached by the
// turns doing real work, and reaching it is what made them worse. So the number is a scope
// setting, inherited channel → workspace → account, and the ceiling exists only so a typo
// cannot bill a month of tokens to one thread.
const (
	maxRoundsCeiling = 80
	// What a run that carries its own budget may be given. A routine or an investigation is not
	// a reply: it was scheduled rather than typed, nobody is waiting on it, and the number came
	// from an operator in Settings rather than from whoever happened to be in a channel. So the
	// ceiling over it is the one that stops a runaway loop, not the one that stops a typo — and
	// the two are far apart, because the work these are written for (read a system, cross-check
	// it, write the result back) spends rounds by the hundred.
	maxRunRounds = 200
	// What one round is worth in wall-clock time. The model call plus a 45s tool timeout fits
	// inside this comfortably. Multiplied out the default rounds run past TURN_MAX_MINUTES, so
	// what a turn that digs actually gets is that ceiling rather than this arithmetic, and a
	// turn that does not dig is bounded by its rounds long before either.
	roundWall = 20 * time.Second
	// A round is only started with this much clock left — one tool call alone may take all of
	// it — and below that the turn stops digging and writes.
	roundReserve = 45 * time.Second
	// What one thread may spend on tool calls, counted across every turn in it. The pair to the
	// rounds a reply gets: two calls a round is what a turn that searches and cross-checks
	// actually spends, and a hundred leaves the second question in a thread its own tools rather
	// than refusing them because the first question was expensive.
	threadToolCalls = 100
	// What the writing itself gets, on a clock of its own: a turn that spent its whole wall
	// digging must still be able to answer, or the budget meant to bound the search would
	// silently swallow the result of it.
	answerWall = 90 * time.Second
	// The spend ceiling for a provider that reports no cost at all. TURN_MAX_USD is the real
	// limit and this is its backstop, because a ceiling that silently does nothing whenever
	// usage arrives without a price is not a ceiling. Eight million prompt tokens is about
	// what fifty cents buys on the models these runs use, and it is four times the largest
	// turn in eight days of production that was doing useful work.
	turnMaxPromptTokens = 8_000_000
)

// outOfRoundsNote is what the model is told on its last round, as its tools are withdrawn.
const outOfRoundsNote = "You have used the tool budget for this question. Answer now, from what you already have. " + answerFromWhatYouHave

// outOfSpendNote is the same for a turn stopped by what it was costing rather than by how long
// it had been going. The model is not told the figure: it cannot act on it, the number is the
// operator's business, and a reply that explains it was too expensive to finish is worse for
// the person reading than one that simply says what was found and what was not.
const outOfSpendNote = "You have used the budget for this question. Answer now, from what you already have. " + answerFromWhatYouHave

// overSpent reports whether a turn has reached the ceiling on what one turn may cost. Cost is
// preferred and tokens are the fallback, never both: a provider that prices its own usage is
// more accurate than any arithmetic here, and one that does not would otherwise be unbounded.
func overSpent(u Usage, ceiling float64) bool {
	if ceiling <= 0 {
		return false
	}
	if u.CostUSD > 0 {
		return u.CostUSD >= ceiling
	}
	return u.In >= turnMaxPromptTokens
}

// answerFromWhatYouHave is the second half of every note that lands a turn — out of rounds,
// out of clock, or repeating itself (stuckNote): the shape of answer wanted once the tools
// are gone.
const answerFromWhatYouHave = "Say what you found and what it rests on, and be explicit about anything you could not check — a partial answer with its gaps named is what is wanted here. " +
	"Do not ask to run another tool: there are none left this turn."

// noToolsNote goes with the one repeat a landing gets when the provider ignored tool_choice
// "none" and answered with a call and no words. Asked again with the tools gone, a model in the
// middle of per-item posts wrote the next post out as <tool_call> markup instead, which is
// stripped whole before anything is shown — so this says in so many words that no call can
// happen now and where the text it meant to send has to go.
const noToolsNote = "Your tools are gone for the rest of this turn: a tool call you write now is never run and nobody sees it, even written out as text. " +
	"Put what you meant to post or send into this reply, as plain prose — it is the only thing that will be posted."

// roundsFor is how many tool rounds this turn may spend. A run carrying its own budget — an
// investigation — has already been given one and is not second-guessed here.
func (a *Agent) roundsFor(ctx context.Context, c *Call, st Settings) int {
	// A run that brought its own number was already bounded where that number was decided; the
	// scope ceiling here is about channel settings and would silently cut it back to 80.
	if c.MaxRounds > 0 {
		return min(c.MaxRounds, maxRunRounds)
	}
	return min(max(a.scopeRounds(ctx, c, st.MaxToolRounds), 1), maxRoundsCeiling)
}

// scopeRounds reads the narrowest scope that names a number; 0 anywhere means inherit, which is
// how every scope starts. Each link is read only if the one before it inherited, so a deployment
// that has never set this pays one lookup per turn and no more.
func (a *Agent) scopeRounds(ctx context.Context, c *Call, def int) int {
	if a.store == nil {
		return def
	}
	links := []func() *Scope{
		func() *Scope { sc, _ := a.store.ChannelScope(ctx, c.OrgID, c.TeamID, c.Channel); return sc },
		func() *Scope { sc, _ := a.store.TeamScope(ctx, c.OrgID, c.TeamID); return sc },
		func() *Scope { sc, _ := a.store.AccountScope(ctx, c.OrgID); return sc },
	}
	for _, link := range links {
		if sc := link(); sc != nil && sc.MaxToolRounds > 0 {
			return sc.MaxToolRounds
		}
	}
	return def
}

// turnWall is the clock a turn gets, derived from the rounds it may spend rather than fixed at
// four minutes: a channel allowed thirty rounds cannot use them inside a budget sized for twelve.
// The ceiling is the operator's, and applies to every turn.
func turnWall(rounds int, ceiling time.Duration) time.Duration {
	w := time.Duration(rounds) * roundWall
	if w < 2*time.Minute {
		w = 2 * time.Minute
	}
	if ceiling > 0 && w > ceiling {
		w = ceiling
	}
	return w
}

// roomForAnotherRound reports whether there is time to call a tool and still answer. A context
// with no deadline always has room.
func roomForAnotherRound(ctx context.Context) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) > roundReserve
}

// ---- limits: per-user rate limit and per-channel budget ----

// allowed reports whether a turn may run, with a user-facing reason when not.
func (a *Agent) allowed(ctx context.Context, c *Call) (bool, string) {
	st := a.settings.Get(ctx, c.OrgID)
	if st.UserRateLimit > 0 && c.UserID != "" && (c.SL == nil || c.UserID != c.SL.BotUserID) {
		if n := a.store.TurnsByUserSince(ctx, c.TeamID, c.UserID, time.Hour); n >= st.UserRateLimit {
			return false, fmt.Sprintf("you've used %d of %d requests this hour; try again a bit later", n, st.UserRateLimit)
		}
	}
	// One instance serves every organisation. A cap on turns in flight per organisation is
	// what keeps one workspace's burst from taking the CPU and memory of all the others.
	if max := a.cfg.PlatformMaxInFlightPerOrg; max > 0 {
		if n := a.inFlight(c.OrgID); n >= max {
			return false, fmt.Sprintf("this organisation already has %d requests running; try again in a moment", n)
		}
	}
	if sc, _ := a.store.ChannelScope(ctx, c.OrgID, c.TeamID, c.Channel); sc != nil && sc.MonthlyBudgetUSD > 0 {
		spent, err := a.store.MonthSpend(ctx, c.OrgID, c.TeamID, c.Channel)
		if err != nil {
			return false, "spend accounting is unavailable, so I've stopped here for now"
		}
		if spent >= sc.MonthlyBudgetUSD {
			a.alert(ctx, c.OrgID, "budget:"+c.Channel, fmt.Sprintf(":moneybag: <#%s> has used its monthly budget ($%.2f of $%.2f). Raise it in the console under Workspaces → the channel.", c.Channel, spent, sc.MonthlyBudgetUSD))
			return false, fmt.Sprintf("this channel's monthly budget of $%.2f is used up", sc.MonthlyBudgetUSD)
		}
	}
	if ok, why := a.budgetOK(ctx, c.OrgID, c.TeamID); !ok {
		a.alert(ctx, c.OrgID, "budget:"+c.TeamID, ":moneybag: Spending stopped: "+why)
		return false, why
	}
	return true, ""
}

// ---- alerts ----

// alert posts an operational alert to the configured channel, at most once per key per hour.
func (a *Agent) alert(ctx context.Context, orgID int64, key, text string) {
	if !a.store.AlertOnce(ctx, orgID, key, time.Hour) {
		return
	}
	a.postAlert(ctx, orgID, text)
}

// postAlert is the same without the once-per-window check, for a caller that has already taken
// the token. The low-credit warning does: it sends a mail and posts in the room off ONE AlertOnce,
// so an account is told once by two routes rather than twice by two windows (billing.go).
func (a *Agent) postAlert(ctx context.Context, orgID int64, text string) {
	ch := a.settings.Get(ctx, orgID).AlertChannel
	if ch == "" {
		return
	}
	// The alert channel is one channel in one workspace. Which one is recorded on its scope
	// row, so the alert goes out on that workspace's token rather than an arbitrary one.
	team, err := a.store.ChannelTeam(ctx, orgID, ch)
	if err != nil || team == "" {
		slog.Warn("alert not sent: the alert channel is not in any connected workspace", "channel", ch)
		return
	}
	sl, err := a.slacks.For(ctx, team)
	if err != nil {
		slog.Warn("alert not sent", "channel", ch, "err", err)
		return
	}
	if _, err := sl.PostText(ctx, ch, "", text); err != nil {
		slog.Warn("alert post failed", "err", err)
	}
}

func shortHost(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.Index(u, "/"); i > 0 {
		u = u[:i]
	}
	return u
}

// imagesFor settles what a turn that carries images sends its model. The turn keeps its model
// either way. When the provider's catalogue says that model takes images they go as they are;
// when it says it does not, they are dropped and the model gets their file names instead,
// because sending them anyway fails the whole turn (OpenRouter answers 404 "No endpoints found
// that support image input", which is what a long thread with a screenshot in it used to get
// once it escalated to a text-only heavy model). A model the catalogue does not describe is
// given the images.
//
// l is the endpoint the turn runs on: its catalogue is the one that says what takes images.
func imagesFor(ctx context.Context, l *LLM, model, why string, msgs []openai.ChatCompletionMessageParamUnion) (string, []openai.ChatCompletionMessageParamUnion) {
	if ok, _ := l.acceptsImages(ctx, model); ok {
		return why, msgs
	}
	slog.Info("images attached to a text-only model; sending their names", "model", model)
	return why + ", images sent by name", dropImages(msgs)
}

// dropImages removes the image parts from user messages, keeping the text (which already names
// each attached image) and adding a note that the model could not see them.
func dropImages(msgs []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		u := m.OfUser
		if u == nil || len(u.Content.OfArrayOfContentParts) == 0 {
			out = append(out, m)
			continue
		}
		var b strings.Builder
		dropped := false
		for _, p := range u.Content.OfArrayOfContentParts {
			switch {
			case p.OfText != nil:
				b.WriteString(p.OfText.Text)
			case p.OfImageURL != nil:
				dropped = true
			}
		}
		if dropped {
			b.WriteString("\n[the attached image could not be shown to this model]")
		}
		out = append(out, openai.UserMessage(b.String()))
	}
	return out
}
