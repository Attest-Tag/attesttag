package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/slack-go/slack"
)

// Files people attach in a thread become part of the model's context: text-like files and
// PDFs as extracted text (cached per file id), images as image parts when the model can see.

// What reaches the model, as against what exists. A file can be worth having on the ticket and
// still be the wrong thing to put in front of a model: an image is base64 in the prompt and
// inflates by a third on the way, and a log nobody truncates is the whole context window.
//
// Over a cap the file is not refused and not an error — it is named. Whoever reads the answer can
// see the file is there, the ticket carries the whole of it, and nothing pretends to have read
// something it did not.
const (
	// maxFileBytes is the hard one: past it the bytes are not fetched at all.
	maxFileBytes = 12 << 20
	// maxModelImageBytes is what may be spent on one picture. A 12 MB screenshot is ~16 MB of
	// base64 and would crowd out the conversation it was meant to explain.
	maxModelImageBytes = 4 << 20
	// maxModelFileChars is how much extracted text goes in. The rest is on the file itself.
	maxModelFileChars = 60_000
)

type attachment struct {
	Name, Text   string
	ImageDataURL string
	// TooBig: the file exists and its content is not here. Nothing was read, so nothing may be
	// said about what is in it.
	TooBig bool
	// Clipped: the text is the first maxModelFileChars of a longer file. The model has to know,
	// or it counts what it can see and answers with a number that is simply wrong.
	Clipped bool
}

// appendFileOnce records a file the turn has been shown. A thread is replayed on every turn and
// the same attachment comes round each time, so this is a set rather than a list.
func appendFileOnce(files []slack.File, f slack.File) []slack.File {
	for _, e := range files {
		if e.ID == f.ID {
			return files
		}
	}
	return append(files, f)
}

// clipForModel bounds extracted text and says whether it had to.
func clipForModel(s string) (string, bool) {
	if len(s) <= maxModelFileChars {
		return s, false
	}
	return truncate(s, maxModelFileChars), true
}

// slackFileHost reports whether a URL is one a workspace bot token may be sent to. Slack serves
// file bytes from files.slack.com and redirects within its own domain; anywhere else the token
// is not merely useless but a credential handed to a stranger.
func slackFileHost(u *url.URL) bool {
	if u == nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return h == "slack.com" || strings.HasSuffix(h, ".slack.com")
}

// slackFileTransport carries one file download. The guarded transport underneath resolves each
// host and refuses to dial anything that is not a public address; this layer decides who is
// entitled to the credential. Slack signs its own storage URLs, so a redirect off Slack is
// followed without the token rather than refused.
type slackFileTransport struct {
	token string
	base  http.RoundTripper
}

func (t *slackFileTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.token != "" && slackFileHostFn(r.URL) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(r)
}

// These two are variables for the same reason mcpURLCheck and mcpBaseTransport are, and they
// only work as a pair: a test that points the host check at its own server still ends at a
// dialler that will not connect to the loopback address that server is listening on.
var (
	slackFileHostFn        = slackFileHost
	slackFileBaseTransport = publicTransport
)

// downloadSlackFile reads one file's bytes with the workspace's own bot token. Split out because
// two things want them: putting a file in front of the model, and putting it on a ticket.
func downloadSlackFile(ctx context.Context, slacks *ChatRegistry, teamID string, f slack.File) ([]byte, error) {
	src := f.URLPrivateDownload
	if src == "" {
		src = f.URLPrivate
	}
	// A file object is Slack-signed, but its address is not always Slack's: a remote file
	// (files.remote.add) carries whatever url_private the app that registered it chose. So the
	// destination is checked before anything is sent, not trusted because the envelope was.
	u, err := url.Parse(strings.TrimSpace(src))
	if err != nil || !slackFileHostFn(u) {
		return nil, fmt.Errorf("%s is not stored on Slack, so it cannot be read with this workspace's token", f.Name)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	// files.slack.com is not the Web API, so this is the one place a bot token is used by
	// hand. It has to be the token of the workspace the file lives in.
	tok, err := slacks.Token(ctx, teamID)
	if err != nil {
		return nil, err
	}
	// The token rides on the transport rather than on the request: a header set here would be
	// copied onto whatever a redirect pointed at, which is the whole thing being avoided.
	resp, err := slackFileClient(tok).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxFileBytes))
}

func slackFileClient(token string) *http.Client {
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: &slackFileTransport{token: token, base: slackFileBaseTransport()},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects fetching a Slack file")
			}
			return nil
		},
	}
}

func (a *Agent) fetchFile(ctx context.Context, c *Call, f slack.File) (*attachment, error) {
	if f.Size > maxFileBytes {
		// Named, not refused: the file is real and belongs on the ticket. Fetching 12 MB to
		// decide we cannot use it would be spending the download to say so.
		return &attachment{Name: fileLabel(f), TooBig: true}, nil
	}
	if t, ok := a.store.FileText(ctx, c.TeamID, f.ID); ok {
		return &attachment{Name: fileLabel(f), Text: t}, nil
	}
	// A mail forwarded to a channel's Slack address. Slack has already parsed it, so the fields
	// on the file object are the email itself — subject, sender, plain-text body — and the bytes
	// behind url_private are only the HTML part. Downloading that and stripping the tags back out
	// of it is work Slack has done better: on one real forward it was the difference between
	// ~2k tokens and 86k, most of the rest being markup.
	if f.Filetype == "email" {
		if att, ok := a.emailFile(ctx, c, f); ok {
			return att, nil
		}
		// No plain text anywhere: read the HTML part like any other file, below.
	}
	raw, err := c.SL.download(ctx, a.slacks, c.TeamID, f)
	if err != nil {
		return nil, err
	}
	mime := strings.ToLower(f.Mimetype)
	ext := strings.ToLower(filepath.Ext(f.Name))
	switch {
	case strings.HasPrefix(mime, "image/"):
		if len(raw) > maxModelImageBytes {
			return &attachment{Name: fileLabel(f), TooBig: true}, nil
		}
		return &attachment{Name: fileLabel(f), ImageDataURL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)}, nil
	case mime == "application/pdf" || ext == ".pdf":
		tmp, err := os.CreateTemp("", "attest-*.pdf")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		tmp.Write(raw)
		tmp.Close()
		out, err := pdfToText(ctx, tmp.Name())
		if err != nil {
			return nil, err
		}
		text, cut := clipForModel(out)
		a.store.PutFileText(ctx, c.TeamID, f.ID, f.Name, text)
		return &attachment{Name: fileLabel(f), Text: text, Clipped: cut}, nil
	case strings.HasPrefix(mime, "text/") || mime == "application/json" || docExts[ext] || ext == ".log" || ext == ".yaml" || ext == ".yml":
		text := string(raw)
		if mime == "text/html" || ext == ".html" || ext == ".htm" {
			text = cleanHTML(text)
		}
		text, cut := clipForModel(text)
		a.store.PutFileText(ctx, c.TeamID, f.ID, f.Name, text)
		return &attachment{Name: fileLabel(f), Text: text, Clipped: cut}, nil
	}
	// Nothing here can read this type, which is not the same as a failure: the model is told the
	// file is there and what it is called, and that is all anybody could honestly say about it.
	return &attachment{Name: fileLabel(f)}, nil
}

// ---- forwarded mail ----

// slackAPIBase is where files.info is called. A variable for the same reason slackFileHostFn is.
var slackAPIBase = "https://slack.com/api"

// emailInfoTTL is how long one mail's parsed form is held. A thread about a forwarded mail reads
// it on every turn, and the mail does not change.
const emailInfoTTL = 10 * time.Minute

// maxEmailAttachments bounds what one mail can drag in. The turn already caps how many files it
// will read; this stops a mail with forty attachments from being the reason the rest are dropped.
const maxEmailAttachments = 4

// emailInfo is a forwarded mail as Slack parsed it: the headers, the plain-text body, and the
// files that came attached to the mail itself.
type emailInfo struct {
	at           time.Time
	Subject      string
	From, To, Cc []slack.EmailFileUserInfo
	Date         string
	PlainText    string
	Attachments  []slack.File
}

// emailInfos holds them per workspace and file, for the reason every other cache here is keyed
// that way: one tenant's mail is not another tenant's to read.
type emailInfos struct {
	mu sync.Mutex
	m  map[string]emailInfo
}

// emailFileInfo reads one email file with files.info and parses the response itself.
//
// Raw rather than through slack-go, because slack-go's File models a mail's headers and body but
// not its attachments — and the attachment is usually the whole report. Somebody writes "this
// image does not OCR" and attaches the image; Slack uploads that as a file of its own and lists
// it on the *mail*, not on the message. Everything that reads message.files therefore sees the
// mail and never the picture, and the model ends up saying the mail arrived with nothing attached
// while the person looking at Slack can see it perfectly well.
func (a *Agent) emailFileInfo(ctx context.Context, c *Call, fileID string) (emailInfo, bool) {
	key := c.TeamID + "|" + fileID
	a.mail.mu.Lock()
	if hit, ok := a.mail.m[key]; ok && time.Since(hit.at) < emailInfoTTL {
		a.mail.mu.Unlock()
		return hit, true
	}
	a.mail.mu.Unlock()

	tok, err := a.slacks.Token(ctx, c.TeamID)
	if err != nil {
		return emailInfo{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", slackAPIBase+"/files.info?file="+url.QueryEscape(fileID), nil)
	if err != nil {
		return emailInfo{}, false
	}
	resp, err := slackFileClient(tok).Do(req)
	if err != nil {
		slog.Warn("could not read a forwarded mail", "file", fileID, "err", err)
		return emailInfo{}, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes))
	if err != nil {
		return emailInfo{}, false
	}
	info, ok := parseEmailInfo(raw, fileID)
	if !ok {
		return emailInfo{}, false
	}
	a.mail.mu.Lock()
	if a.mail.m == nil {
		a.mail.m = map[string]emailInfo{}
	}
	a.mail.m[key] = info
	a.mail.mu.Unlock()
	return info, true
}

// parseEmailInfo reads one files.info response. Split from the call so the field names — which
// are the whole risk here, since slack-go does not model them — are checked by a test against a
// real response rather than by a deployment.
func parseEmailInfo(raw []byte, fileID string) (emailInfo, bool) {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		File  struct {
			Subject string                    `json:"subject"`
			From    []slack.EmailFileUserInfo `json:"from"`
			To      []slack.EmailFileUserInfo `json:"to"`
			Cc      []slack.EmailFileUserInfo `json:"cc"`
			Headers struct {
				Date string `json:"date"`
			} `json:"headers"`
			PlainText        string `json:"plain_text"`
			PreviewPlainText string `json:"preview_plain_text"`
			Attachments      []struct {
				Filename    string `json:"filename"`
				Size        int    `json:"size"`
				Mimetype    string `json:"mimetype"`
				URL         string `json:"url"`
				SlackFileID string `json:"slack_file_id"`
			} `json:"attachments"`
		} `json:"file"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || !out.OK {
		slog.Warn("could not read a forwarded mail", "file", fileID, "slack_error", out.Error, "err", err)
		return emailInfo{}, false
	}
	info := emailInfo{
		at: time.Now(), Subject: out.File.Subject, From: out.File.From, To: out.File.To,
		Cc: out.File.Cc, Date: out.File.Headers.Date,
		PlainText: nonEmpty(strings.TrimSpace(out.File.PlainText), strings.TrimSpace(out.File.PreviewPlainText)),
	}
	for i, at := range out.File.Attachments {
		if at.URL == "" || len(info.Attachments) >= maxEmailAttachments {
			continue
		}
		// slack_file_id is the file Slack made of it, and is what the extracted-text cache keys
		// on. A mail that carries no id still reads, under a key of its own that cannot collide
		// with a real one.
		id := at.SlackFileID
		if id == "" {
			id = fmt.Sprintf("%s:att:%d", fileID, i)
		}
		info.Attachments = append(info.Attachments, slack.File{
			ID: id, Name: nonEmpty(at.Filename, "attachment"), Title: at.Filename,
			Mimetype: at.Mimetype, Size: at.Size, URLPrivate: at.URL,
		})
	}
	return info, true
}

// withEmailAttachments expands a message's file list so that a forwarded mail brings in whatever
// came attached to it. Slack hangs those off the mail rather than off the message, so without
// this they are invisible to every turn in the thread.
func (a *Agent) withEmailAttachments(ctx context.Context, c *Call, files []slack.File) []slack.File {
	var extra []slack.File
	for _, f := range files {
		if f.Filetype != "email" {
			continue
		}
		if info, ok := a.emailFileInfo(ctx, c, f.ID); ok {
			extra = append(extra, info.Attachments...)
		}
	}
	if len(extra) == 0 {
		return files
	}
	return append(append([]slack.File{}, files...), extra...) // never the caller's own slice
}

// emailFile builds the attachment for a forwarded mail out of what Slack parsed, rather than out
// of the bytes. It reports false when there is no parsed body to use, which is the caller's cue to
// read the file the ordinary way.
//
// A file object arriving on a message event is trimmed — often it carries preview_plain_text and
// nothing else — so the body comes from files.info. That is one Web API call in place of a
// download of the HTML part, and it is where the header fields are complete.
func (a *Agent) emailFile(ctx context.Context, c *Call, f slack.File) (*attachment, bool) {
	body := ""
	if info, ok := a.emailFileInfo(ctx, c, f.ID); ok {
		body = info.PlainText
		// Header fields from the same object as the body, never half of each.
		f.Subject, f.From, f.To, f.Cc, f.Headers.Date = info.Subject, info.From, info.To, info.Cc, info.Date
	}
	if body == "" {
		body = nonEmpty(strings.TrimSpace(f.PlainText), strings.TrimSpace(f.PreviewPlainText))
	}
	if body == "" {
		return nil, false
	}
	text := truncate(emailHeader(f)+body, 60_000)
	a.store.PutFileText(ctx, c.TeamID, f.ID, fileLabel(f), text)
	return &attachment{Name: fileLabel(f), Text: text}, true
}

// emailHeader is what the model needs in order to answer "who sent this, when, about what"
// without digging it out of the body.
//
// Addresses are rendered exactly as they arrived and nothing here is evidence of anything: Slack
// passes on no SPF, DKIM or DMARC result, so a From line is a claim the sender made about
// themselves. Whoever reads this — model or person — is looking at a report, not an identity.
func emailHeader(f slack.File) string {
	var b strings.Builder
	line := func(label string, who []slack.EmailFileUserInfo) {
		var parts []string
		for _, u := range who {
			switch {
			case u.Name != "" && u.Address != "":
				parts = append(parts, u.Name+" <"+u.Address+">")
			case u.Address != "":
				parts = append(parts, u.Address)
			case u.Original != "":
				parts = append(parts, u.Original)
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", label, truncate(oneLine(strings.Join(parts, ", ")), 300))
		}
	}
	if s := strings.TrimSpace(f.Subject); s != "" {
		fmt.Fprintf(&b, "Subject: %s\n", truncate(oneLine(s), 300))
	}
	line("From", f.From)
	line("To", f.To)
	line("Cc", f.Cc)
	if d := strings.TrimSpace(f.Headers.Date); d != "" {
		fmt.Fprintf(&b, "Date: %s\n", truncate(oneLine(d), 100))
	}
	if b.Len() == 0 {
		return ""
	}
	b.WriteString("\n")
	return b.String()
}

// fileLabel is the name a file goes into the prompt under. An emailed file is named for its own
// subject and can arrive with that in either field, so both are tried before falling back to
// something that at least says what it is.
func fileLabel(f slack.File) string {
	for _, s := range []string{f.Name, f.Title, f.Subject} {
		if s = strings.TrimSpace(s); s != "" {
			return truncate(oneLine(s), 120)
		}
	}
	if f.Filetype == "email" {
		return "a forwarded email"
	}
	return "file " + f.ID
}

// userMessageWithFiles builds the user turn, inlining extracted text and attaching images.
func (a *Agent) userMessageWithFiles(ctx context.Context, c *Call, label, text string, files []slack.File) (openai.ChatCompletionMessageParamUnion, bool) {
	if len(files) == 0 {
		return openai.UserMessage(label + text), false
	}
	// A forwarded mail carries its own attachments, and Slack hangs them off the mail rather than
	// off the message they arrived on.
	files = a.withEmailAttachments(ctx, c, files)
	var parts []openai.ChatCompletionContentPartUnionParam
	var b strings.Builder
	b.WriteString(label + text)
	hasImage := false
	for i, f := range files {
		if i >= 4 {
			b.WriteString("\n[more files omitted]")
			break
		}
		c.Files = appendFileOnce(c.Files, f)
		att, err := a.fetchFile(ctx, c, f)
		if err != nil {
			slog.Warn("file", "name", f.Name, "err", err)
			fmt.Fprintf(&b, "\n[attached file %s could not be read: %s]", f.Name, err)
			continue
		}
		if att.ImageDataURL != "" {
			hasImage = true
			parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: att.ImageDataURL}))
			fmt.Fprintf(&b, "\n[attached image: %s]", att.Name)
			continue
		}
		if att.TooBig {
			fmt.Fprintf(&b, "\n[attached file: %s — too large to read here. It is on the thread; say so rather than guessing what is in it.]", att.Name)
			continue
		}
		if strings.TrimSpace(att.Text) == "" {
			fmt.Fprintf(&b, "\n[attached file: %s]", att.Name) // a type nothing extracts: its name is the whole truth
			continue
		}
		if att.Clipped {
			fmt.Fprintf(&b, "\n\n<file name=%q note=\"first %d characters of a longer file\">\n%s\n</file>",
				att.Name, maxModelFileChars, redact(att.Text))
			continue
		}
		fmt.Fprintf(&b, "\n\n<file name=%q>\n%s\n</file>", att.Name, redact(att.Text))
	}
	if !hasImage {
		return openai.UserMessage(b.String()), false
	}
	parts = append([]openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(b.String())}, parts...)
	return openai.UserMessage(parts), true
}

// ---- putting a file on the thing that was just created ----

// clickupAttachLimit bounds one upload. ClickUp takes more, but a ticket is a description with
// evidence on it, not a file share.
const clickupAttachLimit = 12 << 20

// attachToClickUp uploads files to a task that has just been created.
//
// It runs after the create, on the same approval: the person who approved "file a ticket about
// this mail" approved the customer's own screenshot going on it, and asking twice for the second
// half of one action is how attachments stop happening at all. Each upload is its own request to
// ClickUp because the API takes one file at a time.
//
// A failure here is reported and never fatal. The ticket exists; a missing attachment is worth a
// line in the thread, not an error that makes it look as though nothing was filed.
func attachToClickUp(ctx context.Context, px *Proxy, slacks *ChatRegistry, orgID int64, acc *Access,
	teamID, base, taskID string, files []slack.File, audit ProxyAudit) []string {
	var done []string
	for _, f := range files {
		if f.Size > clickupAttachLimit {
			slog.Warn("attachment too large for the ticket", "file", f.ID, "size", f.Size)
			continue
		}
		raw, err := downloadSlackFile(ctx, slacks, teamID, f)
		if err != nil {
			slog.Warn("attachment not read", "file", f.ID, "err", err)
			continue
		}
		name := fileLabel(f)
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, err := w.CreateFormFile("attachment", name)
		if err != nil {
			continue
		}
		part.Write(raw)
		w.WriteField("filename", name)
		w.Close()
		// The multipart body never passes through JSON: it is built here and handed straight to
		// the proxy, which records the method, host and path of a call but never its body.
		resp, err := px.Do(ctx, orgID, acc, ProxyRequest{
			Method: "POST", URL: base + "/api/v2/task/" + url.PathEscape(taskID) + "/attachment",
			Body:    buf.String(),
			Headers: map[string]string{"Content-Type": w.FormDataContentType()},
		}, audit, true)
		if err != nil || resp == nil || resp.Status >= 300 {
			slog.Warn("attachment not uploaded", "file", f.ID, "task", taskID, "err", err)
			continue
		}
		done = append(done, name)
	}
	return done
}

// clickupTaskCreate reports whether a request is the one that makes a task, and on which host.
// The attachment goes to the same host the task was made on: an organisation with two ClickUp
// connections must not have one's file land on the other's task.
func clickupTaskCreate(req ProxyRequest) (base string, ok bool) {
	if !strings.EqualFold(req.Method, "POST") {
		return "", false
	}
	u, err := url.Parse(req.URL)
	if err != nil || !strings.EqualFold(u.Hostname(), "api.clickup.com") {
		return "", false
	}
	if !strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/task") {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// createdTaskID reads the id out of what ClickUp answered. Without it there is nothing to attach
// to, and inventing one would attach the file to somebody else's task.
func createdTaskID(body string) string {
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return ""
	}
	return out.ID
}

// attachableFiles is what goes on a ticket filed from this turn: the evidence, not the envelope.
//
// The forwarded mail itself is left off. Its text is already the description, and a .html file of
// an Outlook signature block on every ticket is noise that makes the real attachment harder to
// find. What came attached to that mail is exactly what somebody wanted looked at.
func attachableFiles(c *Call) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, f := range c.Files {
		if f.Filetype == "email" || f.ID == "" {
			continue
		}
		out = append(out, f.ID)
		if len(out) >= maxEmailAttachments {
			break
		}
	}
	return out
}

// fileByID asks Slack for one file. A held write carries ids, so by the time it runs — which for
// an approval may be days later — the addressable file object has to be fetched again.
func fileByID(ctx context.Context, sl *Chat, id string) (slack.File, bool) {
	if sl == nil || id == "" {
		return slack.File{}, false
	}
	api, err := sl.slackAPI()
	if err != nil {
		slog.Warn("attachment not found", "file", id, "err", err)
		return slack.File{}, false
	}
	f, _, _, err := api.GetFileInfoContext(ctx, id, 0, 0)
	if err != nil || f == nil {
		slog.Warn("attachment not found", "file", id, "err", err)
		return slack.File{}, false
	}
	return *f, true
}
