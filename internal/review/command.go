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
	VerbQuestion   Verb = "question" // anything else: free text for the bot to answer
)

// Command is a parsed request from a pull request comment.
type Command struct {
	Verb Verb
	// Types are the review type keys named after "review", lowercased and deduplicated, e.g.
	// "@bot review security tests". Empty means the matching branch rule's types. They are
	// only shaped like keys; whether the organisation has such a type is for the caller.
	Types []string
	// Text is the comment from just after the mention to its end, as written. For a question
	// it is the question, and it may run on past the first line.
	Text string
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
// says how to ask for one.
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
// this PR please", "review again, thanks".
var commandFiller = map[string]bool{
	"please": true, "pls": true, "plz": true, "this": true, "the": true, "pr": true,
	"it": true, "again": true, "now": true, "and": true, "thanks": true, "thank": true, "you": true,
}

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
	case len(words) == 0 || verb(0) == "" || verb(0) == "help":
		return Command{Verb: VerbHelp}
	case verb(0) == "status":
		return Command{Verb: VerbStatus}
	case verb(0) == "pause" && aboutReviews(words[1:]):
		return Command{Verb: VerbPause}
	case (verb(0) == "resume" || verb(0) == "unpause") && aboutReviews(words[1:]):
		return Command{Verb: VerbResume}
	case verb(0) == "full" && reviewWord(verb(1)):
		return withTypes(VerbFullReview, words[2:])
	case verb(0) == "full-review":
		return withTypes(VerbFullReview, words[1:])
	case reviewWord(verb(0)):
		return withTypes(VerbReview, words[1:])
	case verb(0) == "please" && reviewWord(verb(1)):
		return withTypes(VerbReview, words[2:])
	}
	return Command{Verb: VerbQuestion}
}

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
