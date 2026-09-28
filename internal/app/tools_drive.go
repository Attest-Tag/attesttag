package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Finding a file in Google Drive, whichever Drive it is in.
//
// A channel can reach two kinds of Drive: the company's, through a service account the folders
// were shared with, and the asker's own, through their Google Workspace sign-in. One request goes
// out under one credential, so any rule for choosing between them is wrong for some question.
// Chosen by rank, "find my Q3 doc" was answered out of the service account's folder and called
// the asker's own. Chosen for the asker, a file sitting in the company's folder needed them to
// connect their own account first — the bot told somebody it could not read a file everyone could
// — and once they had connected, their own Drive was the only place looked, so a file only the
// company's folder held came back as not there at all.
//
// So the question is put to every Drive the asker can use, each under its own credential, and the
// answer says whose each file is: the rule github_search keeps for repositories, for the same
// reason. Nobody is asked to connect anything to reach a company file.
//
// Somebody's own files are theirs, though, and a channel is not a private place. Found by a search
// that was not about their own files, they go to the asker in a direct message and the channel
// hears only that there were some. Asked for — "in my Drive" — or asked for in a DM, they are
// listed like any other.

const (
	driveFromCompany = "company"
	driveFromMine    = "mine"
	// driveFilesPath is what decides whether a connection carries Drive at all: a search is a GET
	// on it, and every read is under it.
	driveFilesPath = "/drive/v3/files"
	// driveSearchMax caps one search, per kind of Drive. A search is for finding the file, and past
	// a couple of dozen the answer is better words rather than a longer list.
	driveSearchMax = 25
	// driveOwnListed is how many of somebody's own files the direct message names before it says
	// how many more there were.
	driveOwnListed = 10
	// driveMatchMax bounds how many matching lines drive_read hands back for a match. Enough rows
	// for any lookup; a match that finds more wanted a narrower match.
	driveMatchMax = 200
)

// drivesOf is every Drive this channel reaches, the company's and the asker's own kind apart, each
// connection once and narrowest grant first. Once by name as well: a search names the connection
// it goes out on, and of two connections sharing a name only the first can ever be named.
func drivesOf(acc *Access) (company, own []*Connection) {
	if acc == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, r := range acc.Rules {
		conn := r.Conn
		if conn == nil || !carriesDrive(conn) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(conn.Name))
		if seen[key] {
			continue
		}
		seen[key] = true
		if isPersonal(conn) {
			own = append(own, conn)
		} else {
			company = append(company, conn)
		}
	}
	return company, own
}

// carriesDrive says whether drive_search can ask a connection: it reaches www.googleapis.com with
// a GET under /drive/v3/files, and it is meant for Drive. A shared credential claiming the whole
// host counts only when its preset says it is a Drive credential, because a key kept for some other
// Google API would answer every search with a 403. A personal Google connection that narrows
// nothing carries every part, Drive among them.
func carriesDrive(conn *Connection) bool {
	if conn.CredType == "mcp" || !slices.ContainsFunc(conn.AllowedHosts, func(h string) bool { return hostMatch(h, driveHost) }) {
		return false
	}
	if len(conn.Methods) > 0 && !slices.ContainsFunc(conn.Methods, func(m string) bool { return strings.EqualFold(m, "GET") }) {
		return false
	}
	if len(conn.PathPrefixes) == 0 {
		return isPersonal(conn) || presetServes(conn, driveFilesPath)
	}
	for _, p := range conn.PathPrefixes {
		if strings.HasPrefix(driveFilesPath, p) {
			return true
		}
	}
	return false
}

// driveStopWords are words people say about a file rather than words in it. Every search word has
// to match, so without this "find the pricing doc" misses a Sheet called Pricing 2026 for want of
// the word "doc" anywhere in it.
var driveStopWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "for": true, "in": true, "on": true, "to": true,
	"and": true, "or": true, "my": true, "our": true, "your": true, "their": true, "from": true,
	"file": true, "files": true, "doc": true, "docs": true, "document": true, "documents": true,
	"sheet": true, "spreadsheet": true, "folder": true, "drive": true, "google": true,
}

// driveQuery is search words in Drive's own query language: every word in the title or the text,
// and never a file in the bin. Built here rather than written by the model, because a q Drive
// cannot parse is a 400 that costs a round to learn from, and one that leaves out trashed = false
// answers out of the bin. Empty when there is nothing to look for.
func driveQuery(words string) string {
	var kept, all []string
	for _, w := range strings.Fields(words) {
		w = strings.Trim(w, `"'“”‘’.,;:!?()[]{}`)
		if w == "" {
			continue
		}
		all = append(all, w)
		if !driveStopWords[strings.ToLower(w)] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		kept = all
	}
	if len(kept) == 0 {
		return ""
	}
	if len(kept) > 8 {
		kept = kept[:8]
	}
	esc := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	parts := []string{"trashed = false"}
	for _, w := range kept {
		e := esc.Replace(w)
		parts = append(parts, fmt.Sprintf("(name contains '%s' or fullText contains '%s')", e, e))
	}
	return strings.Join(parts, " and ")
}

// driveFileMeta is the part of Drive's file resource these tools show.
type driveFileMeta struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Modified string `json:"modifiedTime"`
	Link     string `json:"webViewLink"`
}

const driveMetaFields = "id,name,mimeType,modifiedTime,webViewLink"

// driveHit is one file a search found, and which of the Drives found it.
type driveHit struct {
	driveFileMeta
	company *Connection // the company Drive that found it; nil when only their own did
	own     bool        // their own Drive found it too
}

// driveGet is one read on Drive under a named connection. Always named: which credential answers
// decides what the search can see, so it is never left to the proxy's choice between them.
func (a *Agent) driveGet(ctx context.Context, c *Call, conn *Connection, path string, q url.Values, maxBytes int) (*ProxyResponse, error) {
	u := "https://" + driveHost + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return a.proxy.Do(ctx, c.OrgID, c.Access, ProxyRequest{Method: "GET", URL: u, Connection: conn.Name, MaxBytes: maxBytes}, callAudit(c), true)
}

// errDriveScope is a Drive that answered 403 for want of a scope: a person who signed in without
// ticking Drive, or a service account made for some other API.
var errDriveScope = errors.New("the sign-in does not include Drive")

// driveRefusal turns a Drive error status into something a model can act on.
func driveRefusal(resp *ProxyResponse) error {
	if resp.Status == 403 && scopeRefusal(resp.Body) {
		return errDriveScope
	}
	msg := driveErrorMessage([]byte(resp.Body))
	if msg == "" {
		msg = truncate(oneLine(resp.Body), 200)
	}
	return fmt.Errorf("Drive answered %d: %s", resp.Status, msg)
}

// driveList runs one search on one Drive.
func (a *Agent) driveList(ctx context.Context, c *Call, conn *Connection, q string, limit int) ([]driveFileMeta, bool, error) {
	v := url.Values{
		"q":        {q},
		"fields":   {"files(" + driveMetaFields + "),incompleteSearch"},
		"pageSize": {strconv.Itoa(limit)},
		// Shared drives are where a company's files usually are, and Drive leaves them out of a
		// search unless it is asked for them in all three of these.
		"corpora":                   {"allDrives"},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
	}
	resp, err := a.driveGet(ctx, c, conn, driveFilesPath, v, 0)
	if err != nil {
		return nil, false, err
	}
	if resp.Status >= 400 {
		return nil, false, driveRefusal(resp)
	}
	var out struct {
		Files      []driveFileMeta `json:"files"`
		Incomplete bool            `json:"incompleteSearch"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		return nil, false, errors.New("Drive answered with something that is not a list of files")
	}
	return out.Files, out.Incomplete, nil
}

// ownDrive is the asker's own Drive, when there is one in reach, and where they stand with it.
type ownDrive struct {
	conn      *Connection
	connected bool
	account   string
}

// ownDriveOf picks the asker's own Drive out of the channel's. Only a person has one: a turn with
// nobody behind it gets none, which is the proxy's answer too.
func (a *Agent) ownDriveOf(ctx context.Context, c *Call, own []*Connection) *ownDrive {
	if len(own) == 0 || c.UserID == "" || a.store == nil {
		return nil
	}
	// The first one they have connected, else the first one there is — which is the one they
	// would be asked to connect.
	for _, conn := range own {
		if uc, err := a.store.UserConnection(ctx, c.OrgID, conn.ID, c.TeamID, c.UserID); err == nil && uc.Connected() {
			return &ownDrive{conn: conn, connected: true, account: uc.Account}
		}
	}
	return &ownDrive{conn: own[0]}
}

// needsOwnDrive is what a Drive tool says when answering needs the asker's own account and they
// have not connected it: the Connect card where there is somebody to send it to, and the reason
// there is none everywhere else. The same three lanes proxied() keeps for an http_request.
func (a *Agent) needsOwnDrive(ctx context.Context, c *Call, conn *Connection) string {
	switch {
	case c.Preview:
		return conn.Name + " is connected per person and whoever this preview is running as has not connected theirs. " +
			"Say so plainly; in the channel they would be sent a Connect link. Do not retry."
	case c.Silent:
		return fmt.Sprintf("%s runs on each person's own account and <@%s> has not connected theirs. A quiet run cannot send them "+
			"the link. Report what you found, and say that this step needs them to connect their own account first. Do not retry.", conn.Name, c.UserID)
	}
	return a.askToConnect(ctx, c, conn)
}

// label is how a file in somebody's own Drive is attributed in a tool result. The model writes to
// the asker, so "yours" would do in a reply and is ambiguous here: say whose.
func (od *ownDrive) label(c *Call) string {
	s := fmt.Sprintf("<@%s>'s own Drive", c.UserID)
	if od.account != "" {
		s += " (" + od.account + ")"
	}
	return s
}

func (a *Agent) driveSearch(ctx context.Context, c *Call, query, from string, limit int) (string, error) {
	q := driveQuery(query)
	if q == "" {
		return "", errors.New("say what to look for: words from the file's title or its text")
	}
	if limit <= 0 || limit > driveSearchMax {
		limit = min(max(limit, 10), driveSearchMax)
	}
	company, own := drivesOf(c.Access)
	switch strings.ToLower(strings.TrimSpace(from)) {
	case driveFromCompany:
		from, own = driveFromCompany, nil
		if len(company) == 0 {
			return "", errors.New("no company Drive is connected in this channel; leave from out to search the asker's own")
		}
	case driveFromMine:
		from, company = driveFromMine, nil
		if len(own) == 0 {
			return "", errors.New("nothing in this channel reaches the asker's own Drive; leave from out to search the company's")
		}
	default:
		from = ""
	}
	if len(company)+len(own) == 0 {
		return "", errors.New("no Google Drive is connected in this channel")
	}

	var hits []*driveHit
	byID := map[string]*driveHit{}
	var notes []string
	incomplete := false
	for _, conn := range company {
		files, partial, err := a.driveList(ctx, c, conn, q, limit)
		if err != nil {
			if errors.Is(err, errDriveScope) {
				err = errors.New("its credential does not hold the Drive scope")
			}
			notes = append(notes, fmt.Sprintf("The company Drive %q could not be searched: %s.", conn.Name, err))
			continue
		}
		incomplete = incomplete || partial
		for _, f := range files {
			if _, dup := byID[f.ID]; dup {
				continue
			}
			h := &driveHit{driveFileMeta: f, company: conn}
			byID[f.ID] = h
			hits = append(hits, h)
		}
	}

	od := a.ownDriveOf(ctx, c, own)
	var ownOnly []*driveHit
	switch {
	case od == nil:
	case !od.connected && (from == driveFromMine || len(company) == 0):
		// Their own Drive is the only one there is to look in, so there is no answer without it.
		return a.needsOwnDrive(ctx, c, od.conn), nil
	case !od.connected:
		notes = append(notes, fmt.Sprintf("Only the company's Drive was searched: <@%s> has not connected %s, so their own was not. "+
			"If they meant a file of their own, call connect_account.", c.UserID, od.conn.Name))
	default:
		// Before the request, not after it: the call is theirs from the moment it spends their
		// account, and a note that is only ever wrong towards privacy costs a log line its detail.
		spendNoteFrom(ctx).notePersonal(od.conn.Name)
		files, partial, err := a.driveList(ctx, c, od.conn, q, limit)
		switch {
		case errors.Is(err, ErrNeedsUserAuth):
			if from == driveFromMine {
				return a.needsOwnDrive(ctx, c, od.conn), nil
			}
			notes = append(notes, fmt.Sprintf("Their own Drive was not searched: <@%s> has not connected %s.", c.UserID, od.conn.Name))
		case errors.Is(err, errDriveScope):
			notes = append(notes, fmt.Sprintf("Their own Drive was not searched: the sign-in <@%s> made for %s does not include Drive. "+
				"If they meant a file of their own, call connect_account and tell them to tick Drive on the page it opens.", c.UserID, od.conn.Name))
		case err != nil:
			notes = append(notes, fmt.Sprintf("Their own Drive could not be searched: %s.", err))
		default:
			incomplete = incomplete || partial
			for _, f := range files {
				if h, ok := byID[f.ID]; ok {
					h.own = true // the company's copy is the one to show: it is not private
					continue
				}
				h := &driveHit{driveFileMeta: f, own: true}
				byID[f.ID] = h
				ownOnly = append(ownOnly, h)
			}
		}
	}

	// Their own files, found by a search that was not about them, in a room others read: to them,
	// privately, and the room hears only how many.
	shown := ownOnly
	if len(ownOnly) > 0 && from != driveFromMine && !isDirectConversation(c.Channel) {
		shown = nil
		if a.sendOwnHits(ctx, c, od, ownOnly) {
			notes = append(notes, fmt.Sprintf("%d more in <@%s>'s own Drive went to them in a direct message and are not shown here, "+
				"because this conversation is not private. Do not guess at them. If one of those is what they want, they can say "+
				"\"in my Drive\" or ask in a DM.", len(ownOnly), c.UserID))
		} else {
			notes = append(notes, fmt.Sprintf("%d more in <@%s>'s own Drive are not shown, because this conversation is not private. "+
				"They can ask in a DM, or say \"in my Drive\" to have them listed here.", len(ownOnly), c.UserID))
		}
	}

	var b strings.Builder
	total := len(hits) + len(shown)
	where := driveWhere(company, od, from)
	if total == 0 {
		fmt.Fprintf(&b, "Nothing in %s matched every word of %q. Try fewer or different words — a distinctive word from the title works best.", where, query)
	} else {
		fmt.Fprintf(&b, "%d found in %s for %q, most relevant first:\n", total, where, query)
		n := 0
		line := func(h *driveHit, whose, readFrom string) {
			n++
			fmt.Fprintf(&b, "%d. %s — %s · %s", n, h.Name, whose, driveKind(h.MimeType))
			if d := driveDate(h.Modified); d != "" {
				b.WriteString(" · modified " + d)
			}
			if h.Link != "" {
				b.WriteString(" · " + h.Link)
			}
			fmt.Fprintf(&b, "\n   drive_read file_id=%s from=%s\n", h.ID, readFrom)
		}
		for _, h := range hits {
			whose := fmt.Sprintf("company Drive (%s)", h.company.Name)
			if h.own {
				whose += ", which they can open too"
			}
			line(h, whose, driveFromCompany)
		}
		for _, h := range shown {
			line(h, od.label(c), driveFromMine)
		}
	}
	if incomplete {
		notes = append(notes, "Drive says it did not search everything it holds; more specific words find what this missed.")
	}
	for _, n := range notes {
		b.WriteString("\n[" + n + "]")
	}
	return b.String(), nil
}

// driveWhere names the Drives a search asked, for the line that opens its answer.
func driveWhere(company []*Connection, od *ownDrive, from string) string {
	var parts []string
	if len(company) > 0 {
		names := make([]string, len(company))
		for i, conn := range company {
			names[i] = conn.Name
		}
		parts = append(parts, "the company Drive ("+strings.Join(names, ", ")+")")
	}
	if od != nil && od.connected {
		parts = append(parts, "their own Drive")
	}
	if len(parts) == 0 && from == driveFromMine {
		return "their own Drive"
	}
	return strings.Join(parts, " and ")
}

// sendOwnHits puts the files found in somebody's own Drive in front of them alone.
func (a *Agent) sendOwnHits(ctx context.Context, c *Call, od *ownDrive, hits []*driveHit) bool {
	if c.offline() || c.SL == nil || c.UserID == "" || od == nil {
		return false
	}
	var b strings.Builder
	where := "a channel"
	if c.Channel != "" {
		where = "<#" + c.Channel + ">"
	}
	fmt.Fprintf(&b, "*From your own Drive*\nWhile answering in %s I also found these in your own Drive. They are yours, so they are here and not in the channel:\n", where)
	for i, h := range hits {
		if i == driveOwnListed {
			fmt.Fprintf(&b, "…and %d more.\n", len(hits)-driveOwnListed)
			break
		}
		name := escapeMrkdwn(h.Name)
		if h.Link != "" {
			name = "<" + h.Link + "|" + name + ">"
		}
		fmt.Fprintf(&b, "• %s — %s", name, driveKind(h.MimeType))
		if d := driveDate(h.Modified); d != "" {
			b.WriteString(", modified " + d)
		}
		b.WriteString("\n")
	}
	b.WriteString("To use one, ask me here, or say \"in my Drive\" in the channel.")
	card := Card{Parts: []CardPart{{Markdown: b.String()}}}
	_, _, err := c.SL.PostCard(ctx, c.UserID, "", card, "Files from your own Drive")
	return err == nil
}

// driveKind names a file's type the way a person would.
func driveKind(mime string) string {
	switch mime {
	case "application/vnd.google-apps.document":
		return "Google Doc"
	case "application/vnd.google-apps.spreadsheet":
		return "Google Sheet"
	case "application/vnd.google-apps.presentation":
		return "Google Slides"
	case driveFolderMime:
		return "folder"
	case "application/pdf":
		return "PDF"
	case "text/csv":
		return "CSV"
	case "text/plain":
		return "text file"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/msword":
		return "Word document"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/vnd.ms-excel":
		return "Excel workbook"
	}
	if strings.HasPrefix(mime, "image/") {
		return "image"
	}
	if mime == "" {
		return "file"
	}
	return mime
}

// driveDate is the day out of Drive's RFC 3339 timestamp.
func driveDate(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// driveFileURL matches the id in a link to one file: docs.google.com/document/d/<id>/edit,
// drive.google.com/file/d/<id>/view and the like. The folder, ?id= and bare shapes are
// drive_sync's (driveFolderURL, driveOpenURL, driveBareID).
var driveFileURL = regexp.MustCompile(`/d/([A-Za-z0-9_-]{10,})`)

// driveFileID pulls a file's id out of a link somebody pasted, or takes the id as it is. Nothing
// else gets through: the id goes into a URL path, so it is checked here as well as by the proxy.
func driveFileID(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, re := range []*regexp.Regexp{driveFileURL, driveFolderURL, driveOpenURL} {
		if m := re.FindStringSubmatch(s); m != nil {
			return m[1], nil
		}
	}
	if driveBareID.MatchString(s) {
		return s, nil
	}
	return "", errors.New("file_id is not a Drive file id or link: pass the id drive_search listed, or the file's link")
}

// driveTextual says whether an uploaded file's bytes are text a model can read as they are.
func driveTextual(mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/csv", "application/x-yaml", "application/yaml",
		"application/x-ndjson", "application/sql", "application/javascript":
		return true
	}
	return false
}

func (a *Agent) driveRead(ctx context.Context, c *Call, fileID, from, match string) (string, error) {
	id, err := driveFileID(fileID)
	if err != nil {
		return "", err
	}
	company, own := drivesOf(c.Access)
	from = strings.ToLower(strings.TrimSpace(from))
	switch from {
	case driveFromCompany:
		own = nil
	case driveFromMine:
		company = nil
	default:
		from = ""
	}
	if len(company)+len(own) == 0 {
		if from != "" {
			return "", fmt.Errorf("no Drive of that kind (%s) is connected in this channel", from)
		}
		return "", errors.New("no Google Drive is connected in this channel")
	}

	// The company's first: a file it can open is not private, whoever else can open it too. Their
	// own Drive is asked only for a file the company's cannot open.
	var notes []string
	for _, conn := range company {
		f, err := a.driveMeta(ctx, c, conn, id)
		if err != nil {
			if !errors.Is(err, errDriveNotFound) {
				notes = append(notes, fmt.Sprintf("The company Drive %q could not open it: %s.", conn.Name, err))
			}
			continue
		}
		return a.driveContent(ctx, c, conn, f, fmt.Sprintf("company Drive (%s)", conn.Name), match)
	}
	if od := a.ownDriveOf(ctx, c, own); od != nil {
		if !od.connected {
			if from == driveFromMine || len(company) == 0 {
				return a.needsOwnDrive(ctx, c, od.conn), nil
			}
			notes = append(notes, fmt.Sprintf("Their own Drive was not tried: <@%s> has not connected %s. If it is their own file, call connect_account.", c.UserID, od.conn.Name))
		} else {
			spendNoteFrom(ctx).notePersonal(od.conn.Name)
			f, err := a.driveMeta(ctx, c, od.conn, id)
			switch {
			case err == nil:
				return a.driveContent(ctx, c, od.conn, f, od.label(c), match)
			case errors.Is(err, ErrNeedsUserAuth):
				return a.needsOwnDrive(ctx, c, od.conn), nil
			case errors.Is(err, errDriveScope):
				notes = append(notes, fmt.Sprintf("Their own Drive was not tried: the sign-in <@%s> made for %s does not include Drive; connect_account lets them add it.", c.UserID, od.conn.Name))
			case !errors.Is(err, errDriveNotFound):
				notes = append(notes, fmt.Sprintf("Their own Drive could not open it: %s.", err))
			}
		}
	}
	msg := "No Drive in this channel can open that file: it may have been deleted, or it is shared with nobody this channel reaches."
	if len(notes) > 0 {
		msg += " " + strings.Join(notes, " ")
	}
	return "", errors.New(msg)
}

var errDriveNotFound = errors.New("not found")

// driveMeta reads one file's metadata on one Drive. Not found is its own error, because the next
// Drive may well have it.
func (a *Agent) driveMeta(ctx context.Context, c *Call, conn *Connection, id string) (driveFileMeta, error) {
	resp, err := a.driveGet(ctx, c, conn, driveFilesPath+"/"+id, url.Values{"fields": {driveMetaFields}, "supportsAllDrives": {"true"}}, 0)
	if err != nil {
		return driveFileMeta{}, err
	}
	if resp.Status == 404 {
		return driveFileMeta{}, errDriveNotFound
	}
	if resp.Status >= 400 {
		return driveFileMeta{}, driveRefusal(resp)
	}
	var f driveFileMeta
	if err := json.Unmarshal([]byte(resp.Body), &f); err != nil || f.ID == "" {
		return driveFileMeta{}, errors.New("Drive answered with something that is not a file")
	}
	return f, nil
}

// driveContent is one file as text, under a line saying what and whose it is.
func (a *Agent) driveContent(ctx context.Context, c *Call, conn *Connection, f driveFileMeta, whose, match string) (string, error) {
	var head strings.Builder
	fmt.Fprintf(&head, "%s — %s · %s", f.Name, whose, driveKind(f.MimeType))
	if d := driveDate(f.Modified); d != "" {
		head.WriteString(" · modified " + d)
	}
	if f.Link != "" {
		head.WriteString(" · " + f.Link)
	}
	head.WriteString("\n")

	path, q, csv := driveFilesPath+"/"+f.ID, url.Values{}, false
	switch mime := f.MimeType; {
	case mime == driveFolderMime:
		return a.driveFolder(ctx, c, conn, f, head.String())
	case mime == "application/vnd.google-apps.spreadsheet":
		path += "/export"
		q.Set("mimeType", "text/csv")
		csv = true
		head.WriteString("[A Google Sheet comes out as its first tab only, as CSV.]\n")
	case strings.HasPrefix(mime, "application/vnd.google-apps."):
		exp, ok := driveExport[mime]
		if !ok {
			return head.String() + "\nThis kind of Google file has no text export. Open it with the link above.", nil
		}
		path += "/export"
		q.Set("mimeType", exp.mime)
		if mime == "application/vnd.google-apps.presentation" {
			q.Set("mimeType", "text/plain") // drive_sync keeps the PDF for the index; a reader wants the words
		}
	case driveTextual(mime):
		q.Set("alt", "media")
		q.Set("supportsAllDrives", "true")
		csv = mime == "text/csv" || mime == "application/csv"
	default:
		return head.String() + "\nThis is a " + driveKind(mime) + ", which cannot be read as text here. Open it with the link above; " +
			"if it is in a folder synced into Documents, search_docs reads it.", nil
	}

	// A lookup reads the whole file and hands back only the lines that match, so a row in a long
	// sheet is found rather than cut off with everything after the first screenful.
	maxBytes := modelMaxBody
	if strings.TrimSpace(match) != "" {
		maxBytes = proxyMaxRead
	}
	resp, err := a.driveGet(ctx, c, conn, path, q, maxBytes)
	if err != nil {
		return "", err
	}
	if resp.Status >= 400 {
		return "", driveRefusal(resp)
	}
	body := resp.Body
	if resp.Truncated {
		if i := strings.LastIndex(body, "\n\n[truncated:"); i >= 0 {
			body = body[:i]
		}
	}
	if m := strings.TrimSpace(match); m != "" {
		lines, n, more := matchingLines(body, m, csv)
		switch {
		case n == 0:
			return head.String() + fmt.Sprintf("\nNo line of it contains %q.", m), nil
		case more:
			return head.String() + fmt.Sprintf("\nThe first %d lines containing %q:\n%s\n\n[more lines match; a narrower match finds the one you want]", driveMatchMax, m, lines), nil
		}
		return head.String() + fmt.Sprintf("\n%d line(s) containing %q:\n%s", n, m, lines), nil
	}
	out := head.String() + "\n" + body
	if resp.Truncated {
		out += fmt.Sprintf("\n\n[cut at %d KB: the file is longer. To find something in it, call drive_read again with match set to a word from the line you want.]", modelMaxBody>>10)
	}
	return out, nil
}

// matchingLines keeps the lines of text that contain match, ignoring case — and a CSV's header,
// so the columns still have names. It reports how many matched and whether it stopped short.
func matchingLines(text, match string, header bool) (string, int, bool) {
	m := strings.ToLower(match)
	var out []string
	n := 0
	for i, l := range strings.Split(text, "\n") {
		if i == 0 && header {
			out = append(out, l)
			continue
		}
		if !strings.Contains(strings.ToLower(l), m) {
			continue
		}
		if n == driveMatchMax {
			return strings.Join(out, "\n"), n, true
		}
		out = append(out, l)
		n++
	}
	return strings.Join(out, "\n"), n, false
}

// driveFolder lists what is in a folder, which is what reading one means.
func (a *Agent) driveFolder(ctx context.Context, c *Call, conn *Connection, f driveFileMeta, head string) (string, error) {
	v := url.Values{
		"q":                         {fmt.Sprintf("'%s' in parents and trashed = false", f.ID)},
		"fields":                    {"files(" + driveMetaFields + ")"},
		"pageSize":                  {"100"},
		"orderBy":                   {"folder,name"},
		"corpora":                   {"allDrives"},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
	}
	resp, err := a.driveGet(ctx, c, conn, driveFilesPath, v, 0)
	if err != nil {
		return "", err
	}
	if resp.Status >= 400 {
		return "", driveRefusal(resp)
	}
	var out struct {
		Files []driveFileMeta `json:"files"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		return "", errors.New("Drive answered with something that is not a list of files")
	}
	var b strings.Builder
	b.WriteString(head)
	if len(out.Files) == 0 {
		b.WriteString("\nThe folder is empty, or holds nothing this credential can see.")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "\n%d item(s):\n", len(out.Files))
	for _, it := range out.Files {
		fmt.Fprintf(&b, "- %s · %s", it.Name, driveKind(it.MimeType))
		if d := driveDate(it.Modified); d != "" {
			b.WriteString(" · modified " + d)
		}
		fmt.Fprintf(&b, " · file_id=%s\n", it.ID)
	}
	return b.String(), nil
}

// driveTools are the two tools, offered wherever the channel reaches a Drive of either kind.
func (a *Agent) driveTools() []Tool {
	return []Tool{
		{
			Name: "drive_search",
			Desc: "Find files in Google Drive — the company's Drive and, once the asker has connected it, their own — and say whose each one is. " +
				"Use it for any 'find the … file/doc/sheet' ask, before http_request. Every word must appear in the file's title or text, " +
				"so pass the distinctive words ('pricing 2026', 'onboarding checklist'), not 'the doc about'. A file in the company's Drive " +
				"needs nobody to connect anything.",
			Params: schema(map[string]any{
				"query": str("Distinctive words from the file's title or its text"),
				"from": map[string]any{"type": "string", "enum": []string{driveFromCompany, driveFromMine},
					"description": "Leave it out to search both. \"mine\" only when they ask about their own files ('in my Drive', 'my notes'); \"company\" when they ask for a shared or company file and not their own"},
				"limit": num("Most files to list from each Drive (default 10, max 25)"),
			}, "query"),
			Run: func(ctx context.Context, c *Call, raw json.RawMessage) (string, error) {
				var p struct {
					Query, From string
					Limit       int
				}
				json.Unmarshal(raw, &p)
				return a.driveSearch(ctx, c, p.Query, p.From, p.Limit)
			},
		},
		{
			Name: "drive_read",
			Desc: "Read one Google Drive file as text: a Doc, a Sheet (its first tab, as CSV), Slides, or an uploaded text or CSV file; a folder lists what is in it. " +
				"Pass the file_id and from that drive_search listed, or a Drive link somebody pasted. To find a row or a line in a long file, " +
				"set match: the whole file is read and only the lines containing it come back.",
			Params: schema(map[string]any{
				"file_id": str("The id drive_search listed, or the file's Drive link"),
				"from": map[string]any{"type": "string", "enum": []string{driveFromCompany, driveFromMine},
					"description": "Which Drive to read it from, as drive_search listed it. Left out, the company's is tried first"},
				"match": str("Only the lines containing this text, ignoring case — a name, an id, a word from the row you want (optional)"),
			}, "file_id"),
			Run: func(ctx context.Context, c *Call, raw json.RawMessage) (string, error) {
				var p struct {
					FileID string `json:"file_id"`
					From   string `json:"from"`
					Match  string `json:"match"`
				}
				json.Unmarshal(raw, &p)
				return a.driveRead(ctx, c, p.FileID, p.From, p.Match)
			},
		},
	}
}

// isDriveTool is either of them. Both can spend somebody's own account, so the log treats their
// results as it treats a script's (privateResult) and a refused repeat's arguments as private.
func isDriveTool(name string) bool { return name == "drive_search" || name == "drive_read" }

// driveGuidance is the prompt's paragraph on the two tools, for a channel that has them.
func driveGuidance(c *Call, company, own []*Connection) string {
	names := func(conns []*Connection) string {
		out := make([]string, len(conns))
		for i, conn := range conns {
			out[i] = conn.Name
		}
		return strings.Join(out, ", ")
	}
	var b strings.Builder
	b.WriteString("\nGoogle Drive: find a file with drive_search and read it with drive_read, not with http_request. ")
	switch {
	case len(company) > 0 && len(own) > 0:
		fmt.Fprintf(&b, "drive_search looks in the company's Drive (%s) and in <@%s>'s own (%s) together, and says whose each file is. ", names(company), c.UserID, names(own))
	case len(company) > 0:
		fmt.Fprintf(&b, "They reach the company's Drive (%s). ", names(company))
	default:
		fmt.Fprintf(&b, "They reach only <@%s>'s own Drive (%s); no company Drive is connected here. ", c.UserID, names(own))
	}
	if len(company) > 0 {
		b.WriteString("A file in the company's Drive needs nobody to connect anything, so never ask someone to connect their own account to reach one. ")
	}
	if len(own) > 0 {
		b.WriteString("Set from=\"mine\" only when they ask about their own files. ")
	}
	b.WriteString("When an instruction says where a file is, search for its title rather than assuming it is anyone's own.\n")
	return b.String()
}
