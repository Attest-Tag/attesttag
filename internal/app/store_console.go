package app

import (
	"context"
	"encoding/json"
)

// Roles an organisation defined for itself. The built-in three live in code (console_roles.go);
// these are the extra tiers an admin writes when "editor" is the wrong shape for somebody.
//
// Who holds a role is a membership (store_identity.go), not a row here: a role is a named set of
// permissions, and the person it applies to belongs to exactly one organisation at a time.

type ConsoleRole struct {
	ID          int64
	Key, Label  string
	Permissions []string
	CreatedBy   string
}

// ---- custom roles ----

func (s *Store) ConsoleRoles(ctx context.Context, orgID int64) ([]ConsoleRole, error) {
	rows, err := s.db.QueryContext(ctx, `select id, key, label, permissions, coalesce(created_by,'')
		from console_roles where org_id=? order by label`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConsoleRole{}
	for rows.Next() {
		var r ConsoleRole
		var perms string
		if err := rows.Scan(&r.ID, &r.Key, &r.Label, &perms, &r.CreatedBy); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(perms), &r.Permissions)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CustomRoleMap is what permissionsForRole takes as its second argument.
func (s *Store) CustomRoleMap(ctx context.Context, orgID int64) map[string][]Permission {
	m := map[string][]Permission{}
	rs, err := s.ConsoleRoles(ctx, orgID)
	if err != nil {
		return m
	}
	for _, r := range rs {
		m[r.Key] = r.Permissions
	}
	return m
}

func (s *Store) UpsertConsoleRole(ctx context.Context, orgID int64, r *ConsoleRole) error {
	perms, _ := json.Marshal(sanitizePermissions(r.Permissions))
	_, err := s.db.ExecContext(ctx, `insert into console_roles (org_id, key, label, permissions, created_by)
		values (?, ?, ?, ?, ?)
		on conflict(org_id, key) do update set label=excluded.label, permissions=excluded.permissions`,
		orgID, r.Key, r.Label, string(perms), r.CreatedBy)
	return err
}

func (s *Store) DeleteConsoleRole(ctx context.Context, orgID int64, key string) error {
	_, err := s.db.ExecContext(ctx, `delete from console_roles where org_id=? and key=?`, orgID, key)
	return err
}
