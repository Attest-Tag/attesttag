package review

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Verb is what a comment addressed to the bot asks for.
type Verb string

const (
	VerbReview     Verb = "review"      // review the head, if it has not been already
	VerbFullReview Verb = "full_review" // review from scratch, ignoring what earlier runs found
	VerbStatus     Verb = "status"      // explain the current score from stored state, no model call
	VerbHelp       Verb = "help"
	VerbPause      Verb = "pause"    // stop the reviews nobody asks for on this pull request
	VerbResume     Verb = "resume"   // start them again, with a fresh allowance before the next pause
	VerbFix        Verb = "fix"      // fix open findings with a commit pushed to the pull request's branch
	VerbQuestion   Verb = "question" // anything else: free text for the bot to answer
	// VerbRecheck is "look at this again": in a finding's thread, that finding at the head; on the
	// conversation, which names no finding, a review of the head.
	VerbRecheck Verb = "recheck"
	// VerbUnknown is one word that is no command — "@bot approve", "@bot full" — which is more likely
	// a command misremembered than a question, and is pointed at help rather than answered.
	VerbUnknown Verb = "unknown"
	// VerbThanks is thanks and nothing else, which needs no answer.
	VerbThanks Verb = "thanks"
)

// Command is a parsed request from a pull request comment.
type Command struct {
	Verb Verb
	// Types are the review type keys named after "review", lowercased and deduplicated, e.g.
	// "@bot review security tests". Empty means the matching branch rule's types. They are
	// only shaped like keys; whether the organisation has such a type is for the caller.
	Types []string
	// Severities are the severities named right after "fix", e.g. "@bot fix p0 p1": which of the
	// open findings a fix on the conversation is for. Empty means all of them. In a finding's
	// thread the finding is the one fixed, whatever they say.
	Severities []Severity
	// Text is the comment from just after the mention to its end, as written. For a question
	// it is the question, and it may run on past the first line.
	Text string
	// Bare is a mention with nothing after it but punctuation: somebody saying hello, or finding
	// out what the bot is, who is answered with the short help rather than the whole of it.
	Bare bool
}

// maxCommandTypes bounds how many type keys a command may name before the line is read as
// prose instead: nobody types eleven review types, but a sentence has eleven words.
const maxCommandTypes = 10

// ParseCommand reads a pull request comment for a command to the bot whose App slug is slug,
// and reports whether there is one.
//
// Only the first line that is neither blank nor quoted counts, and it must begin with the
// mention. A comment that quotes an earlier "@bot review" in a "> " line, or mentions the bot
// halfway through a sentence, or in a code block, is talking about the bot, not to it, and
// answering it would spend money nobody asked to spend.
//
// The mention must end at a space, a comma, a colon or the end of the line, which is the
// regular expression ^@slug(?:[\s,:]|$), case-insensitively. A word boundary (\b) is not
// enough: "@slug-dev review" has one after "@slug", and a production App would then answer
// every command meant for a development App installed on the same repository. "email@slug" never
// matches, as the mention has to start the line. "@slug[bot]", the App's actual login, is
// accepted too, since that is what people copy from the bot's own comments.
//
// A bare mention is a request for help rather than a review: it costs nothing, and the help
// says how to ask for one. Its case does not matter, as for any mention.
func ParseCommand(body, slug string) (Command, bool) {
	slug = strings.TrimPrefix(strings.TrimSpace(slug), "@")
	if slug == "" {
		return Command{}, false
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ">") {
			continue
		}
		rest, ok := afterMention(line, slug)
		if !ok {
			return Command{}, false
		}
		text := strings.TrimSpace(strings.Join(append([]string{rest}, lines[i+1:]...), "\n"))
		cmd := classify(rest)
		if (cmd.Verb == VerbUnknown || cmd.Verb == VerbThanks) && strings.Contains(text, "\n") {
			// One word on the mention's line and more below it is somebody writing a question across
			// lines, not a command misremembered nor only thanks.
			cmd.Verb = VerbQuestion
		}
		cmd.Text = text
		return cmd, true
	}
	return Command{}, false
}

// afterMention returns what follows "@slug" on line, past the separators, if line starts with
// the mention.
func afterMention(line, slug string) (string, bool) {
	mention := "@" + slug
	if len(line) < len(mention) || !strings.EqualFold(line[:len(mention)], mention) {
		return "", false
	}
	rest := line[len(mention):]
	if len(rest) >= len("[bot]") && strings.EqualFold(rest[:len("[bot]")], "[bot]") {
		rest = rest[len("[bot]"):]
	}
	if rest == "" {
		return "", true
	}
	if r, _ := utf8.DecodeRuneInString(rest); r != ',' && r != ':' && !unicode.IsSpace(r) {
		return "", false
	}
	return strings.TrimLeftFunc(rest, func(r rune) bool { return r == ',' || r == ':' || unicode.IsSpace(r) }), true
}

// whyScore is the other way people ask for the status: "why is the score 3?", "why only a
// 2/5 score".
var whyScore = regexp.MustCompile(`(?i)^why\b.*\bscore`)

// commandFiller are words that read naturally around a command and change nothing: "review
// this PR please", "review again, thanks", "start the review of this pull request".
var commandFiller = map[string]bool{
	"please": true, "pls": true, "plz": true, "this": true, "the": true, "pr": true,
	"it": true, "again": true, "now": true, "and": true, "thanks": true, "thank": true, "you": true,
	"a": true, "an": true, "of": true, "for": true, "pull": true, "request": true,
}

// recheckWords may follow a review or check verb and still ask for one finding to be looked at
// again — "review this thread again and update the confidence" — rather than name review types.
var recheckWords = map[string]bool{"thread": true, "finding": true, "comment": true, "update": true, "confidence": true,
	"score": true, "its": true}

// thanksWords are a comment that only thanks the bot: "thanks!", "thank you so much".
var thanksWords = map[string]bool{"thanks": true, "thank": true, "you": true, "thx": true, "ty": true, "cheers": true,
	"so": true, "much": true, "a": true, "lot": true, "great": true, "nice": true}

// classify reads the rest of the mention line.
func classify(rest string) Command {
	if whyScore.MatchString(rest) {
		return Command{Verb: VerbStatus}
	}
	words := strings.FieldsFunc(strings.ToLower(rest), func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	verb := func(i int) string {
		if i >= len(words) {
			return ""
		}
		return strings.TrimRight(words[i], ".!?:")
	}
	switch {
	case only(words): // nothing but punctuation, or nothing at all
		return Command{Verb: VerbHelp, Bare: true}
	case verb(0) == "help":
		return Command{Verb: VerbHelp}
	case verb(0) == "status":
		return Command{Verb: VerbStatus}
	case (verb(0) == "score" || verb(0) == "confidence") && only(words[1:], commandFiller):
		// What people call the status: the score is what it is about.
		return Command{Verb: VerbStatus}
	case verb(0) == "pause" && aboutReviews(words[1:]):
		return Command{Verb: VerbPause}
	case (verb(0) == "resume" || verb(0) == "unpause") && aboutReviews(words[1:]):
		return Command{Verb: VerbResume}
	case (verb(0) == "recheck" || verb(0) == "re-check" || verb(0) == "check") && only(words[1:], commandFiller, recheckWords):
		return Command{Verb: VerbRecheck}
	case reviewWord(verb(0)) && slices.ContainsFunc(words[1:], func(w string) bool { return w == "thread" || w == "finding" }) &&
		only(words[1:], commandFiller, recheckWords):
		// "review this thread": one finding, not the pull request, and not a type called "thread".
		return Command{Verb: VerbRecheck}
	case verb(0) == "full" && reviewWord(verb(1)):
		return withTypes(VerbFullReview, words[2:])
	case verb(0) == "full-review":
		return withTypes(VerbFullReview, words[1:])
	case reviewWord(verb(0)):
		return withTypes(VerbReview, words[1:])
	case verb(0) == "please" && reviewWord(verb(1)):
		return withTypes(VerbReview, words[2:])
	case verb(0) == "start" || verb(0) == "begin" || verb(0) == "run":
		// "start the review", "start a review of this pr", "run the review": a review, once the
		// words between the verb and "review" are filler.
		i := 1
		for i < len(words) && commandFiller[verb(i)] {
			i++
		}
		if reviewWord(verb(i)) || verb(i) == "reviewing" {
			return withTypes(VerbReview, words[i+1:])
		}
	case verb(0) == "fix":
		return withSeverities(words[1:])
	case verb(0) == "please" && verb(1) == "fix":
		return withSeverities(words[2:])
	}
	switch {
	case only(words, thanksWords):
		return Command{Verb: VerbThanks}
	case len(words) == 1 && !strings.Contains(words[0], "?"):
		// Every command that is one word was matched above: this one is none of them.
		return Command{Verb: VerbUnknown}
	}
	return Command{Verb: VerbQuestion}
}

// only reports whether every word, its trailing punctuation aside, is in one of the sets.
func only(words []string, sets ...map[string]bool) bool {
	for _, w := range words {
		w = strings.TrimRight(w, ".!?:")
		if w == "" {
			continue
		}
		if !slices.ContainsFunc(sets, func(set map[string]bool) bool { return set[w] }) {
			return false
		}
	}
	return true
}

// withSeverities reads the severities named at the start of the words after "fix": "fix p0 p1",
// "fix all". Unlike a review's types, anything else that follows does not turn the line into a
// question — "fix this, keep the old name" is a fix, and the rest of the comment travels to the
// worker as the asker's own words (Command.Text) — so only the leading words are read, and the
// first that is neither a severity nor filler ends them.
func withSeverities(words []string) Command {
	cmd := Command{Verb: VerbFix}
	for _, w := range words {
		w = strings.TrimRight(w, ".!,:")
		switch sev := Severity(strings.ToUpper(w)); {
		case w == "" || commandFiller[w] || fixFiller[w]:
		case sev.Valid():
			if !slices.Contains(cmd.Severities, sev) {
				cmd.Severities = append(cmd.Severities, sev)
			}
		default:
			return cmd
		}
	}
	return cmd
}

// fixFiller are words around "fix" that name no severity and change nothing: "fix all findings",
// "fix everything".
var fixFiller = map[string]bool{"all": true, "everything": true, "findings": true, "finding": true, "these": true, "them": true,
	"that": true, "open": true, "issues": true, "issue": true}

func reviewWord(w string) bool { return w == "review" || w == "rereview" || w == "re-review" }

// pauseWords are what may follow pause or resume and still be the command: "pause reviews",
// "resume automatic reviews please".
var pauseWords = map[string]bool{"review": true, "reviews": true, "reviewing": true, "automatic": true, "auto": true}

// aboutReviews reports whether the words after pause or resume say nothing more than which reviews.
// Anything else — "pause until the release is out" — is a sentence, and a question is answered
// rather than half of it obeyed: pausing is cheap to undo, but a person who wrote a condition
// expects the condition to be kept.
func aboutReviews(words []string) bool {
	for _, w := range words {
		if w = strings.TrimRight(w, ".!"); w != "" && !commandFiller[w] && !pauseWords[w] {
			return false
		}
	}
	return true
}

// withTypes reads the words after a review verb as type keys. If any of them cannot be one —
// "review the error handling in store.go" — the line was a sentence, not a list, and it is
// answered as a question rather than refused as a command with a bad type. A question mark
// is not stripped for the same reason: "review is this safe?" is a question.
func withTypes(v Verb, words []string) Command {
	var types []string
	for _, w := range words {
		w = strings.TrimRight(w, ".!")
		if w == "" || commandFiller[w] {
			continue
		}
		if !ValidTypeKey(w) {
			return Command{Verb: VerbQuestion}
		}
		if !slices.Contains(types, w) {
			types = append(types, w)
		}
	}
	if len(types) > maxCommandTypes {
		return Command{Verb: VerbQuestion}
	}
	return Command{Verb: v, Types: types}
}
