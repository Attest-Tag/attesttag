package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// Files that come attached to a Teams message. Teams hands a bot three kinds, and they are reached
// three different ways:
//
//   - A file somebody sends the bot in a one-to-one chat arrives as a pre-authorised download link
//     to the sender's OneDrive, which is fetched with no credential at all. It is the reason the
//     app package says supportsFiles: without it Teams offers no way to attach a file to the bot.
//   - An image pasted into a message arrives as an address on the Bot Framework's own attachment
//     service, fetched with the bot's token — which therefore goes only to Microsoft's hosts.
//   - A file attached in a channel or a group chat lives in SharePoint, and reading it takes a
//     Graph permission (Files.Read.All) that a tenant admin has to grant and this app does not ask
//     for. The model is told the file is there and what it is called, which is all that can be
//     said about it honestly.
//
// Everything downstream of the log reads files as the file objects it already knows, with the kind
// in Mode, so a Teams file is summarised, cached and put in front of the model exactly as a Slack
// one is. Only fetching the bytes differs, and that is the transport's.

// msInAttachment is an attachment on an inbound activity.
type msInAttachment struct {
	ContentType string          `json:"contentType"`
	ContentURL  string          `json:"contentUrl"`
	Name        string          `json:"name"`
	Content     json.RawMessage `json:"content"`
}

const (
	msFileDownload  = "download"
	msFileInline    = "inline"
	msFileReference = "reference"
	msFileMode      = "msteams-" // slack.File.Mode for a Teams file, before its kind
)

// teamsFiles reads what came attached to a message. The copy of the message itself that Teams
// attaches as HTML, and any card, are not files.
func teamsFiles(atts []msInAttachment) []msFile {
	var out []msFile
	for _, a := range atts {
		ct := strings.ToLower(a.ContentType)
		switch {
		case ct == "application/vnd.microsoft.teams.file.download.info":
			var c struct {
				DownloadURL string `json:"downloadUrl"`
				UniqueID    string `json:"uniqueId"`
				FileType    string `json:"fileType"`
			}
			if json.Unmarshal(a.Content, &c) != nil || c.DownloadURL == "" {
				continue
			}
			out = append(out, msFile{ID: nonEmpty(c.UniqueID, fileKey(c.DownloadURL)), Name: nonEmpty(a.Name, "file"),
				Type: mimeOf(a.Name, c.FileType), URL: c.DownloadURL, Kind: msFileDownload})
		case strings.HasPrefix(ct, "image/") && a.ContentURL != "":
			name := nonEmpty(a.Name, "image"+extensionOf(ct))
			out = append(out, msFile{ID: fileKey(a.ContentURL), Name: name, Type: ct, URL: a.ContentURL, Kind: msFileInline})
		case ct == "reference" || (a.ContentURL != "" && a.Name != "" && !strings.HasPrefix(ct, "application/vnd.microsoft.card") && ct != "text/html"):
			out = append(out, msFile{ID: fileKey(nonEmpty(a.ContentURL, a.Name)), Name: nonEmpty(a.Name, "file"),
				Type: mimeOf(a.Name, ""), Kind: msFileReference})
		}
	}
	return out
}

// fileKey is a stable id for a file Teams gives no id of its own, so its extracted text can be
// cached under it like a Slack file's.
func fileKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "msf-" + hex.EncodeToString(sum[:8])
}

func mimeOf(name, fileType string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" && fileType != "" {
		ext = "." + strings.ToLower(strings.TrimPrefix(fileType, "."))
	}
	if t := mime.TypeByExtension(ext); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	return ""
}

func extensionOf(contentType string) string {
	if exts, _ := mime.ExtensionsByType(contentType); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// asSlackFile is a Teams file as the file object the rest of the code reads.
func (f msFile) asSlackFile() slack.File {
	return slack.File{ID: f.ID, Name: f.Name, Title: f.Name, Mimetype: f.Type,
		Filetype:           strings.TrimPrefix(strings.ToLower(filepath.Ext(f.Name)), "."),
		URLPrivateDownload: f.URL, Mode: msFileMode + f.Kind}
}

// Where each kind may be fetched from. A download link is OneDrive's, which is SharePoint; an
// inline image is on the Bot Framework's attachment service, or Skype's media service behind it,
// and only those are ever sent the bot's token. Variables so a test can point them at itself.
var (
	msDownloadHostOK = func(u *url.URL) bool {
		return u.Scheme == "https" && u.User == nil && hostUnder(u.Hostname(), "sharepoint.com", "sharepoint.us", "sharepoint-df.com")
	}
	msInlineHostOK = func(u *url.URL) bool {
		return u.User == nil && (msServiceURLOK(u.String()) || (u.Scheme == "https" && hostUnder(u.Hostname(), "asm.skype.com")))
	}
)

// hostUnder is whether host is one of the domains or a name under one.
func hostUnder(host string, domains ...string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// downloadFile reads one attached file's bytes.
func (t *msteamsTransport) downloadFile(ctx context.Context, f slack.File) ([]byte, error) {
	kind := strings.TrimPrefix(f.Mode, msFileMode)
	if kind == msFileReference {
		return nil, fmt.Errorf("%s is stored in SharePoint, and reading files attached in a Teams channel or group chat "+
			"needs a Microsoft Graph permission this app does not have", f.Name)
	}
	u, err := url.Parse(strings.TrimSpace(f.URLPrivateDownload))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s has no address to fetch it from", f.Name)
	}
	hostOK := msDownloadHostOK
	if kind == msFileInline {
		hostOK = msInlineHostOK
	}
	if !hostOK(u) {
		return nil, fmt.Errorf("%s is not on a Microsoft host this bot fetches files from", f.Name)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if kind == msFileInline {
		tok, err := t.c.botToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{
		Timeout:   60 * time.Second,
		Transport: t.c.http.Transport,
		// The token, when there is one, must not follow a redirect off Microsoft's hosts, and the
		// Go client only drops it for a different domain — so a redirect anywhere else is refused.
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects fetching a Teams file")
			}
			if !hostOK(r.URL) {
				return fmt.Errorf("a Teams file redirected to %s", r.URL.Host)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if kind == msFileDownload && (resp.StatusCode == 401 || resp.StatusCode == 403) {
			// Teams' links last about an hour. The text of a document is kept after the first
			// read, so this is an image, or a file first read after its link ran out.
			return nil, fmt.Errorf("the link Teams gave for %s has expired; ask for it to be sent again", f.Name)
		}
		return nil, fmt.Errorf("fetching %s answered %s", f.Name, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxFileBytes))
}
