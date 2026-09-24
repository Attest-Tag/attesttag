package app

import (
	"context"
	"strings"
	"testing"
)

// The steps the bot posts for setting up Google Workspace are what an admin follows, word for
// word, so they have to match the console: they left out Drive — a part the connection offers
// and an API that has to be enabled for it — and sent people to a page called "Bundles" that the
// console names Access bundles.
func TestGoogleSetupStepsMatchTheConsole(t *testing.T) {
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, nil, newSettingsCache(st, Config{}))
	steps := a.setupSteps(context.Background())
	for _, want := range []string{"Calendar, Drive and People APIs", "Gmail, Calendar, Drive, Contacts", "Access bundles", "Workspaces"} {
		if !strings.Contains(steps, want) {
			t.Errorf("the setup steps do not say %q:\n%s", want, steps)
		}
	}
}
