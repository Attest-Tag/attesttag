package app

import (
	"sort"
	"strings"
	"testing"
)

func scopeSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.Fields(s) {
		out[f] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Preset.Scopes is what a connection made before the options existed asks for, so it has to stay
// the sum of the options. Add a scope to one and forget the other and half the admins get a
// service the console says they connected and Google refuses to serve.
func TestPresetScopesMatchOptions(t *testing.T) {
	for _, pr := range Presets {
		if len(pr.Options) == 0 {
			continue
		}
		all := scopeSet(baseScopes)
		everything := make([]ConnectionOption, 0, len(pr.Options))
		for _, o := range pr.Options {
			everything = append(everything, ConnectionOption{ID: o.ID, Write: true})
			for _, s := range strings.Fields(o.ReadScopes + " " + o.WriteScopes) {
				all[s] = true
			}
		}
		if got, want := sortedKeys(scopeSet(pr.Scopes)), sortedKeys(all); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: Scopes and Options disagree\n Scopes: %v\nOptions: %v", pr.ID, got, want)
		}
		// And ticking every box has to reproduce it exactly, since that is the same connection
		// arrived at by the other road.
		_, _, everyScope := resolveOptions(&pr, everything)
		if got, want := sortedKeys(scopeSet(everyScope)), sortedKeys(all); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: every option ticked gives %v, want %v", pr.ID, got, want)
		}
	}
}

func TestResolveOptionsNarrowsToWhatWasTicked(t *testing.T) {
	pr := presetByID("google")
	if pr == nil || len(pr.Options) == 0 {
		t.Fatal("the google preset has lost its options")
	}

	// Calendar alone, read only: one host, one prefix, and no scope that could book anything.
	hosts, prefixes, scopes := resolveOptions(pr, []ConnectionOption{{ID: "calendar"}})
	if len(hosts) != 1 || hosts[0] != "www.googleapis.com" {
		t.Errorf("hosts = %v, want just www.googleapis.com", hosts)
	}
	if len(prefixes) != 1 || prefixes[0] != "/calendar/v3" {
		t.Errorf("prefixes = %v, want just /calendar/v3", prefixes)
	}
	if !strings.Contains(scopes, "calendar.readonly") {
		t.Errorf("scopes %q lost the calendar read scope", scopes)
	}
	if strings.Contains(scopes, "calendar.events") {
		t.Errorf("scopes %q can book events on a read-only calendar", scopes)
	}
	if strings.Contains(scopes, "gmail") || strings.Contains(scopes, "contacts") {
		t.Errorf("scopes %q reach services nobody ticked", scopes)
	}
	if !strings.HasPrefix(scopes, baseScopes) {
		t.Errorf("scopes %q dropped the identifying scopes", scopes)
	}

	// Calendar that may book, plus contacts to find who to invite.
	hosts, _, scopes = resolveOptions(pr, []ConnectionOption{{ID: "calendar", Write: true}, {ID: "contacts"}})
	if len(hosts) != 2 || hosts[0] != "www.googleapis.com" || hosts[1] != "people.googleapis.com" {
		t.Errorf("hosts = %v, want calendar and people", hosts)
	}
	for _, want := range []string{"calendar.events", "contacts.readonly", "directory.readonly"} {
		if !strings.Contains(scopes, want) {
			t.Errorf("scopes %q missing %s", scopes, want)
		}
	}
	if strings.Contains(scopes, "gmail") {
		t.Errorf("scopes %q reach mail nobody ticked", scopes)
	}

	// Drive alone, read only. The full-Drive scope is a prefix of the read-only one, so whole
	// scopes have to be compared: asking whether the string contains "drive" always says yes.
	hosts, prefixes, scopes = resolveOptions(pr, []ConnectionOption{{ID: "drive"}})
	if len(hosts) != 1 || hosts[0] != "www.googleapis.com" {
		t.Errorf("hosts = %v, want just www.googleapis.com", hosts)
	}
	if len(prefixes) != 2 || prefixes[0] != "/drive/v3" || prefixes[1] != "/upload/drive/v3" {
		t.Errorf("prefixes = %v, want the two Drive prefixes", prefixes)
	}
	if got := scopeSet(scopes); !got["https://www.googleapis.com/auth/drive.readonly"] {
		t.Errorf("scopes %q lost the Drive read scope", scopes)
	} else if got["https://www.googleapis.com/auth/drive"] {
		t.Errorf("scopes %q hand over all of Drive for a read-only tick", scopes)
	}

	// Contacts is only ever read: asking for writing on it adds nothing.
	_, _, readOnly := resolveOptions(pr, []ConnectionOption{{ID: "contacts"}})
	_, _, asked := resolveOptions(pr, []ConnectionOption{{ID: "contacts", Write: true}})
	if readOnly != asked {
		t.Errorf("writing on an option with no write scopes changed them: %q vs %q", readOnly, asked)
	}

	// Nothing ticked is not "everything": it is a connection that reaches nothing, which
	// buildConnection turns into an error rather than a silent full-access install.
	if h, _, s := resolveOptions(pr, nil); h != nil || s != "" {
		t.Errorf("no options gave hosts %v scopes %q, want nothing", h, s)
	}
	// An option the console does not have is ignored rather than widening anything. The id is
	// deliberately not a service Google runs, so that adding a real part later cannot quietly
	// turn this into a test of something else — which is exactly what "drive" did here once.
	if h, _, _ := resolveOptions(pr, []ConnectionOption{{ID: "nonesuch", Write: true}}); h != nil {
		t.Errorf("an unknown option reached %v", h)
	}
	// A preset with no options at all is untouched by any of this.
	if h, _, s := resolveOptions(presetByID("clickup"), []ConnectionOption{{ID: "calendar"}}); h != nil || s != "" {
		t.Errorf("options leaked into a preset that has none: %v %q", h, s)
	}
}

// `!connect` and the Connect card tell a person what a connection is for by reading the parts
// back off what it may reach. A part nobody connected must not be advertised: somebody who was
// never given mail should not be told they can ask about mail.
func TestConnectExamplesFollowTheConnectedParts(t *testing.T) {
	pr := presetByID("google")
	hosts, prefixes, _ := resolveOptions(pr, []ConnectionOption{{ID: "calendar", Write: true}})
	calendarOnly := &Connection{Preset: "google", AllowedHosts: hosts, PathPrefixes: prefixes}

	opts := connectionOptions(pr, calendarOnly)
	if len(opts) != 1 || opts[0].ID != "calendar" {
		t.Fatalf("connectionOptions = %v, want calendar alone", opts)
	}
	ex := strings.Join(connectExamples(calendarOnly), " | ")
	if !strings.Contains(ex, "calendar") {
		t.Errorf("examples %q say nothing about the calendar", ex)
	}
	for _, absent := range []string{"reply", "email address"} {
		if strings.Contains(ex, absent) {
			t.Errorf("examples %q advertise a part nobody connected", ex)
		}
	}

	// All three parts: every example, and the card's tail trimmed to three of them.
	hosts, prefixes, _ = resolveOptions(pr, []ConnectionOption{{ID: "calendar"}, {ID: "gmail"}, {ID: "contacts"}})
	full := &Connection{Preset: "google", AllowedHosts: hosts, PathPrefixes: prefixes}
	if got := len(connectionOptions(pr, full)); got != 3 {
		t.Errorf("connectionOptions = %d parts, want 3", got)
	}
	if got := len(connectExamples(full)); got < 4 {
		t.Errorf("only %d examples for the whole of Google", got)
	}
	if got := strings.Count(examplesBlock(full), "\n• "); got != 3 {
		t.Errorf("the card lists %d examples, want it capped at 3", got)
	}

	// A preset with no parts says nothing rather than making something up.
	if got := examplesBlock(&Connection{Preset: "clickup", AllowedHosts: []string{"api.clickup.com"}}); got != "" {
		t.Errorf("examplesBlock invented %q for a preset with no options", got)
	}
}

// The console sends the ticked boxes; the connection that comes out has to be allowlisted to
// them, whatever the preset's own defaults say.
func TestBuildConnectionHonoursOptions(t *testing.T) {
	b := repoTestBot(t, nil)
	c, sec, err := b.buildConnection(&connectionInput{
		Name: "Google Workspace", Preset: "google", CredType: "oauth_user",
		Options: []ConnectionOption{{ID: "calendar", Write: true}, {ID: "contacts"}},
		Secret:  &Secret{ClientID: "id.apps.googleusercontent.com", ClientSecret: "shh"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedHosts) != 2 {
		t.Errorf("hosts = %v, want calendar and people only", c.AllowedHosts)
	}
	for _, h := range c.AllowedHosts {
		if h == "gmail.googleapis.com" {
			t.Errorf("hosts = %v: mail was not ticked", c.AllowedHosts)
		}
	}
	for _, p := range c.PathPrefixes {
		if strings.HasPrefix(p, "/gmail") {
			t.Errorf("prefixes = %v: mail was not ticked", c.PathPrefixes)
		}
	}
	if sec == nil || sec.OAuth == nil {
		t.Fatal("no OAuth client was stored")
	}
	if strings.Contains(sec.OAuth.Scopes, "gmail") {
		t.Errorf("people are asked to consent to mail nobody ticked: %q", sec.OAuth.Scopes)
	}
	if !strings.Contains(sec.OAuth.Scopes, "calendar.events") {
		t.Errorf("a calendar allowed to book cannot: %q", sec.OAuth.Scopes)
	}

	// Ticking nothing is refused rather than quietly connecting the whole of Google — both when
	// the boxes were all cleared and when only unknown ones came back.
	for _, opts := range [][]ConnectionOption{{}, {{ID: "nope"}}} {
		if _, _, err := b.buildConnection(&connectionInput{
			Name: "Google Workspace", Preset: "google", CredType: "oauth_user",
			Options: opts,
			Secret:  &Secret{ClientID: "id.apps.googleusercontent.com", ClientSecret: "shh"},
		}, nil); err == nil {
			t.Errorf("a connection to nothing was accepted for options %v", opts)
		}
	}

	// An API client that sends no options at all still gets the whole preset, as before.
	full, fullSec, err := b.buildConnection(&connectionInput{
		Name: "Google Workspace", Preset: "google", CredType: "oauth_user",
		Secret: &Secret{ClientID: "id.apps.googleusercontent.com", ClientSecret: "shh"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.AllowedHosts) != 3 {
		t.Errorf("hosts = %v, want all three", full.AllowedHosts)
	}
	if fullSec.OAuth.Scopes != presetByID("google").Scopes {
		t.Errorf("scopes = %q, want the preset's own", fullSec.OAuth.Scopes)
	}
}

// The Drive part is the one an admin has to ask for. Every other part of Google Workspace is
// scoped to a service; a Drive grant reaches every file the person can open, so arriving at it
// by accepting a default is the thing this guards against.
func TestDriveIsNotTickedByDefault(t *testing.T) {
	for _, o := range presetByID("google").Options {
		if o.ID != "drive" {
			continue
		}
		if o.Default || o.DefaultWrite {
			t.Errorf("Drive is ticked by default (read=%v write=%v)", o.Default, o.DefaultWrite)
		}
		if o.WriteScopes == "" || o.WriteLabel == "" {
			t.Error("Drive has no write box for an admin to opt into")
		}
		return
	}
	t.Fatal("the google preset has no drive part")
}

// Drive and Calendar are the same host, so the path prefixes are the only thing that can say
// which parts a connection actually carries. Read back a Drive-only connection and Calendar
// must not appear — otherwise `!connect` offers somebody a calendar they were never granted.
func TestDriveAndCalendarAreToldApartByPath(t *testing.T) {
	pr := presetByID("google")
	hosts, prefixes, _ := resolveOptions(pr, []ConnectionOption{{ID: "drive"}})
	on := map[string]bool{}
	for _, o := range connectionOptions(pr, &Connection{Preset: "google", AllowedHosts: hosts, PathPrefixes: prefixes}) {
		on[o.ID] = true
	}
	if !on["drive"] || on["calendar"] {
		t.Errorf("parts read back as %v, want drive alone", on)
	}
}

// An existing connection has to read back as what the admin configured, writing included. The
// console's edit form is the caller: when it fell back to the preset's defaults instead, adding
// one part cleared the write box on another, and saving dropped a grant everyone had consented
// to. Round-trip every combination that differs from the preset's own defaults.
func TestConnectionOptionStateRoundTrips(t *testing.T) {
	pr := presetByID("google")
	for _, want := range [][]ConnectionOption{
		{{ID: "gmail", Write: true}, {ID: "calendar", Write: false}},
		{{ID: "gmail", Write: false}, {ID: "calendar", Write: true}},
		{{ID: "drive", Write: true}},
		{{ID: "drive", Write: false}, {ID: "contacts", Write: false}},
	} {
		hosts, prefixes, scopes := resolveOptions(pr, want)
		conn := &Connection{Preset: "google", AllowedHosts: hosts, PathPrefixes: prefixes}
		got := connectionOptionState(pr, conn, scopes)
		if len(got) != len(want) {
			t.Errorf("%v read back as %v", want, got)
			continue
		}
		byID := map[string]bool{}
		for _, g := range got {
			byID[g.ID] = g.Write
		}
		for _, w := range want {
			if write, on := byID[w.ID]; !on || write != w.Write {
				t.Errorf("%v: part %s read back on=%v write=%v, want on write=%v", want, w.ID, on, write, w.Write)
			}
		}
	}
}

// Contacts has no write scopes at all, so it can never report writing however the scopes look —
// otherwise the form would offer a box that grants nothing and save it as though it had.
func TestReadOnlyPartNeverReportsWriting(t *testing.T) {
	pr := presetByID("google")
	hosts, prefixes, _ := resolveOptions(pr, []ConnectionOption{{ID: "contacts", Write: true}})
	conn := &Connection{Preset: "google", AllowedHosts: hosts, PathPrefixes: prefixes}
	for _, o := range connectionOptionState(pr, conn, pr.Scopes) {
		if o.ID == "contacts" && o.Write {
			t.Error("contacts reported writing, but it has no write scopes")
		}
	}
}

// Adding a part to a connection that already works is done without retyping the credential: the
// console leaves the secret fields empty and sends the boxes. The hosts and prefixes are plain
// columns and update on their own, but the scopes people consent to live inside the sealed
// secret — so unless it is re-sealed the two halves of one decision disagree, and the connection
// routes Drive while nobody is ever asked for a Drive scope.
func TestEditingPartsWithoutRetypingTheSecret(t *testing.T) {
	b := repoTestBot(t, nil)
	cur, sec, err := b.buildConnection(&connectionInput{
		Name: "Google Workspace", Preset: "google", CredType: "oauth_user",
		Options: []ConnectionOption{{ID: "calendar", Write: true}},
		Secret:  &Secret{ClientID: "id.apps.googleusercontent.com", ClientSecret: "shh"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cur.secretEnc, err = b.sealSecret(sec); err != nil {
		t.Fatal(err)
	}

	// Tick Drive, type nothing else — what the console sends with the secret fields empty.
	c2, sec2, err := b.buildConnection(&connectionInput{
		Name: "Google Workspace", Preset: "google", CredType: "oauth_user",
		Options: []ConnectionOption{{ID: "calendar", Write: true}, {ID: "drive"}},
	}, cur)
	if err != nil {
		t.Fatal(err)
	}
	if sec2 == nil || sec2.OAuth == nil {
		t.Fatal("the secret was left alone, so the Drive scope is never stored and nobody is asked for it")
	}
	if !strings.Contains(sec2.OAuth.Scopes, "drive.readonly") {
		t.Errorf("scopes %q did not follow the boxes", sec2.OAuth.Scopes)
	}
	// A re-seal, not a rotation: the credential nobody retyped is the credential that is kept.
	if sec2.OAuth.ClientID != "id.apps.googleusercontent.com" || sec2.OAuth.ClientSecret != "shh" {
		t.Errorf("the OAuth client was lost: %q / %q", sec2.OAuth.ClientID, sec2.OAuth.ClientSecret)
	}
	// And the whole decision agrees with itself, which is the point.
	c2.secretEnc = cur.secretEnc
	on := map[string]bool{}
	for _, o := range connectionOptionState(presetByID("google"), c2, sec2.OAuth.Scopes) {
		on[o.ID] = true
	}
	if !on["drive"] || !on["calendar"] {
		t.Errorf("parts read back as %v, want calendar and drive", on)
	}
}

// The same edit must not quietly rotate a credential that was never retyped for the other
// credential types either: a connection whose boxes cannot change has nothing to re-seal.
func TestEditingWithoutSecretLeavesOtherCredentialsAlone(t *testing.T) {
	b := repoTestBot(t, nil)
	cur, sec, err := b.buildConnection(&connectionInput{
		Name: "example", Preset: "github", CredType: "bearer", Secret: &Secret{Token: "tok"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cur.secretEnc, err = b.sealSecret(sec); err != nil {
		t.Fatal(err)
	}
	if _, sec2, err := b.buildConnection(&connectionInput{
		Name: "example renamed", Preset: "github", CredType: "bearer"}, cur); err != nil {
		t.Fatal(err)
	} else if sec2 != nil {
		t.Error("a rename re-sealed a credential nobody touched")
	}
}
