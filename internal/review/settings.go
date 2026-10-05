package review

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mode is whether a repository is reviewed and where the result goes. Shadow runs the whole
// review and records it in the console without writing a word to GitHub, which is how a team
// compares this reviewer with the one it already has before letting it speak.
type Mode string

const (
	ModeOff    Mode = "off"
	ModeShadow Mode = "shadow"
	ModeLive   Mode = "live"
)

func (m Mode) Valid() bool { return m == ModeOff || m == ModeShadow || m == ModeLive }

// Trigger is what starts a review without anybody asking: nothing (only a command does), a
// pull request being opened, or every push to it. A command and the console's Start review
// work whatever this says, because a person asked.
type Trigger string

const (
	TriggerCommand Trigger = "command"
	TriggerOpen    Trigger = "open"
	TriggerPush    Trigger = "push"
)

func (t Trigger) Valid() bool { return t == TriggerCommand || t == TriggerOpen || t == TriggerPush }

// Forks is what a pull request from a fork gets. Its content is a stranger's, and reviewing it
// spends the organisation's money, so it is never reviewed automatically: at most a member's
// command starts one.
type Forks string

const (
	ForksCommand Forks = "command"
	ForksOff     Forks = "off"
)

func (f Forks) Valid() bool { return f == ForksCommand || f == ForksOff }

// Strictness is how sure a finding must be before it is posted, and so how many get through.
// High is fewer, surer comments.
type Strictness string

const (
	StrictnessLow    Strictness = "low"
	StrictnessMedium Strictness = "medium"
	StrictnessHigh   Strictness = "high"
)

func (s Strictness) Valid() bool {
	return s == StrictnessLow || s == StrictnessMedium || s == StrictnessHigh
}

// NotifyChannel is the chat channel a pull request's reviews and its merge are announced in: one
// channel of one connected workspace — a Slack channel, or a Teams one — named by the workspace's
// team id and the channel's id, the pair the chat layer posts with. Ids rather than a row's serial,
// so the value means the same channel in an export, a copy and an API call. The zero value is "no
// channel": set at a level, it turns off a channel set further up, as an empty comment header does.
// Which channels an organisation has is the API's to check; this holds only the shape.
type NotifyChannel struct {
	Team    string `json:"team,omitempty"`
	Channel string `json:"channel,omitempty"`
}

// Set reports whether n names a channel to post in.
func (n NotifyChannel) Set() bool { return n.Channel != "" }

// NotifyEvent is one kind of news a pull request's channel can be told: a review starting, a review
// finishing, one failing — or not running for a reason the team needs to hear, a budget spent, the
// pull request paused, a plan without code review — and the pull request being merged.
type NotifyEvent string

const (
	NotifyStarted  NotifyEvent = "started"
	NotifyFinished NotifyEvent = "finished"
	NotifyFailed   NotifyEvent = "failed"
	NotifyMerged   NotifyEvent = "merged"
)

// NotifyEvents is every event, in the order the console lists them: what a channel is told when no
// level says otherwise.
func NotifyEvents() []NotifyEvent {
	return []NotifyEvent{NotifyStarted, NotifyFinished, NotifyFailed, NotifyMerged}
}

func (e NotifyEvent) Valid() bool { return slices.Contains(NotifyEvents(), e) }

// maxNotifyID bounds a team or channel id. Slack's are a dozen characters and a Teams channel's
// "19:…@thread.tacv2" under a hundred; the cap is for a value that is not an id at all.
const maxNotifyID = 200

// Validate checks the shape: a channel, or nothing; never a workspace on its own, which would read
// as set and post nowhere.
func (n NotifyChannel) Validate() error {
	var errs []string
	if n.Channel == "" && n.Team != "" {
		errs = append(errs, "a workspace with no channel names nowhere to post")
	}
	for _, f := range []struct{ name, v string }{{"team", n.Team}, {"channel", n.Channel}} {
		if len(f.v) > maxNotifyID {
			errs = append(errs, fmt.Sprintf("%s is longer than %d characters", f.name, maxNotifyID))
		}
		if strings.ContainsFunc(f.v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			errs = append(errs, fmt.Sprintf("%s %q is not an id", f.name, clip(f.v)))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Level is where in the settings tree a value was set. A connection is one GitHub App
// installation; a group is a set of its repositories a team made in the console; a repository
// is the leaf. Default is the built-in value under all of them, and rule is a branch rule's
// override, applied after the tree has been resolved (see Effective.WithRule).
type Level string

const (
	LevelDefault    Level = "default"
	LevelConnection Level = "connection"
	LevelGroup      Level = "group"
	LevelRepo       Level = "repo"
	LevelRule       Level = "rule"
)

// Settings is one level's own values. Every field is optional and an absent one inherits from
// the level above, so the type is all pointers and nil-able lists, and its JSON form leaves an
// unset key out — that is what lets the console show "Inherit: High · from Frontend" next to a
// field and offer Reset on one that is set here. A JSON null reads as unset too, which is how
// the console resets a field.
//
// Two kinds of field inherit differently. A single value is taken from the nearest level that
// sets it, so a repository can override its group. The lists add up instead, broadest
// first: a connection's "skip these authors" still applies to a repository that adds one more,
// since a list that replaced its parent's would make every repository restate the
// organisation's basics, and the first one to forget would quietly lose them. An empty list
// therefore inherits like an absent one. Branch rules are the exception: an ordered list where
// the first match wins cannot be merged meaningfully, so the nearest level that has any rules
// supplies all of them.
type Settings struct {
	Mode          *Mode       `json:"mode,omitempty"`
	Trigger       *Trigger    `json:"trigger,omitempty"`
	Drafts        *bool       `json:"drafts,omitempty"` // whether a draft pull request is reviewed automatically
	Forks         *Forks      `json:"forks,omitempty"`
	Strictness    *Strictness `json:"strictness,omitempty"`
	MaxComments   *int        `json:"max_comments,omitempty"` // inline comments per review
	CommentHeader *string     `json:"comment_header,omitempty"`
	Model         *string     `json:"model,omitempty"`
	MaxUSD        *float64    `json:"max_usd,omitempty"` // the most one review may spend, verification included
	// Notify is the channel told about each review and about the merge (an empty object: none,
	// over a channel set above). Nothing in it reaches the model or GitHub.
	Notify *NotifyChannel `json:"notify,omitempty"`
	// NotifyOn is which events that channel hears of. A set, but taken whole from the nearest level
	// that sets one, as a single value is: a list that added up could never let a repository's
	// channel hear less than its connection's. An empty one tells the channel nothing, the channel
	// kept. Which news a channel gets is review's noise, not its reach: reviews.manage may change it.
	NotifyOn *[]NotifyEvent `json:"notify_on,omitempty"`

	Instructions   []string `json:"instructions,omitempty"`    // what the team wants checked, one per entry
	ExcludeAuthors []string `json:"exclude_authors,omitempty"` // login globs never reviewed automatically
	ReviewBots     []string `json:"review_bots,omitempty"`     // bot login globs reviewed automatically all the same; other bots are skipped
	IgnorePaths    []string `json:"ignore_paths,omitempty"`    // path globs left out of every review
	ContextRepos   []string `json:"context_repos,omitempty"`   // owner/name of the organisation's own repositories the reviewer may read

	BranchRules []BranchRule `json:"branch_rules,omitempty"`
}

// The bounds Validate holds a level's settings to. Money and comment counts are bounded so a
// typo cannot make one review cost fifty dollars or post two hundred comments; the list caps
// keep what reaches the prompt, three levels of it, to a size a review can afford to send.
const (
	MinMaxUSD      = 0.10
	MaxMaxUSD      = 5.0
	MinMaxComments = 1
	MaxMaxComments = 20
	MaxListEntries = 50
	MaxEntryLen    = 400
	MaxHeaderLen   = 400
)

// Defaults is the built-in value of every single-valued setting, the level under the
// connection. A connection added to review starts in shadow, reviewing pull requests as they
// open, with the general review on every branch: nothing is posted until somebody decides it
// should be, and nothing costs more than a dollar a review.
func Defaults() Settings {
	return Settings{
		Mode:          ptr(ModeShadow),
		Trigger:       ptr(TriggerOpen),
		Drafts:        ptr(false),
		Forks:         ptr(ForksCommand),
		Strictness:    ptr(StrictnessMedium),
		MaxComments:   ptr(8),
		CommentHeader: ptr(""),
		Model:         ptr("heavy"),
		MaxUSD:        ptr(1.00),
		Notify:        ptr(NotifyChannel{}),
		NotifyOn:      ptr(NotifyEvents()),
		BranchRules:   []BranchRule{{Types: []string{DefaultType}}},
	}
}

func ptr[T any](v T) *T { return &v }

// LevelSettings is one level's settings and which level it is.
type LevelSettings struct {
	Level    Level
	Settings Settings
}

// Effective is what will actually run for a repository: every field has a value, and Source
// says where each came from, by its JSON name. For a list, Source is the nearest level that
// added an entry, or "default" when no level did.
type Effective struct {
	Mode          Mode       `json:"mode"`
	Trigger       Trigger    `json:"trigger"`
	Drafts        bool       `json:"drafts"`
	Forks         Forks      `json:"forks"`
	Strictness    Strictness `json:"strictness"`
	MaxComments   int        `json:"max_comments"`
	CommentHeader string     `json:"comment_header"`
	Model         string     `json:"model"`
	MaxUSD        float64    `json:"max_usd"`
	// omitzero for Hash's sake: it zeroes the channel, and a zero one left out keeps the bytes
	// hashed what they were before there was a channel at all, so adding it invalidated no cached
	// review (TestEffectiveHashIsPinned). The console reads a channel left out as none.
	Notify NotifyChannel `json:"notify,omitzero"`
	// Never nil once resolved — empty is "nothing" — and omitzero for Hash's sake, as Notify: Hash
	// sets it nil, so the bytes hashed are what they were before there was a list to leave out.
	NotifyOn []NotifyEvent `json:"notify_on,omitzero"`

	Instructions   []string `json:"instructions"`
	ExcludeAuthors []string `json:"exclude_authors"`
	ReviewBots     []string `json:"review_bots,omitzero"` // omitzero for Hash's sake, as Notify: adding it missed no cached review
	IgnorePaths    []string `json:"ignore_paths"`
	ContextRepos   []string `json:"context_repos"`

	BranchRules []BranchRule `json:"branch_rules"`

	Source map[string]Level `json:"source,omitempty"`
}

// Notifies reports whether the channel is told of ev. An Effective nothing resolved — one built in
// code — has no list at all, and tells every event, as a tree that sets none does.
func (e Effective) Notifies(ev NotifyEvent) bool {
	return e.NotifyOn == nil || slices.Contains(e.NotifyOn, ev)
}

// Resolve folds a chain of levels, broadest first — connection, then a group if the
// repository is in one, then the repository — over the built-in defaults. The chain is taken
// in the order given; building it in the right order is the caller's job, since only the
// caller knows the tree.
func Resolve(chain []LevelSettings) Effective {
	e := Effective{
		Instructions:   []string{},
		ExcludeAuthors: []string{},
		ReviewBots:     []string{},
		IgnorePaths:    []string{},
		ContextRepos:   []string{},
		Source:         map[string]Level{},
	}
	for _, name := range []string{"instructions", "exclude_authors", "review_bots", "ignore_paths", "context_repos"} {
		e.Source[name] = LevelDefault
	}
	levels := append([]LevelSettings{{Level: LevelDefault, Settings: Defaults()}}, chain...)
	for _, ls := range levels {
		s, lv := ls.Settings, ls.Level
		pick(&e.Mode, s.Mode, "mode", lv, e.Source)
		pick(&e.Trigger, s.Trigger, "trigger", lv, e.Source)
		pick(&e.Drafts, s.Drafts, "drafts", lv, e.Source)
		pick(&e.Forks, s.Forks, "forks", lv, e.Source)
		pick(&e.Strictness, s.Strictness, "strictness", lv, e.Source)
		pick(&e.MaxComments, s.MaxComments, "max_comments", lv, e.Source)
		pick(&e.CommentHeader, s.CommentHeader, "comment_header", lv, e.Source)
		pick(&e.Model, s.Model, "model", lv, e.Source)
		pick(&e.MaxUSD, s.MaxUSD, "max_usd", lv, e.Source)
		pick(&e.Notify, s.Notify, "notify", lv, e.Source)
		if s.NotifyOn != nil {
			// A copy, and never nil: an empty set is "nothing", where nil would read as every event.
			e.NotifyOn, e.Source["notify_on"] = append([]NotifyEvent{}, *s.NotifyOn...), lv
		}

		addUp(&e.Instructions, s.Instructions, false, "instructions", lv, e.Source)
		// GitHub logins and repository names are case-insensitive, so "Octocat" added below
		// "octocat" is not a second entry.
		addUp(&e.ExcludeAuthors, s.ExcludeAuthors, true, "exclude_authors", lv, e.Source)
		addUp(&e.ReviewBots, s.ReviewBots, true, "review_bots", lv, e.Source)
		addUp(&e.IgnorePaths, s.IgnorePaths, false, "ignore_paths", lv, e.Source)
		addUp(&e.ContextRepos, s.ContextRepos, true, "context_repos", lv, e.Source)

		if len(s.BranchRules) > 0 {
			e.BranchRules = cloneRules(s.BranchRules)
			e.Source["branch_rules"] = lv
		}
	}
	return e
}

func pick[T any](dst *T, v *T, field string, lv Level, source map[string]Level) {
	if v != nil {
		*dst = *v
		source[field] = lv
	}
}

// addUp appends the entries of add that dst does not already hold, keeping the first
// spelling and the order, and credits the level when it contributed anything.
func addUp(dst *[]string, add []string, foldCase bool, field string, lv Level, source map[string]Level) {
	key := func(s string) string {
		if foldCase {
			return strings.ToLower(s)
		}
		return s
	}
	for _, v := range add {
		v = strings.TrimSpace(v)
		if v == "" || slices.ContainsFunc(*dst, func(d string) bool { return key(d) == key(v) }) {
			continue
		}
		*dst = append(*dst, v)
		source[field] = lv
	}
}

func cloneRules(rules []BranchRule) []BranchRule {
	out := make([]BranchRule, len(rules))
	for i, r := range rules {
		r.Labels = slices.Clone(r.Labels)
		r.Types = slices.Clone(r.Types)
		if r.Notify != nil {
			n := *r.Notify
			r.Notify = &n
		}
		out[i] = r
	}
	return out
}

// WithRule applies a matched branch rule's overrides to the resolved settings and credits
// them to LevelRule. r is one of e.BranchRules, so it was written at e.Source["branch_rules"].
//
// The nearest level still wins. Rules are inherited as a whole list, so the rule usually comes
// from the connection, and a rule there that said "post live" would otherwise overrule a
// repository somebody explicitly set to shadow — posting to GitHub where a person chose that
// nothing should be — or put a repository set to "command" to save money back on every push.
// So each override applies only where the rule list is at least as near as the value it would
// replace: a rule written beside the value, or below it, is the more specific choice; a rule
// from further up is not. A rule's Post never turns on a repository whose mode is off,
// whichever level wrote it: off says the repository is not reviewed at all, and a rule only
// chooses how a review is published. The copy shares nothing with e.
func (e Effective) WithRule(r BranchRule) Effective {
	out := e
	out.Instructions = slices.Clone(e.Instructions)
	out.ExcludeAuthors = slices.Clone(e.ExcludeAuthors)
	out.ReviewBots = slices.Clone(e.ReviewBots)
	out.IgnorePaths = slices.Clone(e.IgnorePaths)
	out.ContextRepos = slices.Clone(e.ContextRepos)
	out.NotifyOn = slices.Clone(e.NotifyOn)
	out.BranchRules = cloneRules(e.BranchRules)
	out.Source = maps.Clone(e.Source)
	if out.Source == nil {
		out.Source = map[string]Level{}
	}
	ruleAt := levelRank(e.Source["branch_rules"])
	overrides := func(field string) bool { return ruleAt >= levelRank(e.Source[field]) }
	if r.Trigger != "" && overrides("trigger") {
		out.Trigger, out.Source["trigger"] = r.Trigger, LevelRule
	}
	if r.Strictness != "" && overrides("strictness") {
		out.Strictness, out.Source["strictness"] = r.Strictness, LevelRule
	}
	if r.Model != "" && overrides("model") {
		out.Model, out.Source["model"] = r.Model, LevelRule
	}
	if r.Post != "" && e.Mode != ModeOff && overrides("mode") {
		out.Mode, out.Source["mode"] = r.Post, LevelRule
	}
	// The same nearness for the channel: a repository somebody pointed at its own channel is not
	// moved to another by a connection-wide rule. An empty one silences the pull requests it
	// matches, as the empty value does at a level.
	if r.Notify != nil && overrides("notify") {
		out.Notify, out.Source["notify"] = *r.Notify, LevelRule
	}
	return out
}

// levelRank orders the levels of the tree from the broadest up: a higher rank is nearer the
// repository. A level nobody recorded counts as the default, the broadest there is.
func levelRank(l Level) int {
	switch l {
	case LevelConnection:
		return 1
	case LevelGroup:
		return 2
	case LevelRepo:
		return 3
	case LevelRule:
		return 4
	}
	return 0
}

// ExcludesAuthor reports whether login matches one of the author globs, case-insensitively as
// GitHub compares logins.
func (e Effective) ExcludesAuthor(login string) bool {
	login = strings.ToLower(login)
	for _, p := range e.ExcludeAuthors {
		if Match(strings.ToLower(p), login) {
			return true
		}
	}
	return false
}

// ReviewsBot reports whether a bot's pull requests are reviewed automatically: whether login matches
// one of the bot globs, case-insensitively. An App's account is "name[bot]", the login its pull
// requests carry, and people write it either way, so the suffix is optional on both sides —
// "dependabot" lets dependabot[bot] through, as "dependabot[bot]" does — and "*" lets every bot.
func (e Effective) ReviewsBot(login string) bool {
	name := strings.TrimSuffix(strings.ToLower(login), "[bot]")
	for _, p := range e.ReviewBots {
		if p = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(p)), "[bot]"); p != "" && Match(p, name) {
			return true
		}
	}
	return false
}

// IgnoresPath reports whether a changed file is left out of the review.
func (e Effective) IgnoresPath(path string) bool { return MatchAny(e.IgnorePaths, path) }

// Hash identifies what will run, for a run to record: a later change to the settings then
// never rewrites what an earlier review was done under, and two runs on the same head with the
// same hash can be answered from the first one's result. Source is left out — where a value
// came from does not change what it does — and so is the channel told about it, and what it is
// told, at every level and in every rule: pointing the announcements somewhere else changes no
// review, and must not make the next request on a reviewed head pay for that review again. So are
// the bots let through, which decide whether a review runs, not what it finds.
func (e Effective) Hash() string {
	e.Source = nil
	e.Notify, e.NotifyOn = NotifyChannel{}, nil
	e.ReviewBots = nil
	e.BranchRules = cloneRules(e.BranchRules)
	for i := range e.BranchRules {
		e.BranchRules[i].Notify = nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		// Only a NaN or an infinity in MaxUSD can fail here, and Validate refuses both; a
		// fallback that still distinguishes settings beats a constant hash that would make two
		// different configurations look the same.
		b = fmt.Appendf(nil, "%#v", e)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SettingFields lists every setting by its JSON name, in declaration order.
func SettingFields() []string {
	return []string{"mode", "trigger", "drafts", "forks", "strictness", "max_comments",
		"comment_header", "model", "max_usd", "notify", "notify_on", "instructions", "exclude_authors",
		"review_bots", "ignore_paths", "context_repos", "branch_rules"}
}

// ChangedFields lists the JSON names of the settings that differ between two versions of one
// level, sorted: what an update actually changes, which is what has to be checked against who
// may change it. A field set to the value it already had is not a change, so an editor can
// save a form that shows an admin-only value without being refused for it.
func ChangedFields(before, after Settings) []string {
	a, errA := fieldValues(before)
	b, errB := fieldValues(after)
	if errA != nil || errB != nil {
		// Cannot compare (a NaN, which JSON never decodes to): say everything changed, so the
		// caller's permission check errs towards asking for more.
		return slices.Sorted(slices.Values(SettingFields()))
	}
	var changed []string
	for _, k := range SettingFields() {
		if !bytes.Equal(a[k], b[k]) {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}

func fieldValues(s Settings) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	err = json.Unmarshal(raw, &m)
	return m, err
}

// loginGlob is a GitHub login, or a glob of one: letters, digits and hyphens, '*' and '?',
// with an optional "[bot]" suffix for an App's account, e.g. "renovate[bot]" or "*-bot".
var loginGlob = regexp.MustCompile(`^[A-Za-z0-9*?][A-Za-z0-9*?-]*(?:\[bot\])?$`)

// Validate reports everything wrong with one level's settings, joined, in terms of the JSON
// field names so the console can put each message beside its field. Unset fields are not
// checked: they inherit, and the levels they inherit from were checked when they were saved.
func (s Settings) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if s.Mode != nil && !s.Mode.Valid() {
		bad("mode %q: want off, shadow or live", clip(string(*s.Mode)))
	}
	if s.Trigger != nil && !s.Trigger.Valid() {
		bad("trigger %q: want command, open or push", clip(string(*s.Trigger)))
	}
	if s.Forks != nil && !s.Forks.Valid() {
		bad("forks %q: want command or off", clip(string(*s.Forks)))
	}
	if s.Strictness != nil && !s.Strictness.Valid() {
		bad("strictness %q: want low, medium or high", clip(string(*s.Strictness)))
	}
	if s.MaxComments != nil && (*s.MaxComments < MinMaxComments || *s.MaxComments > MaxMaxComments) {
		bad("max_comments must be between %d and %d", MinMaxComments, MaxMaxComments)
	}
	if v := s.MaxUSD; v != nil && (math.IsNaN(*v) || *v < MinMaxUSD || *v > MaxMaxUSD) {
		bad("max_usd must be between $%.2f and $%.2f", MinMaxUSD, MaxMaxUSD)
	}
	// An empty model is not "inherit" — that is leaving the key out — and would run nothing.
	if s.Model != nil && !validModelName(*s.Model) {
		bad("model %q is not a model name", clip(*s.Model))
	}
	// An empty header is allowed and meaningful: it turns off a header set further up.
	if s.CommentHeader != nil && utf8.RuneCountInString(*s.CommentHeader) > MaxHeaderLen {
		bad("comment_header must be at most %d characters", MaxHeaderLen)
	}
	// So is an empty channel, for the same reason.
	if s.Notify != nil {
		if err := s.Notify.Validate(); err != nil {
			bad("notify: %v", err)
		}
	}
	// And an empty set of events: the channel kept, told nothing.
	if s.NotifyOn != nil {
		if len(*s.NotifyOn) > len(NotifyEvents()) {
			bad("notify_on: at most %d events", len(NotifyEvents()))
		} else {
			for i, ev := range *s.NotifyOn {
				switch {
				case !ev.Valid():
					bad("notify_on %q: want started, finished, failed or merged", clip(string(ev)))
				case slices.Contains((*s.NotifyOn)[:i], ev):
					bad("notify_on names %s twice", ev)
				}
			}
		}
	}

	checkList := func(field string, list []string, ok func(string) bool, want string) {
		if len(list) > MaxListEntries {
			bad("%s: at most %d entries, got %d", field, MaxListEntries, len(list))
		}
		for _, v := range list {
			switch {
			case strings.TrimSpace(v) == "":
				// An empty glob matches everything, so an empty ignore_paths entry would
				// silently ignore every file. Refused in every list for the same reason.
				bad("%s: an entry is empty", field)
			case utf8.RuneCountInString(v) > MaxEntryLen:
				bad("%s: %q is longer than %d characters", field, clip(v), MaxEntryLen)
			case ok != nil && !ok(strings.TrimSpace(v)):
				bad("%s: %q is not %s", field, clip(v), want)
			}
		}
	}
	checkList("instructions", s.Instructions, nil, "")
	checkList("exclude_authors", s.ExcludeAuthors, loginGlob.MatchString, "a GitHub login or a glob of one")
	checkList("review_bots", s.ReviewBots, loginGlob.MatchString, "a GitHub login or a glob of one")
	checkList("ignore_paths", s.IgnorePaths, validPathGlob, "a path glob")
	checkList("context_repos", s.ContextRepos, validRepoName, "owner/name")

	if err := ValidateRules(s.BranchRules); err != nil {
		errs = append(errs, fmt.Errorf("branch_rules: %w", err))
	}
	return errors.Join(errs...)
}

// validPathGlob refuses what cannot be meant as a path pattern: control characters, and a
// bare "/", which reads as "the root" but matches no file at all.
func validPathGlob(s string) bool {
	return s != "/" && !strings.ContainsFunc(s, unicode.IsControl)
}
