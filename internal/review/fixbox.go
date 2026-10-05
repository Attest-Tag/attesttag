package review

import (
	"regexp"
	"strings"
)

// FixBoxText is the task-list item a finding's inline comment carries when a fix can be asked for
// from it (RenderContext.FixBox). Ticking it is the request. It needs no command to remember and
// no reaction, which GitHub never tells an App about: a tick is an edit of the comment, and an edit
// is delivered. Only somebody who may edit the comment can tick it — the App that wrote it, or
// anybody with write access to the repository — which is the same people a fix is for.
const FixBoxText = "Fix this on the pull request — attest_tag pushes a commit to its branch"

// fixBoxLine is the item in any state: a dash or star bullet, a box that is empty or ticked (GitHub
// writes "x"; a hand edit may write "X"), and the text exactly.
var fixBoxLine = regexp.MustCompile(`^[-*] \[([ xX])\] ` + regexp.QuoteMeta(FixBoxText) + `$`)

// fixBoxState is the one fix item in body: "" when there is none, or more than one — a comment
// somebody edited into holding two is not one whose tick says which was meant — else " " or "x".
func fixBoxState(body string) string {
	state := ""
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		m := fixBoxLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if state != "" {
			return ""
		}
		state = strings.ToLower(m[1])
	}
	return state
}

// FixBoxTicked reports whether an edit of a finding's comment, from before to after, ticked its fix
// box: the item was there unticked and is now ticked. Unticking, ticking a box already ticked, or
// an edit that leaves the box as it was asks for nothing — GitHub delivers every edit, the App's
// own included. Nothing else in either body is read: whoever may tick the box may also rewrite the
// comment around it, and what the fix is for is the finding as stored, never the comment's text.
func FixBoxTicked(before, after string) bool {
	return fixBoxState(before) == " " && fixBoxState(after) == "x"
}
