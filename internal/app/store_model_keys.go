package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ModelKeyRef is an organisation's own model endpoint without its key: everything the console,
// the settings cache and the operator are ever told about it.
type ModelKeyRef struct {
	Preset       string `json:"preset"` // openai | openrouter | compatible: which form the console shows
	BaseURL      string `json:"base_url"`
	KeyHint      string `json:"key_hint"` // "…abcd"
	KeyFP        string `json:"key_fp"`   // secretFingerprint of the key
	DefaultModel string `json:"default_model"`
	EmbedModel   string `json:"embed_model"` // "" = document search is off while this key is in use
	FixJobs      bool   `json:"fix_jobs"`
	UpdatedBy    string `json:"updated_by"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	LastOKAt     string `json:"last_ok_at"`
	LastError    string `json:"last_error"`
	LastErrorAt  string `json:"last_error_at"`
}

// Host is the base URL's host, which is what a person recognises an endpoint by.
func (r ModelKeyRef) Host() string { return urlHost(r.BaseURL) }

// Failing reports that the last thing the endpoint said was an error.
func (r ModelKeyRef) Failing() bool {
	return r.LastErrorAt != "" && r.LastErrorAt >= r.LastOKAt
}

// sameEndpoint reports whether two refs describe the same client: same place, same key, same
// models. It is how the resolver notices a change made on another instance, so it compares
// everything a client is built from rather than a timestamp alone — two saves inside one second
// share an updated_at.
func (r ModelKeyRef) sameEndpoint(o ModelKeyRef) bool {
	return r.BaseURL == o.BaseURL && r.KeyFP == o.KeyFP && r.DefaultModel == o.DefaultModel &&
		r.EmbedModel == o.EmbedModel && r.UpdatedAt == o.UpdatedAt
}

func urlHost(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

const modelKeyCols = `preset, base_url, key_hint, key_fp, default_model, embed_model, fix_jobs, updated_by,
	created_at, updated_at, last_ok_at, last_error, last_error_at`

// ModelKeyRef reads an organisation's own endpoint without its key. No row is nil and no error.
func (s *Store) ModelKeyRef(ctx context.Context, orgID int64) (*ModelKeyRef, error) {
	var r ModelKeyRef
	var fixJobs int
	err := s.db.QueryRowContext(ctx, `select `+modelKeyCols+` from org_model_keys where org_id=?`, orgID).Scan(
		&r.Preset, &r.BaseURL, &r.KeyHint, &r.KeyFP, &r.DefaultModel, &r.EmbedModel, &fixJobs, &r.UpdatedBy,
		&r.CreatedAt, &r.UpdatedAt, &r.LastOKAt, &r.LastError, &r.LastErrorAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.FixJobs = fixJobs == 1
	return &r, nil
}

// errModelKeyUnreadable is a stored key that cannot be opened with this deployment's MASTER_KEY.
var errModelKeyUnreadable = errors.New("the organisation's model key cannot be read with the current MASTER_KEY")

// errModelKeyChanged is a save made against a key that has since been replaced or moved: another
// admin saved in between, and writing this one over it would pair their key with the old address.
var errModelKeyChanged = errors.New("the model key was changed while this was being saved; reload the page and try again")

// ModelKeyRow reads an organisation's endpoint and opens its key in the same query. Anything that
// sends the key somewhere reads it this way, so the key and the address it was saved for can never
// come from two different moments — a key rotated between two reads would otherwise be sent to the
// address it replaced. No row is nil, "" and no error; a key that cannot be opened is
// errModelKeyUnreadable, with the row.
func (s *Store) ModelKeyRow(ctx context.Context, orgID int64, sealer *Sealer) (*ModelKeyRef, string, error) {
	var r ModelKeyRef
	var fixJobs int
	var enc []byte
	err := s.db.QueryRowContext(ctx, `select `+modelKeyCols+`, key_enc from org_model_keys where org_id=?`, orgID).Scan(
		&r.Preset, &r.BaseURL, &r.KeyHint, &r.KeyFP, &r.DefaultModel, &r.EmbedModel, &fixJobs, &r.UpdatedBy,
		&r.CreatedAt, &r.UpdatedAt, &r.LastOKAt, &r.LastError, &r.LastErrorAt, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	r.FixJobs = fixJobs == 1
	plain, err := sealer.Open(enc)
	if err != nil {
		return &r, "", fmt.Errorf("%w: %v", errModelKeyUnreadable, err)
	}
	return &r, string(plain), nil
}

// ModelKeySecret opens an organisation's stored key. ok is false when there is no row.
func (s *Store) ModelKeySecret(ctx context.Context, orgID int64, sealer *Sealer) (key string, ok bool, err error) {
	var enc []byte
	err = s.db.QueryRowContext(ctx, `select key_enc from org_model_keys where org_id=?`, orgID).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	plain, err := sealer.Open(enc)
	if err != nil {
		return "", true, fmt.Errorf("the organisation's model key cannot be read with the current MASTER_KEY: %w", err)
	}
	return string(plain), true, nil
}

// PutModelKey writes an organisation's own endpoint. An empty key keeps the one already stored, so
// changing a model or the fix-jobs switch does not make anybody paste the key again — and then the
// write only lands on the row it was made against: ref.BaseURL and ref.KeyFP are what the caller
// read, and if another save has replaced either since, nothing is written and the answer is
// errModelKeyChanged. A save that carries a key has just been checked against the endpoint
// (handleModelKeyPut), so the status starts out as that check's success.
func (s *Store) PutModelKey(ctx context.Context, orgID int64, ref ModelKeyRef, key, by string, sealer *Sealer) error {
	fixJobs := 0
	if ref.FixJobs {
		fixJobs = 1
	}
	at := now()
	if key == "" {
		res, err := s.db.ExecContext(ctx, `update org_model_keys set preset=?, default_model=?, embed_model=?,
			fix_jobs=?, updated_by=?, updated_at=? where org_id=? and base_url=? and key_fp=?`,
			ref.Preset, ref.DefaultModel, ref.EmbedModel, fixJobs, by, at, orgID, ref.BaseURL, ref.KeyFP)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errModelKeyChanged
		}
		return nil
	}
	enc, err := sealer.Seal([]byte(key))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `insert into org_model_keys (org_id, preset, base_url, key_enc, key_hint, key_fp,
		default_model, embed_model, fix_jobs, updated_by, created_at, updated_at, last_ok_at, last_error, last_error_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '')
		on conflict(org_id) do update set preset=excluded.preset, base_url=excluded.base_url, key_enc=excluded.key_enc,
			key_hint=excluded.key_hint, key_fp=excluded.key_fp, default_model=excluded.default_model,
			embed_model=excluded.embed_model, fix_jobs=excluded.fix_jobs, updated_by=excluded.updated_by,
			updated_at=excluded.updated_at, last_ok_at=excluded.last_ok_at, last_error='', last_error_at=''`,
		orgID, ref.Preset, ref.BaseURL, enc, fingerprint(key), secretFingerprint(key), ref.DefaultModel, ref.EmbedModel,
		fixJobs, by, at, at, at)
	return err
}

// DeleteModelKey removes an organisation's own endpoint; its next model call is on the
// deployment's key. removed is false when there was nothing to remove.
func (s *Store) DeleteModelKey(ctx context.Context, orgID int64) (removed bool, err error) {
	res, err := s.db.ExecContext(ctx, `delete from org_model_keys where org_id=?`, orgID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkModelKeyStatus records the outcome of a call on the organisation's own endpoint: an empty
// problem is a success. Callers write it at most once a minute (modelEndpoints.note).
func (s *Store) MarkModelKeyStatus(ctx context.Context, orgID int64, problem string) error {
	if problem == "" {
		_, err := s.db.ExecContext(ctx, `update org_model_keys set last_ok_at=? where org_id=?`, now(), orgID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `update org_model_keys set last_error=?, last_error_at=? where org_id=?`,
		truncate(problem, 500), now(), orgID)
	return err
}
