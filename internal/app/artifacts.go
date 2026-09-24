package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// An artifact is a file the bot makes because someone asked for one — "give me this as a
// file", an export, a report. It goes up to the thread so it is usable immediately, and the
// content is kept in the store so the console can show everything that has been produced.

const maxArtifactBytes = 1 << 20 // 1 MB of model output is already a very long document

// artifactKind is how one format is written to Slack and served back by the console.
type artifactKind struct {
	Ext string
	// Snippet is Slack's snippet_type; empty lets Slack render by the filename extension.
	Snippet string
	MIME    string
}

var artifactKinds = map[string]artifactKind{
	"md":   {"md", "markdown", "text/markdown; charset=utf-8"},
	"txt":  {"txt", "text", "text/plain; charset=utf-8"},
	"csv":  {"csv", "", "text/csv; charset=utf-8"},
	"json": {"json", "", "application/json; charset=utf-8"},
	"yaml": {"yaml", "", "text/yaml; charset=utf-8"},
	// Served as a download, never rendered: this is model-written markup and the console
	// origin holds the admin session. See the raw handler in admin_api.go.
	"html": {"html", "", "text/html; charset=utf-8"},
}

func artifactFormats() []string { return []string{"md", "txt", "csv", "json", "yaml", "html"} }

func (a *Agent) registerArtifactTools() {
	a.register(Tool{
		Name: "create_artifact",
		Desc: "Write content to a file and post it in this thread. Use it whenever someone asks for something as a file, a download, an export or a document (\"give me this as a file\", \"send that as CSV\"), and when what you have to say reads better as a report than as a message. Write the finished content yourself and in full — the file is what the person receives.",
		Params: schema(map[string]any{
			"title": str("Short title for the file, e.g. 'Access summary'"),
			"format": map[string]any{
				"type": "string", "enum": artifactFormats(),
				"description": "File format; md by default",
			},
			"content": str("The complete contents of the file"),
		}, "title", "content"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Title, Format, Content string }
			json.Unmarshal(args, &p)
			p.Title = strings.TrimSpace(p.Title)
			if p.Title == "" || strings.TrimSpace(p.Content) == "" {
				return "", fmt.Errorf("title and content are required")
			}
			if len(p.Content) > maxArtifactBytes {
				return "", fmt.Errorf("content is %s; the limit is %s — split it across two files",
					humanBytes(len(p.Content)), humanBytes(maxArtifactBytes))
			}
			format := strings.ToLower(strings.TrimSpace(p.Format))
			format = strings.TrimPrefix(format, ".")
			if format == "markdown" {
				format = "md"
			}
			kind, ok := artifactKinds[format]
			if !ok {
				if format != "" {
					return "", fmt.Errorf("unknown format %q; use one of %s", format, strings.Join(artifactFormats(), ", "))
				}
				format, kind = "md", artifactKinds["md"]
			}
			name := slug(p.Title)
			if name == "" {
				name = "artifact"
			}
			filename := name + "." + kind.Ext

			fileID, link, err := c.SL.uploadContent(ctx, c.Channel, c.ThreadTS, filename, p.Title, p.Content, kind.Snippet)
			if err != nil {
				return "", err
			}
			art := &Artifact{
				TeamID: c.TeamID, Channel: c.Channel, ThreadTS: c.ThreadTS, CreatedBy: c.UserID,
				Title: p.Title, Kind: format, Bytes: len(p.Content), Content: p.Content,
				FileID: fileID, Permalink: link,
			}
			if err := a.store.AddArtifact(ctx, c.OrgID, art); err != nil {
				// The file is in the thread; losing the record is worth a warning, not a failure.
				slog.Warn("artifact posted but not recorded", "title", p.Title, "err", err)
			}

			var b strings.Builder
			fmt.Fprintf(&b, "posted %s (%s) in this thread", filename, humanBytes(len(p.Content)))
			if link != "" {
				fmt.Fprintf(&b, "\nview: %s", link)
			}
			b.WriteString("\nThe file is already in the thread. Say in one line what it contains and link to it; do not repeat its contents in your reply.")
			return b.String(), nil
		},
	})
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
