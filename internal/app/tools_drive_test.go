package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// driveFixture is a channel with both kinds of Drive: the company's, a credential made the way the
// console makes a Drive service account — no path prefixes, so it claims the whole host — and the
// asker's own Google Workspace with Drive ticked. Sam has connected theirs; nobody else has. Each
// Drive answers with the files its own token can see, and one file is in both.
type driveFixture struct {
	a       *Agent
	st      *Store
	rec     *recordingSlack
	sl      *Chat
	company *Connection
	google  *Connection
	mu      sync.Mutex
	seen    []string // "<token> <path>?<query>", one per request that reached Drive
}

const (
	fileCompanyCSV = "fileCompanyCSV1"
	fileBoth       = "fileBothPricing"
	fileSam        = "fileSamSalary1"
)

func newDriveFixture(t *testing.T) *driveFixture {
	t.Helper()
	st, p, google := userConnFixture(t)
	ctx := context.Background()
	google.PathPrefixes = []string{"/calendar/v3", "/drive/v3", "/upload/drive/v3"}
	enc, err := p.sealer.Seal(mustJSON(t, &Secret{Token: "company-token"}))
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.InsertConnection(ctx, orgID, &Connection{BundleID: google.BundleID, Name: "Company Drive", Preset: "gdrive",
		CredType: "bearer", AllowedHosts: []string{"www.googleapis.com"}, Writes: "confirm", Status: "active"}, enc)
	if err != nil {
		t.Fatal(err)
	}
	f := &driveFixture{st: st, company: mustConn(t, st, id), google: google}
	grant(t, st, p, google, "T1", "USAM", "sam-token", time.Now().Add(time.Hour))

	meta := map[string]string{
		fileCompanyCSV: `{"id":"fileCompanyCSV1","name":"Paying customers.csv","mimeType":"text/csv","modifiedTime":"2026-09-18T02:09:34Z","webViewLink":"https://drive.google.com/file/d/fileCompanyCSV1/view"}`,
		fileBoth:       `{"id":"fileBothPricing","name":"Pricing 2026","mimeType":"application/vnd.google-apps.spreadsheet","modifiedTime":"2026-09-01T10:00:00Z","webViewLink":"https://docs.google.com/spreadsheets/d/fileBothPricing/edit"}`,
		fileSam:        `{"id":"fileSamSalary1","name":"Salary review","mimeType":"application/vnd.google-apps.document","modifiedTime":"2026-08-30T10:00:00Z","webViewLink":"https://docs.google.com/document/d/fileSamSalary1/edit"}`,
	}
	sees := map[string][]string{"company-token": {fileCompanyCSV, fileBoth}, "sam-token": {fileSam, fileBoth}}
	content := map[string]string{
		fileCompanyCSV: "org,org_id\nAcme,4242\nGlobex,777\n",
		fileBoth:       "tier,price\nstarter,10\n",
		fileSam:        "# Salary review\n\nfor Sam only",
	}
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.seen = append(f.seen, tok+" "+r.URL.Path+"?"+r.URL.RawQuery)
		f.mu.Unlock()
		reply := func(status int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		}
		visible := func(id string) bool {
			for _, v := range sees[tok] {
				if v == id {
					return true
				}
			}
			return false
		}
		path := strings.TrimPrefix(r.URL.Path, "/drive/v3/files")
		switch {
		case path == "" || path == "/":
			var files []string
			for _, id := range sees[tok] {
				files = append(files, meta[id])
			}
			return reply(200, `{"files":[`+strings.Join(files, ",")+`]}`)
		default:
			id, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
			if !visible(id) {
				return reply(404, `{"error":{"code":404,"message":"File not found: `+id+`."}}`)
			}
			if rest == "export" || r.URL.Query().Get("alt") == "media" {
				return reply(200, content[id])
			}
			return reply(200, meta[id])
		}
	})

	f.rec = &recordingSlack{}
	srv := httptest.NewServer(f.rec)
	t.Cleanup(srv.Close)
	f.sl = &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1"}
	// A public address, because a Connect link that cannot be built is not sent at all.
	f.a = &Agent{store: st, proxy: p, settings: newSettingsCache(st, Config{}), loc: time.UTC,
		slacks: testRegistry(f.sl), cfg: Config{HealthAddr: "127.0.0.1:8080"}}
	return f
}

// call is a turn by user in channel. The service account is granted closer in than the person's
// own connection, the arrangement shared-host routing was written against.
func (f *driveFixture) call(user, channel string) *Call {
	return &Call{OrgID: orgID, TeamID: "T1", Channel: channel, ThreadTS: "1700000000.000100", UserID: user, Kind: "channel",
		SL: f.sl, Access: &Access{Rules: []Rule{{Conn: f.company, Rank: 2}, {Conn: f.google, Rank: 1}}}}
}

func (f *driveFixture) run(t *testing.T, c *Call, tool, args string) (string, *spendNote) {
	t.Helper()
	ctx, note := withSpendNote(context.Background())
	tl, ok := f.a.toolsFor(ctx, c)[tool]
	if !ok {
		t.Fatalf("%s is not offered in a channel that reaches Drive", tool)
	}
	out, err := tl.Run(ctx, c, json.RawMessage(args))
	if err != nil {
		return "error: " + err.Error(), note
	}
	return out, note
}

func (f *driveFixture) askedWith(token string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.seen {
		if strings.HasPrefix(s, token+" ") {
			out = append(out, s)
		}
	}
	return out
}

func (f *driveFixture) dmsTo(user string) []string {
	var out []string
	for _, post := range f.rec.posts() {
		if strings.HasPrefix(post, "chat.postMessage "+user+":") {
			out = append(out, post)
		}
	}
	return out
}

// The incident: a file in the company's folder, asked for in a channel by somebody who has
// connected their own Google account. Both Drives are asked, the company's file is listed as the
// company's, and the one file both can open is listed once — as the company's, since it is not
// private. The asker's own file is not put in front of the channel: it goes to them alone.
func TestDriveSearchAsksEveryDriveAndSaysWhoseEachFileIs(t *testing.T) {
	f := newDriveFixture(t)
	out, note := f.run(t, f.call("USAM", "C1"), "drive_search", `{"query":"customers pricing"}`)

	for _, want := range []string{"Paying customers.csv", "company Drive (Company Drive)", "Pricing 2026", "which they can open too",
		"drive_read file_id=fileCompanyCSV1 from=company", "went to them in a direct message"} {
		if !strings.Contains(out, want) {
			t.Errorf("the result never says %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "Pricing 2026"); n != 1 {
		t.Errorf("a file both Drives can open is listed %d times:\n%s", n, out)
	}
	if strings.Contains(out, "Salary review") {
		t.Errorf("a file from the asker's own Drive reached a channel that did not ask for their files:\n%s", out)
	}
	dms := f.dmsTo("USAM")
	if len(dms) != 1 || !strings.Contains(dms[0], "Salary review") || strings.Contains(dms[0], "Paying customers") {
		t.Errorf("their own file did not go to them, alone: %v", dms)
	}
	if len(f.askedWith("company-token")) == 0 || len(f.askedWith("sam-token")) == 0 {
		t.Errorf("both Drives were not asked: %v", f.seen)
	}
	for _, s := range f.seen {
		if !strings.Contains(s, "trashed+%3D+false") || !strings.Contains(s, "corpora=allDrives") {
			t.Errorf("a search left out the bin filter or the shared drives: %s", s)
		}
	}
	// It spent Sam's own account, so Activity keeps whose it was and no more.
	if !note.spentPersonal() {
		t.Error("a search through the asker's own Drive was not marked as spending it")
	}
}

// In a direct message there is nobody else to hide the asker's files from.
func TestDriveSearchInADirectMessageListsTheirOwnFiles(t *testing.T) {
	f := newDriveFixture(t)
	out, _ := f.run(t, f.call("USAM", "D1"), "drive_search", `{"query":"review"}`)
	for _, want := range []string{"Salary review", "<@USAM>'s own Drive (USAM@example.com)", "from=mine", "Paying customers.csv"} {
		if !strings.Contains(out, want) {
			t.Errorf("the result never says %q:\n%s", want, out)
		}
	}
	if dms := f.dmsTo("USAM"); len(dms) != 0 {
		t.Errorf("a DM was sent in a conversation that already is one: %v", dms)
	}
}

// Asked for — "in my Drive" — their own files are what the answer is about, so they are listed
// where they asked, and the company's Drive is not asked at all.
func TestDriveSearchForTheirOwnFilesListsThemWhereTheyAsked(t *testing.T) {
	f := newDriveFixture(t)
	out, _ := f.run(t, f.call("USAM", "C1"), "drive_search", `{"query":"review","from":"mine"}`)
	if !strings.Contains(out, "Salary review") || !strings.Contains(out, "Pricing 2026") {
		t.Errorf("their own files are missing:\n%s", out)
	}
	if strings.Contains(out, "Paying customers.csv") || len(f.askedWith("company-token")) != 0 {
		t.Errorf("the company's Drive was searched for a question about their own:\n%s", out)
	}
	if dms := f.dmsTo("USAM"); len(dms) != 0 {
		t.Errorf("files they asked for in the channel were sent to them privately as well: %v", dms)
	}
}

// Somebody who has never connected their own account is answered from the company's Drive, with no
// Connect card: nothing of theirs is needed to read a company file. Told why their own was not
// searched, the model can still send the link if that is what they meant.
func TestDriveSearchWithoutTheirOwnAccountAnswersFromTheCompanyDrive(t *testing.T) {
	f := newDriveFixture(t)
	out, note := f.run(t, f.call("UNEW", "C1"), "drive_search", `{"query":"customers"}`)
	if !strings.Contains(out, "Paying customers.csv") {
		t.Errorf("the company's file is missing:\n%s", out)
	}
	if !strings.Contains(out, "has not connected") || !strings.Contains(out, "connect_account") {
		t.Errorf("the result does not say their own Drive was left out, or how to add it:\n%s", out)
	}
	if posts := f.rec.posts(); len(posts) != 0 {
		t.Errorf("somebody was sent something for a search that needed nothing of theirs: %v", posts)
	}
	if len(f.askedWith("sam-token")) != 0 || note.spentPersonal() {
		t.Error("an unconnected asker's search spent somebody's own account")
	}
}

// Their own files, asked for by somebody who has not connected: the only answer is the link.
func TestDriveSearchForTheirOwnFilesAsksThemToConnect(t *testing.T) {
	f := newDriveFixture(t)
	out, _ := f.run(t, f.call("UNEW", "C1"), "drive_search", `{"query":"review","from":"mine"}`)
	if !strings.Contains(out, "Connect button") {
		t.Errorf("the result does not say a link went to them:\n%s", out)
	}
	dms := f.dmsTo("UNEW")
	if len(dms) != 1 || !strings.Contains(dms[0], "/connect/") {
		t.Errorf("no Connect card reached them: %v", f.rec.posts())
	}
	if strings.Contains(out, "/connect/") {
		t.Errorf("the link itself reached the tool result:\n%s", out)
	}
}

func TestDriveQuery(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"the pricing doc", "trashed = false and (name contains 'pricing' or fullText contains 'pricing')"},
		{"o'brien notes", `trashed = false and (name contains 'o\'brien' or fullText contains 'o\'brien') and (name contains 'notes' or fullText contains 'notes')`},
		{`back\slash`, `trashed = false and (name contains 'back\\slash' or fullText contains 'back\\slash')`},
		// Nothing but words about a file is still something to look for.
		{"the doc", "trashed = false and (name contains 'the' or fullText contains 'the') and (name contains 'doc' or fullText contains 'doc')"},
		{"  ?! ", ""},
	} {
		if got := driveQuery(tc.in); got != tc.want {
			t.Errorf("driveQuery(%q)\n got %s\nwant %s", tc.in, got, tc.want)
		}
	}
}

// A file is read from the company's Drive when it can open it, and from the asker's own only when
// it cannot — so reading a company file never spends anybody's account.
func TestDriveReadTriesTheCompanyDriveFirst(t *testing.T) {
	f := newDriveFixture(t)
	c := f.call("USAM", "C1")

	out, note := f.run(t, c, "drive_read", `{"file_id":"fileCompanyCSV1"}`)
	if !strings.Contains(out, "Paying customers.csv — company Drive (Company Drive) · CSV") || !strings.Contains(out, "Globex,777") {
		t.Errorf("the company's CSV did not come back:\n%s", out)
	}
	if note.spentPersonal() || len(f.askedWith("sam-token")) != 0 {
		t.Error("reading a company file spent the asker's own account")
	}

	out, note = f.run(t, c, "drive_read", `{"file_id":"https://docs.google.com/document/d/fileSamSalary1/edit"}`)
	if !strings.Contains(out, "<@USAM>'s own Drive") || !strings.Contains(out, "for Sam only") {
		t.Errorf("a pasted link to their own Doc was not read from their own Drive:\n%s", out)
	}
	if !note.spentPersonal() {
		t.Error("reading their own file was not marked as spending their account")
	}
	var exported bool
	for _, s := range f.askedWith("sam-token") {
		exported = exported || strings.Contains(s, "/drive/v3/files/fileSamSalary1/export?mimeType=text%2Fmarkdown")
	}
	if !exported {
		t.Errorf("a Google Doc was not exported: %v", f.seen)
	}

	out, _ = f.run(t, c, "drive_read", `{"file_id":"fileBothPricing","from":"company"}`)
	if !strings.Contains(out, "first tab only") || !strings.Contains(out, "starter,10") {
		t.Errorf("a Sheet did not come back as CSV:\n%s", out)
	}

	if out, _ := f.run(t, c, "drive_read", `{"file_id":"../../calendar/v3/x"}`); !strings.HasPrefix(out, "error:") {
		t.Errorf("a path went through as a file id:\n%s", out)
	}
}

// A lookup in a long file: the whole file is read and only the matching rows come back, under the
// CSV's header so the columns still have names.
func TestDriveReadMatchReturnsOnlyTheMatchingRows(t *testing.T) {
	f := newDriveFixture(t)
	out, _ := f.run(t, f.call("UNEW", "C1"), "drive_read", `{"file_id":"fileCompanyCSV1","match":"GLOBEX"}`)
	if !strings.Contains(out, "org,org_id\nGlobex,777") || strings.Contains(out, "Acme") {
		t.Errorf("the match did not come back as the header and the one row:\n%s", out)
	}
	out, _ = f.run(t, f.call("UNEW", "C1"), "drive_read", `{"file_id":"fileCompanyCSV1","match":"initech"}`)
	if !strings.Contains(out, `No line of it contains "initech"`) {
		t.Errorf("a match with no rows did not say so:\n%s", out)
	}
}

// The tools come with a Drive, of either kind, and not otherwise: not with an unrelated
// connection, not with a Google connection whose Drive part was left unticked, and not with a key
// that claims all of www.googleapis.com without saying it is for Drive.
func TestDriveToolsComeWithADrive(t *testing.T) {
	f := newDriveFixture(t)
	ctx := context.Background()
	calendarOnly := &Connection{ID: 90, Name: "Calendar", Preset: "google", CredType: "oauth_user",
		AllowedHosts: []string{"www.googleapis.com"}, PathPrefixes: []string{"/calendar/v3"}}
	apiKey := &Connection{ID: 91, Name: "Search key", CredType: "bearer", AllowedHosts: []string{"www.googleapis.com"}}
	hubspot := &Connection{ID: 92, Name: "HubSpot", CredType: "bearer", AllowedHosts: []string{"api.hubapi.com"}}
	for _, tc := range []struct {
		name  string
		rules []Rule
		want  bool
	}{
		{"company Drive", []Rule{{Conn: f.company}}, true},
		{"their own Drive", []Rule{{Conn: f.google}}, true},
		{"an unrelated service", []Rule{{Conn: hubspot}}, false},
		{"Google without Drive", []Rule{{Conn: calendarOnly}}, false},
		{"a host-wide key for something else", []Rule{{Conn: apiKey}}, false},
	} {
		c := f.call("USAM", "C1")
		c.Access = &Access{Rules: tc.rules}
		tools := f.a.toolsFor(ctx, c)
		_, search := tools["drive_search"]
		_, read := tools["drive_read"]
		if search != tc.want || read != tc.want {
			t.Errorf("%s: drive_search %v, drive_read %v, want %v", tc.name, search, read, tc.want)
		}
		prompt := f.a.systemPrompt(ctx, c)
		if got := strings.Contains(prompt, "find a file with drive_search"); got != tc.want {
			t.Errorf("%s: the prompt's Drive paragraph is there: %v, want %v", tc.name, got, tc.want)
		}
	}
	// And where the company's Drive is in reach, the prompt rules out the answer that started this.
	prompt := f.a.systemPrompt(ctx, f.call("UNEW", "C1"))
	if !strings.Contains(prompt, "never ask someone to connect their own account to reach one") {
		t.Errorf("the prompt leaves room to send somebody to connect for a company file:\n%s", truncate(prompt, 3000))
	}
}

// What Activity keeps of a Drive call that spent somebody's own account: its size. Not the first
// sentence, which counts their files, and not an error, which names one.
func TestDriveCallsOnSomebodysOwnAccountKeepOnlyTheirSize(t *testing.T) {
	got := privateResult("drive_search", ProxyRequest{}, "3 found in their own Drive for \"salary\"", 120, nil)
	if !strings.Contains(got, `"bytes":120`) || strings.Contains(got, "salary") {
		t.Errorf("the log kept more than the size: %s", got)
	}
	got = privateResult("drive_read", ProxyRequest{}, "", 0, errors.New("No Drive in this channel can open Salary review"))
	if strings.Contains(got, "Salary") {
		t.Errorf("the log kept the error: %s", got)
	}

	// And a refused repeat, whose arguments are the words that were searched for.
	f := newDriveFixture(t)
	ctx := context.Background()
	f.a.refuseTool(ctx, f.call("USAM", "C1"), "drive_search", `{"query":"salary review"}`)
	rows, err := f.st.RecentToolCalls(ctx, orgID, "", 5, false, "")
	if err != nil || len(rows) == 0 {
		t.Fatalf("nothing logged: %v", err)
	}
	if !strings.HasPrefix(rows[0].Args, privateMark) || strings.Contains(rows[0].Args, "salary") {
		t.Errorf("a refused Drive search logged its words: %s", rows[0].Args)
	}
}

// ---- routing: http_request on a shared host ----

// Shared-host routing puts the asker's own account first for a Drive URL, and lets a shared
// credential stand in for somebody who has not connected — but it only ever let one in that
// claimed the same path, and a Drive service account is made claiming none. So everyone who had
// not connected was sent a Connect card for a file the company's Drive could read. A credential
// whose preset is Drive now stands in for a Drive read; for anything else on the host, and for a
// write, the answer is still the link.
func TestAHostWideDriveCredentialStandsInForSomebodyNotConnected(t *testing.T) {
	f := newDriveFixture(t)
	ctx := context.Background()
	acc := f.call("UNEW", "C1").Access
	audit := ProxyAudit{TeamID: "T1", Requester: "UNEW"}

	resp, err := f.a.proxy.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: "https://www.googleapis.com/drive/v3/files?q=x"}, audit, false)
	if err != nil {
		t.Fatalf("a Drive read by somebody not connected: %v", err)
	}
	if resp.Conn.ID != f.company.ID || resp.Skipped != f.google.Name {
		t.Errorf("answered by %q (skipped %q), want the company's Drive standing in for their own", resp.Conn.Name, resp.Skipped)
	}
	if note := connNote(resp); !strings.Contains(note, "not their own account") {
		t.Errorf("the stand-in is not labelled as the shared view: %q", note)
	}

	if _, err := f.a.proxy.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: "https://www.googleapis.com/calendar/v3/calendars/primary/events"},
		audit, false); !errors.Is(err, ErrNeedsUserAuth) {
		t.Errorf("a calendar read went to the Drive credential: err=%v", err)
	}
	if _, err := f.a.proxy.Do(ctx, orgID, acc, ProxyRequest{Method: "POST", URL: "https://www.googleapis.com/upload/drive/v3/files", Body: "{}"},
		audit, true); !errors.Is(err, ErrNeedsUserAuth) {
		t.Errorf("a Drive write went out as the company's credential rather than asking them to connect: err=%v", err)
	}

	// A connected asker is still answered as themselves.
	resp, err = f.a.proxy.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: "https://www.googleapis.com/drive/v3/files?q=x"},
		ProxyAudit{TeamID: "T1", Requester: "USAM"}, false)
	if err != nil || resp.Conn.ID != f.google.ID || resp.Account != "USAM@example.com" {
		t.Errorf("a connected asker's Drive read: conn=%v account=%q err=%v", resp.Conn, resp.Account, err)
	}
}

// "/drive/v3/" claims nothing "/drive/v3" does not. Counted as written it was one character longer,
// and that put a shared credential ahead of everybody's own account — the bug shared-host routing
// fixed, back through a typo the console accepts.
func TestATrailingSlashDoesNotPutASharedCredentialFirst(t *testing.T) {
	f := newDriveFixture(t)
	f.company.PathPrefixes = []string{"/drive/v3/", "/upload/drive/v3/"}
	acc := f.call("USAM", "C1").Access
	resp, err := f.a.proxy.Do(context.Background(), orgID, acc, ProxyRequest{Method: "GET", URL: "https://www.googleapis.com/drive/v3/files?q=x"},
		ProxyAudit{TeamID: "T1", Requester: "USAM"}, false)
	if err != nil || resp.Conn.ID != f.google.ID {
		t.Errorf("a connected asker's Drive read went to %v (err %v), want their own account", resp.Conn, err)
	}
}
