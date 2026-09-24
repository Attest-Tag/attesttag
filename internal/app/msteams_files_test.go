package app

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// A file sent to the bot in a Teams chat and an image pasted into a message are read the way a
// Slack attachment is: a document's text goes in front of the model, an image goes to it as an
// image. A OneDrive link is fetched with no credential, the bot's token goes to Microsoft's
// attachment service and nowhere else, and a file in SharePoint is named and explained rather than
// silently missing. The copy of the message Teams attaches as HTML is not a file.
func TestFilesAttachedInTeamsReachTheModel(t *testing.T) {
	b, mux, f, st := teamsTestBot(t, "Noted.")
	linkTeams(t, st, mux, f)
	ctx := context.Background()
	f.mu.Lock()
	f.files["refunds.txt"] = "Refunds take five working days."
	f.files["img:shot"] = "\x89PNG not really"
	f.mu.Unlock()

	before := len(f.messages())
	deliverTeams(t, mux, f.sign(nil), f.activity("what do these say?", map[string]any{
		"attachments": []map[string]any{
			{"contentType": "application/vnd.microsoft.teams.file.download.info", "name": "refunds.txt",
				"content": map[string]any{"downloadUrl": f.srv.URL + "/files/refunds.txt", "uniqueId": "u-1", "fileType": "txt"}},
			{"contentType": "image/png", "contentUrl": f.srv.URL + "/v3/attachments/shot/views/original"},
			{"contentType": "reference", "contentUrl": "https://contoso.sharepoint.com/sites/x/plan.docx", "name": "plan.docx"},
			{"contentType": "text/html", "content": "<p>what do these say?</p>"},
		},
	}))
	f.waitForMessages(before + 1)

	sl, err := b.slacks.For(ctx, "msteams:"+teamsOrg)
	if err != nil {
		t.Fatal(err)
	}
	thread, _ := sl.Thread(ctx, "a:chat-ana", msteamsChatThread, 0)
	var files []slack.File
	for _, m := range thread {
		if len(m.files) > 0 {
			files = m.files
		}
	}
	if len(files) != 3 || files[0].Name != "refunds.txt" || files[1].Name != "image.png" || files[2].Name != "plan.docx" {
		t.Fatalf("the thread reads back with files %+v", files)
	}

	c := &Call{SL: sl, TeamID: sl.TeamID}
	doc, err := b.agent.fetchFile(ctx, c, files[0])
	if err != nil || !strings.Contains(doc.Text, "five working days") {
		t.Errorf("the document read as %+v, %v", doc, err)
	}
	img, err := b.agent.fetchFile(ctx, c, files[1])
	if err != nil || !strings.HasPrefix(img.ImageDataURL, "data:image/png;base64,") {
		t.Errorf("the pasted image read as %+v, %v", img, err)
	}
	if _, err := b.agent.fetchFile(ctx, c, files[2]); err == nil || !strings.Contains(err.Error(), "SharePoint") {
		t.Errorf("a SharePoint file should be explained, not fetched: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.downloadAuth) != 0 {
		t.Errorf("a pre-authorised OneDrive link was sent a credential: %v", f.downloadAuth)
	}
}

// The bot's token goes only to Microsoft's own hosts, and a download link only to SharePoint's.
func TestTeamsFilesAreFetchedOnlyFromMicrosoft(t *testing.T) {
	for raw, want := range map[string][2]bool{ // download ok, inline ok
		"https://contoso-my.sharepoint.com/personal/x/_layouts/15/download.aspx?UniqueId=1": {true, false},
		"https://smba.trafficmanager.net/amer/v3/attachments/0-x/views/original":            {false, true},
		"https://us-api.asm.skype.com/v1/objects/0-x/views/imgo":                            {false, true},
		"https://contoso.sharepoint.com.evil.com/file":                                      {false, false},
		"https://evil.com/smba.trafficmanager.net/v3/attachments/1":                         {false, false},
		"http://contoso.sharepoint.com/file":                                                {false, false},
		"https://user@contoso.sharepoint.com/file":                                          {false, false},
	} {
		u := mustParseURL(t, raw)
		if got := [2]bool{msDownloadHostOK(u), msInlineHostOK(u)}; got != want {
			t.Errorf("%s: download %v inline %v, want %v", raw, got[0], got[1], want)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
