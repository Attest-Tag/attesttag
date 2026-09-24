package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// Setup links let an admin who doesn't hold a secret ask the person who does to paste it,
// without the secret passing through chat. The link is one-time and expires in 7 days; the
// connection is created in status "pending" until an admin approves it in the console.

type SetupLink struct {
	Token        string `json:"token"`
	OrgID        int64  `json:"-"`
	BundleID     int64  `json:"bundle_id"`
	Preset       string `json:"preset"`
	Name         string `json:"name"`
	CreatedBy    string `json:"created_by"`
	Status       string `json:"status"`
	ConnectionID int64  `json:"connection_id"`
	ExpiresAt    string `json:"expires_at"`
	URL          string `json:"url"`
}

func (s *Store) CreateSetupLink(ctx context.Context, orgID, bundleID int64, preset, name, by string) (string, error) {
	tok := randomToken()
	_, err := s.db.ExecContext(ctx, `insert into setup_links (token, org_id, bundle_id, preset, name, created_by, expires_at) values (?, ?, ?, ?, ?, ?, ?)`,
		tok, orgID, bundleID, preset, name, by, time.Now().Add(7*24*time.Hour).UTC().Format(time.DateTime))
	return tok, err
}

func (s *Store) SetupLink(ctx context.Context, tok string) (*SetupLink, error) {
	var l SetupLink
	err := s.db.QueryRowContext(ctx, `select token, org_id, bundle_id, preset, coalesce(name,''), coalesce(created_by,''), status, coalesce(connection_id,0), expires_at
		from setup_links where token=?`, tok).Scan(&l.Token, &l.OrgID, &l.BundleID, &l.Preset, &l.Name, &l.CreatedBy, &l.Status, &l.ConnectionID, &l.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if l.Status == "pending" && l.ExpiresAt < now() {
		l.Status = "expired"
	}
	return &l, nil
}

func (s *Store) SetupLinksForBundle(ctx context.Context, orgID, bundleID int64) ([]SetupLink, error) {
	rows, err := s.db.QueryContext(ctx, `select token, org_id, bundle_id, preset, coalesce(name,''), coalesce(created_by,''), status, coalesce(connection_id,0), expires_at
		from setup_links where org_id=? and bundle_id=? order by created_at desc`, orgID, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SetupLink{}
	for rows.Next() {
		var l SetupLink
		if err := rows.Scan(&l.Token, &l.OrgID, &l.BundleID, &l.Preset, &l.Name, &l.CreatedBy, &l.Status, &l.ConnectionID, &l.ExpiresAt); err != nil {
			return nil, err
		}
		if l.Status == "pending" && l.ExpiresAt < now() {
			l.Status = "expired"
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) FinishSetupLink(ctx context.Context, orgID, connID int64, tok string) error {
	_, err := s.db.ExecContext(ctx, `update setup_links set status='submitted', connection_id=? where org_id=? and token=?`, connID, orgID, tok)
	return err
}

func (s *Store) RevokeSetupLink(ctx context.Context, orgID int64, tok string) error {
	_, err := s.db.ExecContext(ctx, `update setup_links set status='revoked' where org_id=? and token=? and status='pending'`, orgID, tok)
	return err
}

var setupPage = template.Must(template.New("setup").Parse(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Connect {{.Preset.Name}}</title>
<style>
:root{--bg:#fbfaf8;--fg:#191c2b;--card:#fff;--muted:#f2f1ed;--muted-fg:#5d6170;--border:#e8e6e1;--input:#d4d2cb;--primary:#5a50c8;--ok:#2e7d4f;--bad:#bf3b2b}
@media (prefers-color-scheme:dark){:root{--bg:#101019;--fg:#eceaf3;--card:#17161f;--muted:#1d1c27;--muted-fg:#9a97a8;--border:#292834;--input:#363443;--primary:#8a80e8;--ok:#4caf7d;--bad:#e0604f}}
body{margin:0;background:var(--bg);color:var(--fg);font:13px/1.5 Geist,ui-sans-serif,system-ui,sans-serif}
main{max-width:520px;margin:48px auto;padding:0 20px}.card{background:var(--card);border:1px solid var(--border);border-radius:6px;box-shadow:0 1px 2px rgba(16,24,40,.04);padding:18px}
h1{font-size:18px;margin:0 0 6px}p{margin:0 0 10px;color:var(--muted-fg)}label{display:block;font-weight:500;margin:12px 0 4px}
input,textarea{width:100%;box-sizing:border-box;border:1px solid var(--input);border-radius:3px;background:var(--card);color:var(--fg);padding:7px 9px;font:inherit}
textarea{min-height:120px;font-family:ui-monospace,Menlo,monospace;font-size:12px}
button{margin-top:14px;background:var(--primary);color:#fff;border:0;border-radius:3px;padding:8px 14px;font:inherit;font-weight:500;cursor:pointer}
.chip{display:inline-block;font-size:11px;background:var(--muted);border:1px solid var(--border);border-radius:3px;padding:3px 6px;margin-right:4px;font-family:ui-monospace,monospace}
.ok{color:var(--ok)}.bad{color:var(--bad)}.eyebrow{font-size:11px;letter-spacing:.06em;text-transform:uppercase;color:var(--muted-fg)}
</style>
<main><div class="card">
<div class="eyebrow">attest_tag · setup link</div>
{{if .Done}}<h1>Thanks, it's submitted</h1><p>An admin will review and approve the connection. You can close this page.</p>
{{else if ne .Link.Status "pending"}}<h1>This link is {{.Link.Status}}</h1><p>Ask the admin who sent it for a new one.</p>
{{else}}
<h1>Connect {{.Preset.Name}}</h1>
<p>{{.Link.CreatedBy}} asked you to add the {{.Preset.Name}} credential for the Slack assistant. It is stored encrypted and never shown again. Use a dedicated account for the bot, not your personal login.</p>
<p>Allowed websites: {{range .Hosts}}<span class="chip">{{.}}</span>{{end}}</p>
{{if .Error}}<p class="bad">{{.Error}}</p>{{end}}
<form method="post">
<label>{{.Preset.SecretLabel}}</label>
{{if eq .Preset.CredType "gcp_sa"}}<textarea name="sa_json" placeholder='{{.Preset.Placeholder}}' required></textarea>
{{else if eq .Preset.CredType "basic"}}<input name="user" placeholder="email or user" required><label>Password / API token</label><input name="password" type="password" required>
{{else}}<input name="token" type="password" placeholder="{{.Preset.Placeholder}}" required>{{end}}
{{range .Preset.ExtraHeaders}}<label>{{.Name}}</label><input name="hdr_{{.Name}}" type="password" required>{{end}}
<p style="margin-top:6px">{{.Preset.SecretHint}}{{if .Preset.DocsURL}} <a href="{{.Preset.DocsURL}}" target="_blank" rel="noopener">Where do I find this?</a>{{end}}</p>
<button type="submit">Submit credential</button>
</form>
{{end}}
</div></main>`))

// handleSetupPage serves the public form for a setup link and accepts the submission.
func (b *Bot) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	link, err := b.store.SetupLink(r.Context(), tok)
	if err != nil || link == nil {
		http.Error(w, "unknown setup link", http.StatusNotFound)
		return
	}
	pr := presetByID(link.Preset)
	if pr == nil {
		pr = presetByID("custom")
	}
	data := map[string]any{"Link": link, "Preset": pr, "Hosts": pr.Hosts, "Done": false, "Error": ""}
	if r.Method == http.MethodPost && link.Status == "pending" {
		r.ParseForm()
		sec := &Secret{Token: strings.TrimSpace(r.FormValue("token")), User: r.FormValue("user"), Password: r.FormValue("password"), SAJSON: r.FormValue("sa_json")}
		for _, h := range pr.ExtraHeaders {
			if v := r.FormValue("hdr_" + h.Name); v != "" {
				if sec.Headers == nil {
					sec.Headers = map[string]string{}
				}
				sec.Headers[h.Name] = v
			}
		}
		org := link.OrgID
		in := &connectionInput{BundleID: link.BundleID, Name: link.Name, Preset: link.Preset, Secret: sec, Status: "pending"}
		c, s, err := b.buildConnection(in, nil)
		if err == nil {
			var enc []byte
			enc, err = b.sealSecret(s)
			if err == nil {
				c.CreatedBy = "setup-link:" + link.CreatedBy
				var id int64
				id, err = b.store.InsertConnection(r.Context(), org, c, enc)
				if err == nil {
					b.store.FinishSetupLink(r.Context(), org, id, tok)
					b.changed(r.Context(), org)
					data["Done"] = true
				}
			}
		}
		if err != nil {
			data["Error"] = err.Error()
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	setupPage.Execute(w, data)
}

func (b *Bot) setupLinkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup/{token}", b.handleSetupPage)
	mux.HandleFunc("POST /setup/{token}", b.handleSetupPage)
	mux.HandleFunc("GET /api/bundles/{id}/setup-links", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		ls, err := b.store.SetupLinksForBundle(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			fail(w, err)
			return
		}
		for i := range ls {
			ls[i].URL = b.baseURL(r) + "/setup/" + ls[i].Token
		}
		writeJSON(w, 200, ls)
	}))
	mux.HandleFunc("POST /api/bundles/{id}/setup-links", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Preset, Name string }
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		pr := presetByID(in.Preset)
		if pr == nil || pr.CredType == "mcp" || pr.CredType == "custom" {
			bad(w, fmt.Errorf("setup links are available for preset services with a single secret"))
			return
		}
		if in.Name == "" {
			in.Name = pr.Name
		}
		tok, err := b.store.CreateSetupLink(r.Context(), orgOf(r), pathID(r, "id"), in.Preset, in.Name, adminFromCtx(r.Context()).Name)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"token": tok, "url": b.baseURL(r) + "/setup/" + tok})
	}))
	mux.HandleFunc("DELETE /api/setup-links/{token}", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.RevokeSetupLink(r.Context(), orgOf(r), r.PathValue("token")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
}

var _ = json.Marshal
