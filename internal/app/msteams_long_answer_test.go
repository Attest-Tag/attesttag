package app

import (
	"strings"
	"testing"
	"time"
)

// Teams takes no file from a bot, so an answer long enough to be filed in Slack came to Teams as a
// lead saying "Full answer attached as a file" — and then, the upload having failed, the whole
// answer again in a second message under it. It is now posted whole, and nothing promises a file.
func TestALongTeamsAnswerIsNotPromisedAsAFile(t *testing.T) {
	answer := strings.Repeat("Refunds wait on the bank's settlement window, which is why they take days. ", 260) +
		"And that is the whole of it."
	if len(answer) <= 16000 {
		t.Fatalf("the answer is %d characters, not long enough to be filed", len(answer))
	}
	_, mux, f, st := teamsTestBot(t, answer)
	linkTeams(t, st, mux, f)
	before := len(f.messages())
	deliverTeams(t, mux, f.sign(nil), f.activity("why do refunds take so long?", nil))

	deadline := time.Now().Add(10 * time.Second)
	var said []string
	for {
		said = said[:0]
		for _, p := range f.messages()[before:] {
			said = append(said, p.Activity.Text)
		}
		if strings.Contains(strings.Join(said, "\n"), "And that is the whole of it.") || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	all := strings.Join(said, "\n")
	if !strings.Contains(all, "And that is the whole of it.") {
		t.Fatalf("the answer never arrived whole; Teams was sent:\n%s", truncate(all, 600))
	}
	if strings.Contains(all, "attached as a file") {
		t.Errorf("Teams was promised a file it cannot be sent:\n%s", truncate(all, 600))
	}
}
