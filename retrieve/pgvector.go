package retrieve

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// PGVector keeps embeddings in Postgres with the pgvector extension and an
// HNSW index on cosine distance: the shape of a production tool or memory
// index, where the catalog lives next to the rest of the gateway's state.
type PGVector struct {
	DB    *sql.DB
	Table string
	Dim   int
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

func (p *PGVector) Insert(ctx context.Context, ids []string, vecs [][]float32) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, fmt.Sprintf(`INSERT INTO %s (id, emb) VALUES ($1, $2::vector)`, p.Table))
	if err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := st.ExecContext(ctx, id, vecLiteral(vecs[i])); err != nil {
			return err
		}
	}
	st.Close()
	return tx.Commit()
}

// Index builds the HNSW index (m = 16, ef_construction = 64, pgvector defaults).
func (p *PGVector) Index(ctx context.Context) error {
	_, err := p.DB.ExecContext(ctx, fmt.Sprintf(`CREATE INDEX ON %s USING hnsw (emb vector_cosine_ops)`, p.Table))
	if err == nil {
		_, err = p.DB.ExecContext(ctx, fmt.Sprintf(`ANALYZE %s`, p.Table))
	}
	return err
}

// Search returns the k nearest ids by cosine similarity.
func (p *PGVector) Search(ctx context.Context, q []float32, k int) ([]Hit, error) {
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
