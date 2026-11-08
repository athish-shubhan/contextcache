package retrieve

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PGVector keeps embeddings in Postgres with the pgvector extension and an
// HNSW index on cosine distance: the shape of a production tool or memory
// index, where the catalog lives next to the rest of the gateway's state.
type PGVector struct {
	DB    *sql.DB
	Table string // a plain SQL identifier; it is formatted into statements
	Dim   int
}

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

func (p *PGVector) check() error {
	if !identRE.MatchString(p.Table) {
		return fmt.Errorf("pgvector: table name %q is not a plain identifier", p.Table)
	}
	if p.Dim <= 0 {
		return fmt.Errorf("pgvector: dimension %d", p.Dim)
	}
	return nil
}

func vecLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', 7, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// Reset recreates the table.
func (p *PGVector) Reset(ctx context.Context) error {
	if err := p.check(); err != nil {
		return err
	}
	for _, q := range []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		fmt.Sprintf(`DROP TABLE IF EXISTS %s`, p.Table),
		fmt.Sprintf(`CREATE TABLE %s (id text PRIMARY KEY, emb vector(%d) NOT NULL)`, p.Table, p.Dim),
	} {
		if _, err := p.DB.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// Insert stores the vectors and returns how many it stored. Zero vectors (a
// text with no known word) are skipped: cosine distance to them is
// undefined, and the in-memory Dense ranker leaves them out too, so both
// rank the same set.
func (p *PGVector) Insert(ctx context.Context, ids []string, vecs [][]float32) (int, error) {
	if err := p.check(); err != nil {
		return 0, err
	}
	if len(ids) != len(vecs) {
		return 0, fmt.Errorf("pgvector: %d ids, %d vectors", len(ids), len(vecs))
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	st, err := tx.PrepareContext(ctx, fmt.Sprintf(`INSERT INTO %s (id, emb) VALUES ($1, $2::vector)`, p.Table))
	if err != nil {
		return 0, err
	}
	n := 0
	for i, id := range ids {
		if len(vecs[i]) != p.Dim {
			return 0, fmt.Errorf("pgvector: %s has %d dimensions, table has %d", id, len(vecs[i]), p.Dim)
		}
		if isZero(vecs[i]) {
			continue
		}
		if _, err := st.ExecContext(ctx, id, vecLiteral(vecs[i])); err != nil {
			return 0, err
		}
		n++
	}
	if err := st.Close(); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Index builds the HNSW index (m = 16, ef_construction = 64, pgvector defaults).
func (p *PGVector) Index(ctx context.Context) error {
	_, err := p.DB.ExecContext(ctx, fmt.Sprintf(`CREATE INDEX ON %s USING hnsw (emb vector_cosine_ops)`, p.Table))
	if err == nil {
		_, err = p.DB.ExecContext(ctx, fmt.Sprintf(`ANALYZE %s`, p.Table))
	}
	return err
}

// Search returns the k nearest ids by cosine similarity. A zero query (no
// known word) returns no hits, as Dense.Rank does.
func (p *PGVector) Search(ctx context.Context, q []float32, k int) ([]Hit, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if isZero(q) || k <= 0 {
		return nil, nil
	}
	rows, err := p.DB.QueryContext(ctx, fmt.Sprintf(`SELECT id, 1 - (emb <=> $1::vector) FROM %s ORDER BY emb <=> $1::vector LIMIT %d`, p.Table, k), vecLiteral(q))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hs []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.Score); err != nil {
			return nil, err
		}
		hs = append(hs, h)
	}
	return hs, rows.Err()
}
