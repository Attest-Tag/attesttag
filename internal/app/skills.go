package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Skills are Markdown instructions attached to a bundle: how to use a tool well, house
// conventions, runbooks. Enabled skills travel with the bundle into every scope it covers and
// are appended to the system prompt (capped, so a long skill file can't crowd out the thread).

const skillsCap = 12_000

func (b *Bot) skillsRoutes(mux *http.ServeMux) {
	a := b.requireAdmin
	mux.HandleFunc("GET /api/bundles/{id}/skills", a(func(w http.ResponseWriter, r *http.Request) {
		ks, err := b.store.SkillsForBundle(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, ks)
	}))
	mux.HandleFunc("POST /api/bundles/{id}/skills", b.requirePerm(PermBundlesManage, func(w http.ResponseWriter, r *http.Request) {
		var in Skill
		if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Content) == "" {
			bad(w, fmt.Errorf("name and content are required"))
			return
		}
		in.BundleID, in.ID = pathID(r, "id"), 0
		if !in.Enabled {
			in.Enabled = true
		}
		id, err := b.store.UpsertSkill(r.Context(), orgOf(r), in)
		if err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 201, map[string]any{"id": id})
	}))
	mux.HandleFunc("PUT /api/skills/{id}", b.requirePerm(PermBundlesManage, func(w http.ResponseWriter, r *http.Request) {
		var in Skill
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		in.ID = pathID(r, "id")
		if _, err := b.store.UpsertSkill(r.Context(), orgOf(r), in); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/skills/{id}", b.requirePerm(PermBundlesManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DeleteSkill(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
}

// skillsText renders enabled skills of the given bundles for the system prompt.
func (s *Store) skillsText(ctx context.Context, orgID int64, bundleIDs []int64) string {
	var b strings.Builder
	for _, id := range bundleIDs {
		ks, _ := s.SkillsForBundle(ctx, orgID, id)
		for _, k := range ks {
			if !k.Enabled {
				continue
			}
			if b.Len()+len(k.Content) > skillsCap {
				b.WriteString("\n[more skills omitted for length]\n")
				return b.String()
			}
			fmt.Fprintf(&b, "\n## Skill: %s\n%s\n", k.Name, strings.TrimSpace(k.Content))
		}
	}
	return b.String()
}
