package memory

import (
	"context"
	"database/sql"
	"time"

	_ "github.com/lib/pq"
)

// PGBiTemporal is the bi-temporal model in Postgres: every version of a
// fact is a row with a valid-time interval [valid_from, valid_to) and a
// transaction-time interval [recorded_at, superseded_at). Nothing is ever
// updated in place except closing superseded_at; deletes and corrections
// supersede rows and insert truncated versions. A query picks the row whose
// two intervals contain the asked valid time and transaction time.
type PGBiTemporal struct {
	db      *sql.DB
	WriteMs []float64
	ReadMs  []float64
}

const pgSchema = `
DROP TABLE IF EXISTS memrevise_facts;
CREATE TABLE memrevise_facts (
	user_id       text        NOT NULL,
	attr          text        NOT NULL,
	value         text        NOT NULL,
	valid_from    timestamptz NOT NULL,
	valid_to      timestamptz,          -- NULL = still valid
	recorded_at   timestamptz NOT NULL,
	superseded_at timestamptz,          -- NULL = current belief
	event_id      int         NOT NULL  -- provenance
);
CREATE INDEX memrevise_facts_key ON memrevise_facts (user_id, attr, recorded_at);
`

func NewPGBiTemporal(ctx context.Context, db *sql.DB) (*PGBiTemporal, error) {
	if _, err := db.ExecContext(ctx, pgSchema); err != nil {
		return nil, err
	}
	return &PGBiTemporal{db: db}, nil
}

func (p *PGBiTemporal) Name() string { return "bi-temporal (Postgres)" }

func (p *PGBiTemporal) Apply(e Event) error {
	if e.Op == OpChatter {
		return nil
	}
	t := time.Now()
	defer func() { p.WriteMs = append(p.WriteMs, float64(time.Since(t).Microseconds())/1000) }()
	ctx := context.Background()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cut := e.ValidFrom
	if e.Op == OpDelete {
		cut = e.Recorded
	}
	// supersede every current version still valid at or after the cut, and
	// re-insert the part of each that lies before the cut
	_, err = tx.ExecContext(ctx, `
		WITH closed AS (
			UPDATE memrevise_facts SET superseded_at = $4
			WHERE user_id = $1 AND attr = $2 AND superseded_at IS NULL
			  AND (valid_to IS NULL OR valid_to > $3)
			RETURNING value, valid_from, event_id
		)
		INSERT INTO memrevise_facts (user_id, attr, value, valid_from, valid_to, recorded_at, event_id)
		SELECT $1, $2, value, valid_from, $3, $4, event_id FROM closed WHERE valid_from < $3`,
		e.User, e.Attr, cut, e.Recorded)
	if err != nil {
		return err
	}
	if e.Op != OpDelete {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memrevise_facts (user_id, attr, value, valid_from, recorded_at, event_id)
			VALUES ($1, $2, $3, $4, $5, $6)`, e.User, e.Attr, e.Value, e.ValidFrom, e.Recorded, e.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p *PGBiTemporal) Answer(q Query, now time.Time) (string, error) {
	vt, tx := times(q, now)
	t := time.Now()
	defer func() { p.ReadMs = append(p.ReadMs, float64(time.Since(t).Microseconds())/1000) }()
	var v string
	err := p.db.QueryRow(`
		SELECT value FROM memrevise_facts
		WHERE user_id = $1 AND attr = $2
		  AND recorded_at <= $4 AND (superseded_at IS NULL OR superseded_at > $4)
		  AND valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3)
		ORDER BY recorded_at DESC LIMIT 1`, q.User, q.Attr, vt, tx).Scan(&v)
	if err == sql.ErrNoRows {
		return None, nil
	}
	return v, err
}
