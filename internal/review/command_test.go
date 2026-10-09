package review

import (
	"slices"
	"testing"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		ok    bool
		verb  Verb
		types []string
	}{
		{"plain", "@slug review", true, VerbReview, nil},
		{"any case", "@SLUG Review", true, VerbReview, nil},
		{"rereview", "@slug rereview", true, VerbReview, nil},
		{"re-review", "@slug re-review", true, VerbReview, nil},
		{"review please", "@slug review please", true, VerbReview, nil},
		{"please review", "@slug please review", true, VerbReview, nil},
		{"review this PR again", "@slug review this PR again, thanks!", true, VerbReview, nil},
		{"comma after the mention", "@slug, review", true, VerbReview, nil},
		{"colon after the mention", "@slug: review", true, VerbReview, nil},
		{"tab after the mention", "@slug\treview", true, VerbReview, nil},
		{"trailing full stop", "@slug review.", true, VerbReview, nil},
		{"trailing question mark", "@slug review?", true, VerbReview, nil},
		{"the App's own login", "@slug[bot] review", true, VerbReview, nil},
		{"indented", "   @slug review", true, VerbReview, nil},
		{"after blank lines, with more below", "\n\n@slug review\nThe tests are in a separate commit.", true, VerbReview, nil},
		{"CRLF body", "\r\n@slug review\r\nthanks", true, VerbReview, nil},
		{"after a quote", "> Older totals can overwrite newer ones\n\n@slug review", true, VerbReview, nil},
		{"a quoted command does not count, the next line does", "> @slug review\n@slug status", true, VerbStatus, nil},

		{"types by space", "@slug review security tests", true, VerbReview, []string{"security", "tests"}},
		{"types by comma", "@slug review security,tests", true, VerbReview, []string{"security", "tests"}},
		{"types with and, any case", "@slug review Security and tests", true, VerbReview, []string{"security", "tests"}},
		{"a type twice", "@slug review security security", true, VerbReview, []string{"security"}},
		{"a hyphenated key", "@slug review api-contract please", true, VerbReview, []string{"api-contract"}},

		{"full review", "@slug full review", true, VerbFullReview, nil},
		{"full re-review with a type", "@slug full re-review security", true, VerbFullReview, []string{"security"}},
		{"full-review", "@slug full-review", true, VerbFullReview, nil},

		{"status", "@slug status", true, VerbStatus, nil},
		{"status?", "@slug status?", true, VerbStatus, nil},
		{"why the score", "@slug why is the score 3?", true, VerbStatus, nil},
		{"why only this score", "@slug Why only a 2/5 score", true, VerbStatus, nil},

		{"help", "@slug help", true, VerbHelp, nil},
		{"a bare mention is help, not a paid review", "@slug", true, VerbHelp, nil},
		{"a bare mention with punctuation", "@slug ?", true, VerbHelp, nil},

		{"pause", "@slug pause", true, VerbPause, nil},
		{"pause reviews please", "@slug pause automatic reviews, please", true, VerbPause, nil},
		{"resume", "@slug resume", true, VerbResume, nil},
		{"unpause reviews", "@slug unpause reviews.", true, VerbResume, nil},
		{"pause with a condition is a question", "@slug pause until the release is out", true, VerbQuestion, nil},
		{"resume with more to say is a question", "@slug resume tomorrow", true, VerbQuestion, nil},

		{"a question", "@slug what does this function do?", true, VerbQuestion, nil},
		{"a sentence that starts with review", "@slug review the error handling in store.go", true, VerbQuestion, nil},
		{"a question that starts with review", "@slug review is this safe?", true, VerbQuestion, nil},
		{"why without score", "@slug why did you flag this", true, VerbQuestion, nil},
		{"full on its own is no command", "@slug full", true, VerbUnknown, nil},
		{"too many words to be types", "@slug review b c d e f g h i j k l", true, VerbQuestion, nil},

		// What people actually type for the commands they mean.
		{"score is the status", "@slug score", true, VerbStatus, nil},
		{"confidence? is the status", "@slug Confidence?", true, VerbStatus, nil},
		{"a sentence about the score is a question", "@slug score seems low for a typo fix", true, VerbQuestion, nil},
		{"start review", "@slug start review", true, VerbReview, nil},
		{"start the review", "@slug start the review", true, VerbReview, nil},
		{"start the review of pr", "@slug start the review of pr", true, VerbReview, nil},
		{"start a review of this pull request", "@slug start a review of this pull request please", true, VerbReview, nil},
		{"start reviewing", "@slug start reviewing", true, VerbReview, nil},
		{"review this", "@slug review this", true, VerbReview, nil},
		{"review again", "@slug review again", true, VerbReview, nil},
		{"review this pull request", "@slug review this pull request", true, VerbReview, nil},
		{"review for a type", "@slug review for security", true, VerbReview, []string{"security"}},
		{"recheck", "@slug recheck", true, VerbRecheck, nil},
		{"re-check this finding", "@slug re-check this finding", true, VerbRecheck, nil},
		{"check again", "@slug check again", true, VerbRecheck, nil},
		{"check on its own", "@slug check", true, VerbRecheck, nil},
		{"review this thread", "@slug review this thread", true, VerbRecheck, nil},
		{"review this thread again and update the confidence", "@slug review this thread again and update the confidence", true, VerbRecheck, nil},
		{"check something is a question", "@slug check this for races", true, VerbQuestion, nil},
		{"thanks", "@slug thanks!", true, VerbThanks, nil},
		{"thank you so much", "@slug thank you so much", true, VerbThanks, nil},
		{"one unknown word", "@slug approve", true, VerbUnknown, nil},
		{"one unknown word, any case", "@slug LGTM.", true, VerbUnknown, nil},
		{"one word asked is a question", "@slug why?", true, VerbQuestion, nil},
		{"a bare mention in capitals", "@SLUG", true, VerbHelp, nil},
		{"a bare mention with a bang", "@Slug !", true, VerbHelp, nil},

		// Not commands.
		{"another App's slug sharing a prefix", "@slug-dev review", false, "", nil},
		{"an email address", "email@slug review", false, "", nil},
		{"no separator", "@slugreview", false, "", nil},
		{"underscore after the slug", "@slug_x review", false, "", nil},
		{"mentioned mid-sentence", "please @slug review", false, "", nil},
		{"only quoted", "> @slug review", false, "", nil},
		{"in a code block", "```\n@slug review\n```", false, "", nil},
		{"after other text", "Thanks!\n@slug review", false, "", nil},
		{"someone else", "@octocat review", false, "", nil},
		{"empty", "", false, "", nil},
		{"blank", "  \n\t\n", false, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, ok := ParseCommand(c.body, "slug")
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (%+v)", ok, c.ok, cmd)
			}
			if cmd.Verb != c.verb || !slices.Equal(cmd.Types, c.types) {
				t.Errorf("got %s %q, want %s %q", cmd.Verb, cmd.Types, c.verb, c.types)
			}
		})
	}
}

// The development App's commands must reach the development App only, and the other way
// round: a word boundary would let both answer.
func TestParseCommandKeepsSlugsThatShareAPrefixApart(t *testing.T) {
	if _, ok := ParseCommand("@slug-dev review", "slug-dev"); !ok {
		t.Error("the dev App should answer its own mention")
	}
	if _, ok := ParseCommand("@slug review", "slug-dev"); ok {
		t.Error("the dev App answered the production App's mention")
	}
	if _, ok := ParseCommand("@slug review", "@slug"); !ok {
		t.Error("a slug passed with its @ should still match")
	}
	if _, ok := ParseCommand("@ review", ""); ok {
		t.Error("an empty slug matches nothing")
	}
}

func TestParseCommandKeepsTheTextAsWritten(t *testing.T) {
	cmd, ok := ParseCommand("> Missing tenant check\n@slug, Is this really reachable?\r\nThe handler is behind auth.\n", "slug")
	if !ok || cmd.Verb != VerbQuestion {
		t.Fatalf("got %+v, %v", cmd, ok)
	}
	if want := "Is this really reachable?\nThe handler is behind auth."; cmd.Text != want {
		t.Errorf("Text = %q, want %q", cmd.Text, want)
	}
	cmd, _ = ParseCommand("@slug", "slug")
	if cmd.Text != "" {
		t.Errorf("a bare mention has no text, got %q", cmd.Text)
	}
}

// A bare mention is somebody saying hello, answered with the short help; "help" asks for all of it.
func TestParseCommandTellsABareMentionFromHelp(t *testing.T) {
	for _, body := range []string{"@slug", "@SLUG", "  @slug ?", "@slug[bot]", "@slug:"} {
		if cmd, ok := ParseCommand(body, "slug"); !ok || cmd.Verb != VerbHelp || !cmd.Bare {
			t.Errorf("%q = %+v, %v; want a bare mention", body, cmd, ok)
		}
	}
	if cmd, _ := ParseCommand("@slug help", "slug"); cmd.Verb != VerbHelp || cmd.Bare {
		t.Errorf("help = %+v; want the whole help", cmd)
	}
}

// A fix is asked for by a word everybody already uses, and whatever follows the severities is the
// asker's own words for the worker rather than a reason to read the line as a question.
func TestParseCommandFix(t *testing.T) {
	cases := []struct {
		name string
		body string
		verb Verb
		sevs []Severity
	}{
		{"plain", "@slug fix", VerbFix, nil},
		{"please fix", "@slug please fix", VerbFix, nil},
		{"fix this please", "@slug fix this please", VerbFix, nil},
		{"fix it.", "@slug fix it.", VerbFix, nil},
		{"fix all findings", "@slug fix all findings", VerbFix, nil},
		{"severities", "@slug fix p0 p1", VerbFix, []Severity{P0, P1}},
		{"severities in any case, once each", "@slug fix P1, p1 and p2", VerbFix, []Severity{P1, P2}},
		{"words after the severities are the note", "@slug fix p1 but keep the old name", VerbFix, []Severity{P1}},
		{"a sentence after fix is still a fix", "@slug fix this, keep the public API", VerbFix, nil},
		{"the filler before a severity is skipped", "@slug fix the p1 only", VerbFix, []Severity{P1}},
		{"a severity after other words is the note's", "@slug fix only the tenant check, the p2 can wait", VerbFix, nil},
		{"fixed is a claim, not a request", "@slug fixed in abc123", VerbQuestion, nil},
		{"fix in the middle is a question", "@slug can you fix this?", VerbQuestion, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, ok := ParseCommand(c.body, "slug")
			if !ok || cmd.Verb != c.verb || !slices.Equal(cmd.Severities, c.sevs) {
				t.Errorf("got %v %s %q, want %s %q", ok, cmd.Verb, cmd.Severities, c.verb, c.sevs)
			}
		})
	}
}
