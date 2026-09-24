package app

import (
	"context"
	"database/sql"
)

// DriveSync is one Drive folder kept in step with one Documents folder.
type DriveSync struct {
	ID           int64  `json:"id"`
	ConnectionID int64  `json:"connection_id"`
	ConnName     string `json:"connection_name"` // joined, so the console can name the credential
	// BundleName is the access bundle the credential is filed under. Joined for the same
	// reason as the name: two bundles can each hold a connection called "Drive", and the
	// console has to say which one a sync is spending.
	BundleName  string `json:"bundle_name"`
	FolderID    string `json:"folder_id"`
	FolderName  string `json:"folder_name"`
	Dest        string `json:"dest"`
	Scope       string `json:"scope"`
	Recurse     bool   `json:"recurse"`
	Enabled     bool   `json:"enabled"`
	LastRun     string `json:"last_run"`
	LastStatus  string `json:"last_status"`
	LastError   string `json:"last_error"`
	LastAdded   int    `json:"last_added"`
	LastUpdated int    `json:"last_updated"`
	LastRemoved int    `json:"last_removed"`
	// Docs is how many documents this sync currently owns. Counted from drive_sync_files
	// rather than from the folder, because a person's upload can sit in the same folder.
	Docs      int    `json:"docs"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

const driveSyncCols = `s.id, s.connection_id, coalesce(c.name,''), coalesce(b.name,''), s.folder_id, s.folder_name, s.dest, s.scope,
	s.recurse, s.enabled, coalesce(s.last_run,''), s.last_status, s.last_error,
	s.last_added, s.last_updated, s.last_removed, coalesce(s.created_by,''), s.created_at,
	(select count(*) from drive_sync_files f where f.org_id=s.org_id and f.sync_id=s.id)`

func scanDriveSync(sc interface{ Scan(...any) error }) (*DriveSync, error) {
	var d DriveSync
	err := sc.Scan(&d.ID, &d.ConnectionID, &d.ConnName, &d.BundleName, &d.FolderID, &d.FolderName, &d.Dest, &d.Scope,
		&d.Recurse, &d.Enabled, &d.LastRun, &d.LastStatus, &d.LastError,
		&d.LastAdded, &d.LastUpdated, &d.LastRemoved, &d.CreatedBy, &d.CreatedAt, &d.Docs)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// DriveSyncs lists one organisation's syncs: the ones that run first, then alphabetically.
// Not by id — a list whose order says when each was added buries the folder somebody actually
// looks at, and an order that moves after every pass is harder to read than one that holds.
func (s *Store) DriveSyncs(ctx context.Context, orgID int64) ([]*DriveSync, error) {
	rows, err := s.db.QueryContext(ctx, `select `+driveSyncCols+` from drive_syncs s
		left join connections c on c.id = s.connection_id and c.org_id = s.org_id
		left join bundles b on b.id = c.bundle_id and b.org_id = s.org_id
		where s.org_id=? order by s.enabled desc, s.folder_name, s.id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*DriveSync{}
	for rows.Next() {
		d, err := scanDriveSync(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// EnabledDriveSyncs is what the scheduled pass walks: every sync across every organisation,
// so one loop serves the whole deployment. The organisation each belongs to travels with it.
func (s *Store) EnabledDriveSyncs(ctx context.Context) (map[int64][]*DriveSync, error) {
	rows, err := s.db.QueryContext(ctx, `select s.org_id, `+driveSyncCols+` from drive_syncs s
		left join connections c on c.id = s.connection_id and c.org_id = s.org_id
		left join bundles b on b.id = c.bundle_id and b.org_id = s.org_id
		where s.enabled=1 order by s.org_id, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]*DriveSync{}
	for rows.Next() {
		var orgID int64
		var d DriveSync
		if err := rows.Scan(&orgID, &d.ID, &d.ConnectionID, &d.ConnName, &d.BundleName, &d.FolderID, &d.FolderName, &d.Dest, &d.Scope,
			&d.Recurse, &d.Enabled, &d.LastRun, &d.LastStatus, &d.LastError,
			&d.LastAdded, &d.LastUpdated, &d.LastRemoved, &d.CreatedBy, &d.CreatedAt, &d.Docs); err != nil {
			return nil, err
		}
		copy := d
		out[orgID] = append(out[orgID], &copy)
	}
	return out, rows.Err()
}

func (s *Store) DriveSync(ctx context.Context, orgID, id int64) (*DriveSync, error) {
	row := s.db.QueryRowContext(ctx, `select `+driveSyncCols+` from drive_syncs s
		left join connections c on c.id = s.connection_id and c.org_id = s.org_id
		left join bundles b on b.id = c.bundle_id and b.org_id = s.org_id
		where s.org_id=? and s.id=?`, orgID, id)
	return scanDriveSync(row)
}

// AddDriveSync records a new folder to follow. The unique index is what stops the same Drive
// folder being added twice on the same credential, which would have two syncs fighting over
// the same documents.
func (s *Store) AddDriveSync(ctx context.Context, orgID int64, d DriveSync) (int64, error) {
	if s.countRows(ctx, "drive_syncs", orgID) >= maxDriveSyncsPerOrg {
		return 0, errOrgCap
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into drive_syncs
		(org_id, connection_id, folder_id, folder_name, dest, scope, recurse, enabled, created_by)
		values (?,?,?,?,?,?,?,1,?) returning id`,
		orgID, d.ConnectionID, d.FolderID, d.FolderName, d.Dest, d.Scope, d.Recurse, d.CreatedBy).Scan(&id)
	return id, err
}

// UpdateDriveSync changes where a sync lands and whether it runs. The connection and the Drive
// folder are not editable: changing either would make every file the sync already owns belong
// to something else, so that is a new sync and a deletion.
func (s *Store) UpdateDriveSync(ctx context.Context, orgID, id int64, dest, scope string, recurse, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `update drive_syncs set dest=?, scope=?, recurse=?, enabled=? where org_id=? and id=?`,
		dest, scope, recurse, enabled, orgID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteDriveSync forgets a sync and the record of what it owned. The documents themselves
// stay: stopping a sync is not a reason to take away what the bot already answers from, and
// anything unwanted can be deleted in the table like any other document.
func (s *Store) DeleteDriveSync(ctx context.Context, orgID, id int64) error {
	if _, err := s.db.ExecContext(ctx, `delete from drive_sync_files where org_id=? and sync_id=?`, orgID, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `delete from drive_syncs where org_id=? and id=?`, orgID, id)
	return err
}

// DeleteDriveSyncsForConnection drops the syncs that spent a credential that has just been
// deleted. Without this they would run every six hours against nothing and report an error
// each time, which reads as a fault rather than as the consequence it is.
func (s *Store) DeleteDriveSyncsForConnection(ctx context.Context, orgID, connID int64) error {
	if _, err := s.db.ExecContext(ctx, `delete from drive_sync_files where org_id=? and sync_id in
		(select id from drive_syncs where org_id=? and connection_id=?)`, orgID, orgID, connID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `delete from drive_syncs where org_id=? and connection_id=?`, orgID, connID)
	return err
}

// StartDriveSyncRun marks a sync as running, so the console shows a spinner rather than the
// previous result while a manual run is in flight.
func (s *Store) StartDriveSyncRun(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx, `update drive_syncs set last_status='running' where org_id=? and id=?`, orgID, id)
	return err
}

// FinishDriveSyncRun records what one pass did. An error keeps the counts from the pass that
// still managed some work, because "failed after taking 3 of 40" is the useful sentence.
func (s *Store) FinishDriveSyncRun(ctx context.Context, orgID, id int64, rep DriveReport, errMsg string) error {
	status := "ok"
	if errMsg != "" {
		status = "error"
	}
	_, err := s.db.ExecContext(ctx, `update drive_syncs set last_run=?, last_status=?, last_error=?,
		last_added=?, last_updated=?, last_removed=?, folder_name=coalesce(nullif(?, ''), folder_name)
		where org_id=? and id=?`,
		now(), status, truncate(errMsg, 500), rep.Added, rep.Updated, rep.Removed, rep.FolderName, orgID, id)
	return err
}

// driveSyncFile is one Drive file this sync owns and the document it became.
type driveSyncFile struct {
	FileID  string
	Path    string
	Version string
}

// DriveSyncFiles is what a sync owned as of its last pass, keyed by Drive file id.
func (s *Store) DriveSyncFiles(ctx context.Context, orgID, syncID int64) (map[string]driveSyncFile, error) {
	rows, err := s.db.QueryContext(ctx, `select file_id, path, version from drive_sync_files where org_id=? and sync_id=?`, orgID, syncID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]driveSyncFile{}
	for rows.Next() {
		var f driveSyncFile
		if err := rows.Scan(&f.FileID, &f.Path, &f.Version); err != nil {
			return nil, err
		}
		out[f.FileID] = f
	}
	return out, rows.Err()
}

// MarkDriveSyncFile remembers that a Drive file is now a document at path, in the version the
// copy was taken from.
func (s *Store) MarkDriveSyncFile(ctx context.Context, orgID, syncID int64, f driveSyncFile) error {
	_, err := s.db.ExecContext(ctx, `insert into drive_sync_files (org_id, sync_id, file_id, path, version, seen_at)
		values (?,?,?,?,?,?)
		on conflict(org_id, sync_id, file_id) do update set path=excluded.path, version=excluded.version, seen_at=excluded.seen_at`,
		orgID, syncID, f.FileID, f.Path, f.Version, now())
	return err
}

// TouchDriveSyncFile says a file came back unchanged: nothing was downloaded, but it is still
// there, so it must not be swept as a deletion.
func (s *Store) TouchDriveSyncFile(ctx context.Context, orgID, syncID int64, fileID string) error {
	_, err := s.db.ExecContext(ctx, `update drive_sync_files set seen_at=? where org_id=? and sync_id=? and file_id=?`,
		now(), orgID, syncID, fileID)
	return err
}

func (s *Store) ForgetDriveSyncFile(ctx context.Context, orgID, syncID int64, fileID string) error {
	_, err := s.db.ExecContext(ctx, `delete from drive_sync_files where org_id=? and sync_id=? and file_id=?`, orgID, syncID, fileID)
	return err
}

// DriveSyncedPaths is every document path owned by any sync in one organisation. The documents
// table shows a Drive badge from this, and nothing else may claim a path that is in it.
func (s *Store) DriveSyncedPaths(ctx context.Context, orgID int64) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `select path, sync_id from drive_sync_files where org_id=?`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var id int64
		if err := rows.Scan(&p, &id); err != nil {
			return nil, err
		}
		out[p] = id
	}
	return out, rows.Err()
}
