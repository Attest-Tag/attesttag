package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// A turn that runs the same tool call again and again is not working, it is stuck, and the
// round budget was never meant to be what stops it. Routine #1 in #leads spent two
// hundred rounds on 2026-09-21 alternating the same two web searches, at $1.41, because
// nothing between the model and the tool noticed that the question had already been asked;
// and since every round re-sends the whole transcript, a loop's cost grows with the square of
// its length — the last rounds of that run were 300k tokens apiece.
//
// The guard is three steps, each cheaper than the one the model would have taken next:
//   - the second and third identical call still run, but come back with a line saying they
//     already have, in case the model simply lost its place;
//   - the fourth is refused without running, since its answer is already in the transcript;
//   - and a turn whose last few rounds asked for nothing new is landed: its tools are
//     withdrawn and it is told to write up, the way a turn out of rounds is.
//
// Identical means the same tool with the same arguments, compared as canonical JSON so key
// order and spacing do not make two calls look different. Nothing about the result is
// compared: a search engine that returns slightly different hits for the same query is still
// the same question, and that is exactly the loop this exists for.
//
// A fourth step catches the loop that the first three cannot see, because every call in it is
// new. On 2026-09-21 a turn made sixteen run_js calls that differed only in a page offset —
// p=0, p=10, p=20 — and every one of them returned "total: 1000", because nothing carries
// between sandbox calls and each script was throwing its thousand rows away. Byte-identical
// arguments never recurred, so the guard above saw a turn asking sixteen fresh questions. So a
// call whose arguments match an earlier one once the numbers are removed, AND whose result is
// byte-identical to what that one returned, is counted as no progress: it is noted, and the
// round stops counting as fresh. Both halves are required. Paging that actually returns
// different rows is real work, and two writes that differ only in a number are two writes; it
// is the pair — same shape, same answer — that means the turn is running on the spot. Even
// then it is only ever a note: a tool whose result is a constant "ok" would otherwise see
// legitimate work refused, and four such rounds landing the turn is a bound worth having
// without a refusal that could be wrong.
const (
	// How many times one call may run in a turn. The second and third come back with a note
	// rather than being refused outright: a run that re-reads a record after changing it has
	// a reason to ask twice, and three of anything is enough for a reason.
	repeatRunsAllowed = 3
	// How many rounds in a row may consist only of repeated calls before the turn is landed.
	// Four is one alternating pair asked twice over: the shape of the run that started this.
	stuckAfterRounds = 4
)

// stuckNote is what the model is told when its tools are withdrawn for repeating itself.
const stuckNote = "Your last rounds only repeated tool calls you had already made this turn, and a repeated call returns what it already returned. " +
	"Your tools have been withdrawn. Answer now, from what you already have. " + answerFromWhatYouHave

// repeatGuard counts what one turn has asked its tools, so the loop can tell a question from
// its echo. One per turn; a routine's pinned steps and the console assistant do not use it.
type repeatGuard struct {
	seen map[string]int // fingerprint → how many times this turn has asked for it
	// answers is the fourth step: the shape of a call with its numbers removed, paired with
	// what it returned, so a loop that steps an offset can be told apart from one that reads.
	answers map[string]int
	quiet   int // rounds in a row in which every call was a repeat
	total   int // calls this turn that added nothing, refused ones included, for the log line
}

func newRepeatGuard() *repeatGuard {
	return &repeatGuard{seen: map[string]int{}, answers: map[string]int{}}
}

// count records one call and returns how many times this turn has now made it, the first
// being 1.
func (g *repeatGuard) count(name, rawArgs string) int {
	fp := callFingerprint(name, rawArgs)
	g.seen[fp]++
	n := g.seen[fp]
	if n > 1 {
		g.total++
	}
	return n
}

// progressed records what a call returned and reports whether it told the turn anything it did
// not already know. False means the same shape of call has already produced this exact answer,
// which is the loop described at the top of this file. The result is hashed rather than kept:
// a turn may make two hundred calls and a tool result runs to twelve thousand characters.
func (g *repeatGuard) progressed(name, rawArgs, result string) bool {
	sum := sha256.Sum256([]byte(result))
	fp := shapeFingerprint(name, rawArgs) + "\x00" + hex.EncodeToString(sum[:])
	g.answers[fp]++
	if g.answers[fp] > 1 {
		g.total++
		return false
	}
	return true
}

// digits matches a run of digits anywhere in a call's arguments — a JSON number, and equally a
// number written inside a string, which is where a sandbox script keeps its page offset.
var digits = regexp.MustCompile(`\d+`)

// shapeFingerprint is a call with its numbers taken out, so that p=0, p=10 and p=20 are one
// shape. Crude on purpose: anything that parses the arguments would have to know which field
// of which tool is an offset, and the pairing with the result is what makes the crudeness safe.
func shapeFingerprint(name, rawArgs string) string {
	return digits.ReplaceAllString(callFingerprint(name, rawArgs), "#")
}

// endRound closes a round; fresh says whether any call in it was one the turn had not made
// before. A round with no tool calls never gets here: it is the answer, and the loop is over.
func (g *repeatGuard) endRound(fresh bool) {
	if fresh {
		g.quiet = 0
		return
	}
	g.quiet++
}

// stuck reports whether the turn should be landed: its last rounds asked for nothing new.
func (g *repeatGuard) stuck() bool { return g.quiet >= stuckAfterRounds }

// callFingerprint is a tool call as the guard compares it: the name and the arguments as
// canonical JSON, so {"b":1,"a":2} and { "a": 2, "b": 1 } are one call. Arguments that are
// not JSON are compared as the text the model wrote, trimmed.
func callFingerprint(name, rawArgs string) string {
	var v any
	if err := json.Unmarshal([]byte(rawArgs), &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			return name + " " + string(b)
		}
	}
	return name + " " + strings.TrimSpace(rawArgs)
}

// repeatNote goes above the result of a call that has run before. It is written to the model
// about the model: the model is the one that asked twice.
func repeatNote(name string, n int) string {
	return fmt.Sprintf("Note: this is the %s time this turn you have run %s with exactly these arguments, and it returns what it already returned. "+
		"Use that, change the arguments, or move on to the next step; after %d identical calls it will not run again.\n",
		ordinal(n), name, repeatRunsAllowed)
}

// sameResultNote goes above the result of a call that stepped a number and got back exactly
// what the last one did. It names the thing the model cannot see from inside the loop — that
// the calls are not accumulating — because that is the belief the loop rests on.
func sameResultNote(name string) string {
	return fmt.Sprintf("Note: this %s call differs from an earlier one this turn only in its numbers, and it returned exactly what that one returned. "+
		"Nothing carries between calls, so stepping through something a batch at a time collects nothing — each call discards what it read. "+
		"Do the whole job in one call and return the answer from inside it, or use what you already have and move on.\n", name)
}

// refuseTool is a repeated call the turn will not run again. It is still logged, as a call
// that did not succeed with no duration, so the activity page shows the refusals where the
// loop would have been rather than a run that went quiet.
func (a *Agent) refuseTool(ctx context.Context, c *Call, name, rawArgs string) string {
	out := fmt.Sprintf("error: refused — you have already run %s with exactly these arguments %d times this turn, and its result is in this conversation. "+
		"It will not run again. Use what it returned, change the arguments, or move on to the next step.", name, repeatRunsAllowed)
	logArgs := truncate(rawArgs, 2000)
	// The same rule as runToolRaw, a request on somebody's own connection included: the refusal
	// repeats the arguments of a call that did run, a Gmail search's query among them.
	if conn, req := a.requestConn(c, name, json.RawMessage(rawArgs)); privateCall(name, conn) {
		logArgs = privateArgs(ctx, c, conn, req)
	} else if name == "run_js" {
		// The runs this refusal repeats left no note behind, so the channel's reach decides: where
		// a fetch() could have gone through somebody's own account, the script is theirs to keep.
		if conns := a.personalReach(c); len(conns) > 0 {
			logArgs = privateScriptArgs(ctx, c, conns)
		}
	}
	a.store.LogToolCall(ctx, c.OrgID, c.TeamID, c.Channel, c.ThreadTS, name, logArgs, out, false, 0)
	return fmt.Sprintf("<tool_result name=%q>\n%s\n</tool_result>", name, out)
}

// ordinal is 2 → "2nd", for the note above.
func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}
