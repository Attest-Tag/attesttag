package app

import (
	"context"
	"strings"
	"testing"
)

// chatCall is a person typing in a channel whose access holds repos, the way a turn resolves it.
func chatCall(repos ...string) *Call {
	acc := &Access{}
	for _, r := range repos {
		acc.Rules = append(acc.Rules, Rule{Conn: &Connection{Name: strings.ReplaceAll(r, "/", "-"), Repo: r}})
	}
	return &Call{TeamID: "T1", OrgID: orgID, Channel: "C1", ThreadTS: "1700000000.000100", UserID: "U1",
		HumanTurn: true, Access: acc}
}

// A review asked for in a channel is the channel's to ask for only when its Access bundles grant the
// repository: one it is not granted is refused by name, before anything is read or queued, and so is
// a channel with no repositories at all.
func TestChatReviewNeedsTheRepositoryInTheChannel(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	ctx := context.Background()

	for _, c := range []*Call{chatCall("acme/other"), chatCall(), {OrgID: orgID, HumanTurn: true}} {
		out, err := rig.b.reviewFromChat(ctx, c, "acme/web", 7, nil, false)
		if err == nil {
			t.Fatalf("a channel without acme/web started a review of it: %q", out)
		}
		if strings.Contains(err.Error(), "Automation") {
			t.Errorf("the refusal told a channel without the repository how it is reviewed: %v", err)
		}
	}
	if runs := rig.runs(7); len(runs) != 0 {
		t.Fatalf("a refused request queued %d runs", len(runs))
	}
	if sent := rig.fake.sent(); len(sent) != 0 {
		t.Errorf("a refused request reached GitHub: %v", sent)
	}
}

// A channel that holds the repository queues the review as a person asking: past the "when" setting,
// under the repository's own mode, recorded as asked for in chat by that person, and said back with
// where it will go. Asking again in the same thread for the same head is that run, not a second one.
func TestChatReviewQueuesForAChannelThatHoldsTheRepository(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live","trigger":"command"}`)
	ctx := context.Background()
	c := chatCall("acme/web")

	out, err := rig.b.reviewFromChat(ctx, c, "acme/web", 7, nil, false)
	if err != nil {
		t.Fatalf("a channel holding acme/web was refused: %v", err)
	}
	if !strings.Contains(out, "Queued a review of acme/web#7") || !strings.Contains(out, "posted on the pull request") {
		t.Errorf("the answer = %q", out)
	}
	runs := rig.runs(7)
	if len(runs) != 1 {
		t.Fatalf("queued %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.Trigger != "chat" || r.RequestedBy != "chat:T1/U1" || r.InstallationID != fakeInstallation {
		t.Errorf("the run = trigger %q, requested by %q, installation %d", r.Trigger, r.RequestedBy, r.InstallationID)
	}

	if _, err := rig.b.reviewFromChat(ctx, c, "acme/web", 7, nil, false); err != nil {
		t.Fatalf("asking again: %v", err)
	}
	if runs := rig.runs(7); len(runs) != 1 {
		t.Errorf("asking again in the same thread queued %d runs, want the one already queued", len(runs))
	}

	// The repository may be left out where the channel has only the one.
	if _, err := rig.b.reviewFromChat(ctx, chatCall("acme/web"), "", 7, []string{"Security"}, true); err != nil {
		t.Errorf("a security review in shadow, the repository left out: %v", err)
	}
}

// Somebody in a channel cannot ask for a type that is not there, nor reach a repository the channel is
// granted but nobody added to code review; and only a person typing starts one — not a forwarded mail's
// turn, a routine, or the console's Playground.
func TestChatReviewRefusals(t *testing.T) {
	rig := newReviewAPIRig(t, `{"mode":"live"}`)
	ctx := context.Background()

	if _, err := rig.b.reviewFromChat(ctx, chatCall("acme/web"), "acme/web", 7, []string{"nonsense"}, false); err == nil ||
		!strings.Contains(err.Error(), "no review type nonsense") {
		t.Errorf("an unknown type: %v", err)
	}
	if _, err := rig.b.reviewFromChat(ctx, chatCall("acme/api"), "acme/api", 7, nil, false); err == nil ||
		!strings.Contains(err.Error(), "is not reviewed") {
		t.Errorf("a granted repository nobody added to code review: %v", err)
	}
	mail := chatCall("acme/web")
	mail.HumanTurn = false
	if _, err := rig.b.reviewFromChat(ctx, mail, "acme/web", 7, nil, false); err == nil {
		t.Error("a turn nobody typed started a review")
	}
	preview := chatCall("acme/web")
	preview.Preview = true
	if _, err := rig.b.reviewFromChat(ctx, preview, "acme/web", 7, nil, false); err == nil {
		t.Error("the Playground started a real review")
	}
	if _, err := rig.b.reviewFromChat(ctx, chatCall("acme/web"), "acme/web", 0, nil, false); err == nil {
		t.Error("no pull request number was accepted")
	}
	if runs := rig.runs(7); len(runs) != 0 {
		t.Errorf("refusals queued %d runs", len(runs))
	}
}

// The tool is offered with the GitHub pack only where the deployment has code review.
func TestChatReviewToolFollowsTheDeployment(t *testing.T) {
	has := func(a *Agent) bool {
		for _, tl := range a.packs("github", "api.github.com") {
			if tl.Name == "github_start_review" {
				return true
			}
		}
		return false
	}
	a := &Agent{}
	if has(a) {
		t.Error("offered with code review off")
	}
	a.startReview = func(context.Context, *Call, string, int, []string, bool) (string, error) { return "", nil }
	if !has(a) {
		t.Error("not offered with code review on")
	}
}
