package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Stored provider objects (video jobs, files, stored responses, vector
// stores, ...) live at the provider under the upstream's one key, which every
// caller of that upstream shares. The gateway therefore records who created
// each object and lets only that owner reach it afterwards. An object with no
// row here was never created through the gateway and is reachable by nobody.
var upstreamObjectMigration = migration{name: "0075_upstream_object", stmt: []string{
	`CREATE TABLE IF NOT EXISTS upstream_object (
		upstream_id TEXT NOT NULL,
		object_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		owner_type TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		model_id TEXT NOT NULL DEFAULT '',
		access_id TEXT NOT NULL DEFAULT '',
		requested_model TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		deleted_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (upstream_id, object_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_upstream_object_owner ON upstream_object(owner_type, owner_id, kind)`,
	`CREATE INDEX IF NOT EXISTS idx_upstream_object_id ON upstream_object(object_id)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_upstream_object_id`,
	`DROP INDEX IF EXISTS idx_upstream_object_owner`,
	`DROP TABLE IF EXISTS upstream_object`,
}}

// Jobs (batches, fine-tuning) finish at the provider long after the call that
// started them, so their usage is metered when they end. state follows that:
// open until the job is settled. claimed_until lets one replica at a time
// follow a job up; detail keeps who to bill.
var upstreamObjectJobMigration = migration{name: "0076_upstream_object_job", stmt: []string{
	`ALTER TABLE upstream_object ADD COLUMN state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE upstream_object ADD COLUMN claimed_until TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE upstream_object ADD COLUMN detail TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_upstream_object_state ON upstream_object(state, claimed_until)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_upstream_object_state`,
	`ALTER TABLE upstream_object DROP COLUMN detail`,
	`ALTER TABLE upstream_object DROP COLUMN claimed_until`,
	`ALTER TABLE upstream_object DROP COLUMN state`,
}}

// Job states of a stored object.
const (
	ObjectJobOpen    = "open"
	ObjectJobSettled = "settled"
)

// OpenObjectJob marks a recorded object as a job to follow up, with the
// attribution needed to bill it when it ends.
func (s *Store) OpenObjectJob(ctx context.Context, upstreamID, objectID, detail string) error {
	return s.exec(ctx, `UPDATE upstream_object SET state = ?, detail = ? WHERE upstream_id = ? AND object_id = ? AND state = ''`,
		ObjectJobOpen, detail, upstreamID, objectID)
}

// ObjectJob is an open job and its billing detail.
type ObjectJob struct {
	UpstreamObject
	Detail string
}

// ClaimObjectJobs hands out open jobs nobody is working on, holding each for
// hold so another replica does not follow the same job up at the same time.
func (s *Store) ClaimObjectJobs(ctx context.Context, limit int, hold time.Duration) ([]*ObjectJob, error) {
	now := nowUTC()
	rows, err := s.query(ctx, `SELECT `+upstreamObjectColumns+`, detail FROM upstream_object
		WHERE state = ? AND claimed_until < ? ORDER BY claimed_until, created_at LIMIT ?`, ObjectJobOpen, FormatTime(now), limit)
	if err != nil {
		return nil, err
	}
	var candidates []*ObjectJob
	for rows.Next() {
		var job ObjectJob
		o, err := scanUpstreamObject(func(dest ...any) error { return rows.Scan(append(dest, &job.Detail)...) })
		if err != nil {
			rows.Close()
			return nil, err
		}
		job.UpstreamObject = *o
		candidates = append(candidates, &job)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var claimed []*ObjectJob
	for _, job := range candidates {
		n, err := s.execCount(ctx, `UPDATE upstream_object SET claimed_until = ? WHERE upstream_id = ? AND object_id = ? AND state = ? AND claimed_until < ?`,
			FormatTime(now.Add(hold)), job.UpstreamID, job.ObjectID, ObjectJobOpen, FormatTime(now))
		if err != nil {
			return nil, err
		}
		if n == 1 {
			claimed = append(claimed, job)
		}
	}
	return claimed, nil
}

// SettleObjectJob closes a job: its usage has been recorded (or it ended with
// nothing to bill). It reports whether this call was the one that closed it.
func (s *Store) SettleObjectJob(ctx context.Context, upstreamID, objectID string) (bool, error) {
	n, err := s.execCount(ctx, `UPDATE upstream_object SET state = ? WHERE upstream_id = ? AND object_id = ? AND state = ?`,
		ObjectJobSettled, upstreamID, objectID, ObjectJobOpen)
	return n == 1, err
}

// Owner kinds of a stored provider object.
const (
	ObjectOwnerUser         = "user"
	ObjectOwnerServiceToken = "service_token"
)

// UpstreamObject is one stored provider object and who may reach it.
type UpstreamObject struct {
	UpstreamID string
	ObjectID   string
	Kind       string
	OwnerType  string
	OwnerID    string
	// ModelID is the catalog model that created the object and serves every
	// later call on it. AccessID is what the caller's grant was on (that
	// model, or the managed model that led to it): the grant is checked
	// again on each call. RequestedModel is the name the caller used.
	ModelID        string
	AccessID       string
	RequestedModel string
	ParentID       string
	CreatedAt      time.Time
	DeletedAt      time.Time
}

// ErrObjectOwnedByAnother reports an attempt to record an object id that is
// already recorded for a different owner.
var ErrObjectOwnedByAnother = errors.New("the object is recorded for another owner")

// RecordUpstreamObject records the owner of a newly created provider object.
// Recording the same object for the same owner again is a no-op; an id that
// already belongs to someone else is refused and left untouched.
func (s *Store) RecordUpstreamObject(ctx context.Context, o UpstreamObject) error {
	if o.UpstreamID == "" || o.ObjectID == "" || o.OwnerType == "" || o.OwnerID == "" {
		return &ValidationError{Field: "object_id", Message: "an object record needs an upstream, an id and an owner"}
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = nowUTC()
	}
	err := s.exec(ctx, `INSERT INTO upstream_object
		(upstream_id, object_id, kind, owner_type, owner_id, model_id, access_id, requested_model, parent_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (upstream_id, object_id) DO NOTHING`,
		o.UpstreamID, o.ObjectID, o.Kind, o.OwnerType, o.OwnerID, o.ModelID, o.AccessID, o.RequestedModel, o.ParentID, FormatTime(o.CreatedAt))
	if err != nil {
		return err
	}
	var ownerType, ownerID string
	row := s.runner(ctx).QueryRowContext(ctx, s.rebind(`SELECT owner_type, owner_id FROM upstream_object WHERE upstream_id = ? AND object_id = ?`), o.UpstreamID, o.ObjectID)
	if err := row.Scan(&ownerType, &ownerID); err != nil {
		return err
	}
	if ownerType != o.OwnerType || ownerID != o.OwnerID {
		return ErrObjectOwnedByAnother
	}
	return nil
}

const upstreamObjectColumns = `upstream_id, object_id, kind, owner_type, owner_id, model_id, access_id, requested_model, parent_id, created_at, deleted_at`

func scanUpstreamObject(scan func(...any) error) (*UpstreamObject, error) {
	var o UpstreamObject
	var created, deleted string
	if err := scan(&o.UpstreamID, &o.ObjectID, &o.Kind, &o.OwnerType, &o.OwnerID, &o.ModelID, &o.AccessID, &o.RequestedModel, &o.ParentID, &created, &deleted); err != nil {
		return nil, err
	}
	o.CreatedAt, o.DeletedAt = ParseTime(created), ParseTime(deleted)
	return &o, nil
}

// OwnedUpstreamObject returns the caller's live record for an object id, or
// ErrNotFound: for an id that was never recorded, that belongs to someone
// else, or that the owner deleted. The three are deliberately indistinguishable.
func (s *Store) OwnedUpstreamObject(ctx context.Context, ownerType, ownerID, objectID string) (*UpstreamObject, error) {
	row := s.runner(ctx).QueryRowContext(ctx, s.rebind(`SELECT `+upstreamObjectColumns+` FROM upstream_object
		WHERE object_id = ? AND owner_type = ? AND owner_id = ? AND deleted_at = '' ORDER BY created_at DESC LIMIT 1`), objectID, ownerType, ownerID)
	o, err := scanUpstreamObject(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

// OwnedObjectIDs reports which of ids are live objects of the owner, on one
// upstream or (upstreamID "") on any.
func (s *Store) OwnedObjectIDs(ctx context.Context, upstreamID, ownerType, ownerID string, ids []string) (map[string]bool, error) {
	owned := map[string]bool{}
	const chunk = 200
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		args := []any{ownerType, ownerID}
		where := ""
		if upstreamID != "" {
			where = " AND upstream_id = ?"
			args = append(args, upstreamID)
		}
		for _, id := range part {
			args = append(args, id)
		}
		rows, err := s.query(ctx, `SELECT object_id FROM upstream_object WHERE owner_type = ? AND owner_id = ? AND deleted_at = ''`+where+`
			AND object_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			owned[id] = true
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return owned, nil
}

// ObjectHomes lists where the owner's live objects of one kind were created:
// one representative record per upstream, newest first. A list call with no
// model named is routed by it.
func (s *Store) ObjectHomes(ctx context.Context, ownerType, ownerID, kind string) ([]*UpstreamObject, error) {
	rows, err := s.query(ctx, `SELECT `+upstreamObjectColumns+` FROM upstream_object
		WHERE owner_type = ? AND owner_id = ? AND kind = ? AND deleted_at = '' ORDER BY created_at DESC LIMIT 500`, ownerType, ownerID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var homes []*UpstreamObject
	for rows.Next() {
		o, err := scanUpstreamObject(rows.Scan)
		if err != nil {
			return nil, err
		}
		if !seen[o.UpstreamID] {
			seen[o.UpstreamID] = true
			homes = append(homes, o)
		}
	}
	return homes, rows.Err()
}

// MarkUpstreamObjectDeleted records that the owner deleted the object at the provider.
func (s *Store) MarkUpstreamObjectDeleted(ctx context.Context, upstreamID, objectID string) error {
	return s.exec(ctx, `UPDATE upstream_object SET deleted_at = ? WHERE upstream_id = ? AND object_id = ? AND deleted_at = ''`,
		FormatTime(nowUTC()), upstreamID, objectID)
}
