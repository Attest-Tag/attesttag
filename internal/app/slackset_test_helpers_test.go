package app

import "time"

// Tests work with one workspace and a stub Slack client. testRegistry hands that client back for
// any team id, including the empty one, so a test does not have to seal a token or write a
// teams row just to exercise the message path.
func testRegistry(sl *Chat) *ChatRegistry {
	r := &ChatRegistry{byTeam: map[string]*Chat{}, cached: map[string]time.Time{}}
	if sl != nil {
		r.Put(sl.TeamID, sl)
		r.Put("", sl)
	}
	return r
}
