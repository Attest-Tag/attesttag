package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
)

// Drive sync: a folder in Google Drive, copied into Documents and kept in step with it.
//
// This is deliberately not the same thing as the Drive *connection*, which lets the bot ask
// Drive a question while it is answering. A sync takes copies, so the text is in the index and
// every answer can cite it without a round trip — and so an answer still works when Drive is
// slow, or when the question was too vague for the model to have searched Drive well.
//
// The copies are a mirror, and drive_sync_files is what makes the mirror honest: every document
// a sync created is recorded there, so a file that stops coming back from Drive can be removed,
// and a document with no row was put there by a person and is never touched by this code.

const (
	driveHost = "www.googleapis.com"
	// How often the scheduled pass runs. Six hours is the same cadence as the re-index loop,
	// which is the other thing in the product that walks everything and costs real time.
	driveSyncEvery = 6 * time.Hour
	// A single file bigger than this is skipped with a note rather than pulled into memory and
	// then into the index. Drive holds video and disk images; the index holds prose.
	maxDriveFileBytes = 25 << 20
	// Drive pages at 1000 and a folder deeper than this is a checkout, not a document folder.
	maxDriveFiles      = 2000
	maxDriveFolderWalk = 200
)

// driveExport says what a Google-native file becomes on the way out. Native files have no bytes
// to download — they only exist as an export — so a type missing from this map is skipped.
//
// A spreadsheet exports its *first* sheet as CSV and nothing else; that is Drive's rule, not
// ours, and it is the reason a multi-tab sheet is worth keeping as a real .csv per tab instead.
var driveExport = map[string]struct{ mime, ext string }{
	"application/vnd.google-apps.document":     {"text/markdown", ".md"},
	"application/vnd.google-apps.spreadsheet":  {"text/csv", ".csv"},
	"application/vnd.google-apps.presentation": {"application/pdf", ".pdf"},
}

const driveFolderMime = "application/vnd.google-apps.folder"

// driveFile is the slice of Drive's file resource this code reads.
type driveFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Modified string `json:"modifiedTime"`
	MD5      string `json:"md5Checksum"`
	Size     string `json:"size"` // Drive sends int64 as a string
	Trashed  bool   `json:"trashed"`
}

// version is what decides whether a file has to be fetched again. md5 is exact but Drive only
// has one for uploaded bytes; a native Doc carries modifiedTime alone, which moves whenever
// anybody touches it — so natives are re-exported a little more often than strictly needed,
// which is the right way round.
func (f driveFile) version() string { return f.Modified + " " + f.MD5 }

// DriveReport is what one pass did, for the console and the run log.
type DriveReport struct {
	FolderName string   `json:"folder_name"`
	Added      int      `json:"added"`
	Updated    int      `json:"updated"`
	Removed    int      `json:"removed"`
	Unchanged  int      `json:"unchanged"`
	Skipped    []string `json:"skipped"` // name — reason, for files Drive has but the index cannot take
	Took       string   `json:"took"`
}

// driveIDPattern matches the id in every shape of Drive URL people actually paste:
// .../folders/<id>, .../drive/u/0/folders/<id>?usp=..., open?id=<id>, and a bare id.
var driveFolderURL = regexp.MustCompile(`/folders/([A-Za-z0-9_-]{10,})`)
var driveOpenURL = regexp.MustCompile(`[?&]id=([A-Za-z0-9_-]{10,})`)
var driveBareID = regexp.MustCompile(`^[A-Za-z0-9_-]{10,}$`)

// driveFolderID pulls the folder id out of whatever somebody pasted. Asking for "the id" and
// getting a URL is the normal case, so the URL is the input this is written for.
func driveFolderID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("paste a Drive folder link or its id")
	}
	if m := driveFolderURL.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if m := driveOpenURL.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if driveBareID.MatchString(s) {
		return s, nil
	}
	return "", fmt.Errorf("that does not look like a Drive folder link or id")
}

// ---- talking to Drive on a connection's credential ----

// driveAPI runs Drive calls on one connection. It borrows the proxy's credential handling —
// the same service-account exchange, the same token cache — but not its policy: the allow
// rules exist to bound what a *model* may reach, and this path is a console admin's own
// configuration, running deterministic calls against one host.
type driveAPI struct {
	proxy *Proxy
	conn  *Connection
	orgID int64
	// Where Drive is, and what dials it. Fields rather than constants only so a test can stand
	// a Drive up locally; outside a test these are always the real host and the proxy's own
	// client, which is the one with the SSRF guard on it.
	scheme, host string
	client       *http.Client
}

// driveOverride points Drive at a server a test stood up. The real client refuses loopback
// addresses — that guard is the point of it — so a test has to supply its own client as well
// as its own host. nil in every build that is not running tests.
type driveOverride struct {
	scheme, host string
	client       *http.Client
}

var driveTest *driveOverride

func newDriveAPI(proxy *Proxy, conn *Connection, orgID int64) (*driveAPI, error) {
	if conn == nil {
		return nil, errors.New("the credential this sync used has been deleted")
	}
	// Google Workspace shares this host and would pass the allow-list check below, so the
	// credential type is what turns it away: it holds one token per person, and a sync acts for
	// nobody in particular. Caught here as well as in the picker, because the API takes an id.
	if conn.CredType == "oauth_user" {
		return nil, fmt.Errorf("connection %q signs in per person, so it cannot keep a shared folder in step — use a service-account Drive credential", conn.Name)
	}
	scheme, host, client := "https", driveHost, proxy.client
	if driveTest != nil {
		scheme, host, client = driveTest.scheme, driveTest.host, driveTest.client
	}
	// Not every credential can be a Drive credential, and a mismatch here would send a
	// customer's Slack token to Google. The check is against the host this will actually call,
	// not against the constant, so it cannot pass for one host and then dial another.
	if !hostAllowed(conn, host) {
		return nil, fmt.Errorf("connection %q is not allowed to reach %s", conn.Name, host)
	}
	return &driveAPI{proxy: proxy, conn: conn, orgID: orgID, scheme: scheme, host: host, client: client}, nil
}

// hostAllowed says whether a connection's own allow-list reaches a host. A connection with no
// hosts listed reaches nothing: an empty list is not a wildcard anywhere else in this codebase
// and must not become one here.
func hostAllowed(conn *Connection, host string) bool {
	for _, h := range conn.AllowedHosts {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

// do runs one GET against Drive and hands back the response for the caller to read or close.
func (d *driveAPI) do(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := &url.URL{Scheme: d.scheme, Host: d.host, Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if err := d.proxy.inject(ctx, d.orgID, d.conn, req, ProxyAudit{}); err != nil {
		return nil, fmt.Errorf("%w: %w", errCredential, err)
	}
	res, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 2<<10))
		res.Body.Close()
		return nil, fmt.Errorf("drive %s: %s", res.Status, driveErrorMessage(body))
	}
	return res, nil
}

// errCredential marks a call that never reached Drive because the connection's own credential
// could not be made ready — a service-account key Google no longer knows, say. It is worth
// telling apart: "is the folder shared?" is the wrong question to put to that.
var errCredential = errors.New("credential")

// driveShareHint adds the question a folder that cannot be read usually comes down to. Not
// for a credential that failed before Drive was asked anything: sharing would not fix it.
func driveShareHint(err error) error {
	if errors.Is(err, errCredential) {
		return err
	}
	return fmt.Errorf("%w — is the folder shared with this connection?", err)
}

// driveErrorMessage digs the sentence out of Drive's error envelope, so a failed sync says
// "File not found" rather than forty lines of JSON.
func driveErrorMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return truncate(strings.TrimSpace(string(body)), 200)
}

func (d *driveAPI) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	res, err := d.do(ctx, path, q)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
}

// shared are the parameters that make a call see shared drives as well as My Drive. Drive
// treats them as opt-in, so leaving them off silently returns an empty folder for anything on
// a shared drive — which is where a company's documents usually are.
func shared(q url.Values) url.Values {
	q.Set("supportsAllDrives", "true")
	q.Set("includeItemsFromAllDrives", "true")
	return q
}

// meta reads one file's or folder's metadata, which is also how a folder id is verified before
// it is saved.
func (d *driveAPI) meta(ctx context.Context, id string) (*driveFile, error) {
	var f driveFile
	q := shared(url.Values{"fields": {"id,name,mimeType,modifiedTime,md5Checksum,size,trashed"}})
	if err := d.getJSON(ctx, "/drive/v3/files/"+url.PathEscape(id), q, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// list walks one folder, paging until Drive stops.
func (d *driveAPI) list(ctx context.Context, folderID string) ([]driveFile, error) {
	var out []driveFile
	page := ""
	for {
		q := shared(url.Values{
			"q":        {fmt.Sprintf("%q in parents and trashed = false", folderID)},
			"fields":   {"nextPageToken, files(id,name,mimeType,modifiedTime,md5Checksum,size,trashed)"},
			"pageSize": {"1000"},
			// A stable order means a capped folder takes the same files every pass, rather
			// than a different arbitrary thousand each time.
			"orderBy": {"name"},
		})
		if page != "" {
			q.Set("pageToken", page)
		}
		var res struct {
			Files         []driveFile `json:"files"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := d.getJSON(ctx, "/drive/v3/files", q, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Files...)
		if res.NextPageToken == "" || len(out) >= maxDriveFiles {
			return out, nil
		}
		page = res.NextPageToken
	}
}

// driveDir is a folder the walk is in: which one, and where under dest its files land.
type driveDir struct {
	ID  string
	Rel string
}

// walk visits everything under one folder, breadth-first so a capped walk still covers the
// top level. Subfolders are handed to visit like files, and descended into only when recurse
// is set. It stops at the same caps for every caller — a pass, and the preview of one — which
// is what lets the preview promise what the pass will take. The bool says it stopped short:
// too many folders, or a folder with more files than one listing takes.
func (d *driveAPI) walk(ctx context.Context, rootID, dest string, recurse bool, visit func(in driveDir, f driveFile) error) (bool, error) {
	capped := false
	queue := []driveDir{{ID: rootID, Rel: dest}}
	for i := 0; i < len(queue); i++ {
		if i >= maxDriveFolderWalk {
			return true, nil
		}
		cur := queue[i]
		files, err := d.list(ctx, cur.ID)
		if err != nil {
			return capped, err
		}
		if len(files) >= maxDriveFiles {
			capped = true
		}
		for _, f := range files {
			if ctx.Err() != nil {
				return capped, ctx.Err()
			}
			if err := visit(cur, f); err != nil {
				return capped, err
			}
			if f.MimeType == driveFolderMime && recurse {
				queue = append(queue, driveDir{ID: f.ID, Rel: path.Join(cur.Rel, driveSafeName(f.Name))})
			}
		}
	}
	return capped, nil
}

// open returns the bytes to store for one file: the file itself, or Drive's export of it when
// it is a native Doc, Sheet or Slides.
func (d *driveAPI) open(ctx context.Context, f driveFile) (io.ReadCloser, error) {
	if exp, ok := driveExport[f.MimeType]; ok {
		return d.body(ctx, "/drive/v3/files/"+url.PathEscape(f.ID)+"/export", url.Values{"mimeType": {exp.mime}})
	}
	return d.body(ctx, "/drive/v3/files/"+url.PathEscape(f.ID), shared(url.Values{"alt": {"media"}}))
}

func (d *driveAPI) body(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
	res, err := d.do(ctx, path, q)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// ---- what a Drive file becomes in Documents ----

// docNameFor gives a Drive file the name it will have as a document, or "" for a file the
// index has no way to read. A native Doc has no extension of its own, so it takes the one its
// export implies; an uploaded file keeps its own name when that name is already a type we take.
func docNameFor(f driveFile) (string, string) {
	if exp, ok := driveExport[f.MimeType]; ok {
		return driveSafeName(f.Name) + exp.ext, ""
	}
	if strings.HasPrefix(f.MimeType, "application/vnd.google-apps.") {
		return "", "Google " + strings.TrimPrefix(f.MimeType, "application/vnd.google-apps.") + " — no text to export"
	}
	name := driveSafeName(f.Name)
	if !docExts[strings.ToLower(path.Ext(name))] {
		return "", "not a type the index reads"
	}
	return name, ""
}

// driveSafeName makes a Drive title safe as one path segment. Drive allows a slash in a title
// and the documents tree reads a slash as a folder, so a file called "Q3 / Q4 plan" must not
// quietly become a folder called "Q3 ".
var driveUnsafe = regexp.MustCompile(`[/\\]+`)

func driveSafeName(name string) string {
	name = driveUnsafe.ReplaceAllString(strings.TrimSpace(name), "-")
	// A leading dot hides a document from the indexer (cleanRel refuses it outright), and a
	// Drive title beginning with one is far more likely to be a title than an intent to hide.
	name = strings.TrimLeft(name, ". ")
	if name == "" {
		name = "untitled"
	}
	return truncate(name, 120)
}

// planDriveFile says what one Drive file becomes: the document path it would be stored at, or
// the reason it would not be stored. A pass and the preview of one both go through it, so the
// preview cannot promise a file the pass then refuses.
func planDriveFile(dir string, f driveFile) (rel, skip string) {
	name, why := docNameFor(f)
	if name == "" {
		return "", why
	}
	if n := driveSize(f); n > maxDriveFileBytes {
		return "", fmt.Sprintf("%s is over the %s limit", humanBytes(int(n)), humanBytes(maxDriveFileBytes))
	}
	rel, err := cleanRel(path.Join(dir, name))
	if err != nil {
		return "", err.Error()
	}
	return rel, ""
}

// ---- what a pass would do, without doing it ----

// DrivePreview is a pass worked out from Drive's listing alone: nothing downloaded, nothing
// written. It is the answer to "what will this bring in?" while somebody is still looking at
// the dialog, and it comes from the same walk and the same rules as a pass, so it cannot
// promise a file the pass then refuses.
type DrivePreview struct {
	FolderName string     `json:"folder_name"`
	FolderID   string     `json:"folder_id"`
	Tree       *DriveNode `json:"tree"`
	Files      int        `json:"files"`   // would come into Documents
	Skipped    int        `json:"skipped"` // Drive has them; the index cannot take them
	Folders    int        `json:"folders"` // subfolders the pass would walk
	Bytes      int64      `json:"bytes"`   // of the files coming in, where Drive knows a size
	// More is how many entries the tree leaves out: the counts above cover them, the tree
	// stops so the response stays a screen's worth rather than a checkout's.
	More int `json:"more"`
	// Capped says the walk stopped at the limits a pass has, so even the counts are of the
	// first slice of the folder rather than all of it.
	Capped bool   `json:"capped"`
	Took   string `json:"took"`
}

// DriveNode is one entry in the preview tree: a folder with what is under it, or a file with
// the name it would have as a document — or the reason it would not become one.
type DriveNode struct {
	Name     string       `json:"name"`
	Folder   bool         `json:"folder,omitempty"`
	Doc      string       `json:"doc,omitempty"`  // the document's name, once it is one
	Skip     string       `json:"skip,omitempty"` // why it would not come in
	Size     int64        `json:"size,omitempty"`
	Children []*DriveNode `json:"children,omitempty"`
}

// A tree bigger than this is counted but not drawn.
const maxDrivePreviewNodes = 2000

// preview walks a folder the way a pass would and says what the pass would do. The folder is
// checked the way adding a sync checks it, so a link to a file, or to a folder the service
// account was never shared with, is turned away with the reason.
func (d *driveAPI) preview(ctx context.Context, folderID, dest string, recurse bool) (*DrivePreview, error) {
	start := time.Now()
	root, err := d.meta(ctx, folderID)
	if err != nil {
		return nil, driveShareHint(err)
	}
	if root.MimeType != driveFolderMime {
		return nil, fmt.Errorf("%s is a file, not a folder", root.Name)
	}
	if root.Trashed {
		return nil, fmt.Errorf("%s is in the Drive trash", root.Name)
	}
	pv := &DrivePreview{FolderName: root.Name, FolderID: root.ID, Tree: &DriveNode{Name: root.Name, Folder: true}}
	// Folders by Drive id, so a child lands under the folder it was listed in. Keyed on the id
	// rather than the path: two Drive folders can share a name, and a pass merges them, but
	// the preview should show the folder as Drive has it.
	byID := map[string]*DriveNode{folderID: pv.Tree}
	shown := 0
	add := func(parent string, n *DriveNode) bool {
		p := byID[parent]
		if p == nil || shown >= maxDrivePreviewNodes {
			pv.More++
			return false
		}
		shown++
		p.Children = append(p.Children, n)
		return true
	}
	capped, err := d.walk(ctx, folderID, dest, recurse, func(in driveDir, f driveFile) error {
		if f.MimeType == driveFolderMime {
			n := &DriveNode{Name: f.Name, Folder: true}
			if recurse {
				pv.Folders++
			} else {
				n.Skip = "subfolders are off"
			}
			// A folder that did not make it into the tree must not collect children either:
			// they would be drawn nowhere and still counted as shown.
			if add(in.ID, n) && recurse {
				byID[f.ID] = n
			}
			return nil
		}
		n := &DriveNode{Name: f.Name, Size: driveSize(f)}
		if rel, why := planDriveFile(in.Rel, f); rel == "" {
			n.Skip = why
			pv.Skipped++
		} else {
			n.Doc = path.Base(rel)
			pv.Files++
			pv.Bytes += n.Size
		}
		add(in.ID, n)
		return nil
	})
	if err != nil {
		return nil, err
	}
	pv.Capped = capped
	sortDriveTree(pv.Tree)
	pv.Took = time.Since(start).Round(time.Millisecond).String()
	return pv, nil
}

// sortDriveTree puts folders first and then names in order, the way Drive itself shows a
// folder, so the preview reads like the thing it previews.
func sortDriveTree(n *DriveNode) {
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if a.Folder != b.Folder {
			return a.Folder
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	for _, c := range n.Children {
		if c.Folder {
			sortDriveTree(c)
		}
	}
}

// ---- one pass ----

// syncDrive brings one folder into Documents and takes back out whatever Drive no longer has.
// It returns a report even when it fails part-way: work already done is still done, and saying
// so is more use than a bare error.
func (b *Bot) syncDrive(ctx context.Context, orgID int64, s *DriveSync) (DriveReport, error) {
	start := time.Now()
	rep := DriveReport{FolderName: s.FolderName}
	defer func() { rep.Took = time.Since(start).Round(time.Millisecond).String() }()

	conn, err := b.store.Connection(ctx, orgID, s.ConnectionID)
	if err != nil {
		return rep, fmt.Errorf("credential: %w", err)
	}
	api, err := newDriveAPI(b.proxy, conn, orgID)
	if err != nil {
		return rep, err
	}
	// Confirm the folder is still there and still a folder before touching any documents: a
	// folder that has been deleted in Drive must not read as "every file is gone, mirror it".
	root, err := api.meta(ctx, s.FolderID)
	if err != nil {
		return rep, err
	}
	if root.MimeType != driveFolderMime {
		return rep, fmt.Errorf("%s is a file, not a folder", root.Name)
	}
	if root.Trashed {
		return rep, fmt.Errorf("%s is in the Drive trash", root.Name)
	}
	rep.FolderName = root.Name

	known, err := b.store.DriveSyncFiles(ctx, orgID, s.ID)
	if err != nil {
		return rep, err
	}
	docs := b.docs.For(orgID)
	seen := map[string]bool{}

	// The walk is the one the preview uses, so what "Test" showed in the dialog is what this
	// takes. A pass takes what the walk gives it and does not report hitting the caps; the
	// preview is where somebody learns a folder is over them.
	_, err = api.walk(ctx, s.FolderID, s.Dest, s.Recurse, func(in driveDir, f driveFile) error {
		if f.MimeType == driveFolderMime {
			return nil
		}
		rel, why := planDriveFile(in.Rel, f)
		if rel == "" {
			rep.Skipped = append(rep.Skipped, f.Name+" — "+why)
			return nil
		}
		seen[f.ID] = true
		prev, had := known[f.ID]
		// Unchanged and still where we put it: nothing to download.
		if had && prev.Version == f.version() && prev.Path == rel {
			rep.Unchanged++
			b.store.TouchDriveSyncFile(ctx, orgID, s.ID, f.ID)
			return nil
		}
		// Renamed in Drive, or moved between subfolders: the old document is this same file
		// under its old name, so it goes rather than being left as a duplicate.
		if had && prev.Path != rel {
			docs.Delete(ctx, prev.Path)
		}
		if err := b.copyDriveFile(ctx, api, docs, orgID, s, f, rel); err != nil {
			rep.Skipped = append(rep.Skipped, f.Name+" — "+err.Error())
			return nil
		}
		if had {
			rep.Updated++
		} else {
			rep.Added++
		}
		return nil
	})
	if err != nil {
		return rep, err
	}

	// The mirror: anything this sync owned that Drive did not hand back is gone from Drive, so
	// it goes from Documents too. Only rows this sync wrote are considered, so an upload that
	// happens to sit in the same folder is never touched.
	for id, f := range known {
		if seen[id] {
			continue
		}
		if err := docs.Delete(ctx, f.Path); err != nil && !isNotExist(err) {
			slog.Warn("drive sync delete", "org", orgID, "path", f.Path, "err", err)
			continue
		}
		b.store.ForgetDriveSyncFile(ctx, orgID, s.ID, id)
		rep.Removed++
	}

	if rep.Added+rep.Updated+rep.Removed > 0 {
		go b.reindex(context.WithoutCancel(ctx), orgID)
	}
	return rep, nil
}

// copyDriveFile takes one file's bytes into the document store and records that it did.
func (b *Bot) copyDriveFile(ctx context.Context, api *driveAPI, docs DocStore, orgID int64, s *DriveSync, f driveFile, rel string) error {
	body, err := api.open(ctx, f)
	if err != nil {
		return err
	}
	defer body.Close()
	// Read through the cap rather than trusting the size Drive reported: a native export has
	// no size until it is made, so this is the only place a runaway export is caught.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(body, maxDriveFileBytes+1)); err != nil {
		return err
	}
	if buf.Len() > maxDriveFileBytes {
		return fmt.Errorf("over the %s limit", humanBytes(maxDriveFileBytes))
	}
	by := "drive-sync"
	if s.CreatedBy != "" {
		by = s.CreatedBy
	}
	if err := docs.Put(ctx, rel, &buf, by, s.Scope); err != nil {
		return err
	}
	for _, folder := range parentFolders(rel) {
		b.store.AddDocumentFolder(ctx, orgID, folder)
	}
	return b.store.MarkDriveSyncFile(ctx, orgID, s.ID, driveSyncFile{FileID: f.ID, Path: rel, Version: f.version()})
}

// isNotExist covers both document stores: a missing file locally, and a missing object in the
// bucket. A mirror deleting something that is already gone is success, not a fault.
func isNotExist(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, storage.ErrObjectNotExist)
}

func driveSize(f driveFile) int64 {
	var n int64
	fmt.Sscanf(f.Size, "%d", &n)
	return n
}

// runDriveSync is one pass with its bookkeeping: the console sees "running", then the result.
func (b *Bot) runDriveSync(ctx context.Context, orgID int64, s *DriveSync) (DriveReport, error) {
	b.store.StartDriveSyncRun(ctx, orgID, s.ID)
	rep, err := b.syncDrive(ctx, orgID, s)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	b.store.FinishDriveSyncRun(ctx, orgID, s.ID, rep, msg)
	if err != nil {
		slog.Warn("drive sync", "org", orgID, "sync", s.ID, "folder", s.FolderName, "err", err)
	} else {
		slog.Info("drive sync", "org", orgID, "sync", s.ID, "folder", rep.FolderName,
			"added", rep.Added, "updated", rep.Updated, "removed", rep.Removed, "unchanged", rep.Unchanged, "took", rep.Took)
	}
	return rep, err
}

// RunDriveSyncs is the scheduled half: every enabled sync, every six hours, for every
// organisation. It runs one at a time on purpose — a pass is mostly waiting on Drive and then
// on the embedding endpoint, and doing thirty at once would put a deployment's whole
// re-indexing budget into one minute.
func (b *Bot) RunDriveSyncs(ctx context.Context) {
	// Not at boot: a container that restarts often would sync on every restart, and the
	// documents were already there a moment ago. The first pass is one interval in.
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(driveSyncEvery):
		}
		byOrg, err := b.store.EnabledDriveSyncs(ctx)
		if err != nil {
			slog.Warn("drive syncs", "err", err)
			continue
		}
		for orgID, syncs := range byOrg {
			for _, s := range syncs {
				if ctx.Err() != nil {
					return
				}
				b.runDriveSync(ctx, orgID, s)
			}
		}
	}
}
