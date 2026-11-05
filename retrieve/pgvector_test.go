package retrieve

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// Runs against Postgres with pgvector when CONTEXTCACHE_TEST_DSN is set, e.g.
// postgres://postgres@127.0.0.1:5432/contextcache?sslmode=disable
func TestPGVector(t *testing.T) {
	dsn := os.Getenv("CONTEXTCACHE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTEXTCACHE_TEST_DSN not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	p := &PGVector{DB: db, Table: "test_tool_vectors", Dim: 3}
	if err := p.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := p.Insert(ctx, []string{"east", "north", "northeast", "nothing"},
		[][]float32{{1, 0, 0}, {0, 1, 0}, {0.7071, 0.7071, 0}, {0, 0, 0}})
	if err != nil || n != 3 {
		t.Fatalf("stored %d (want 3: the zero vector is skipped): %v", n, err)
	}
	if err := p.Index(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := p.Search(ctx, []float32{0.9, 0.1, 0}, 2)
	if err != nil || len(h) != 2 || h[0].ID != "east" || h[1].ID != "northeast" {
		t.Fatalf("hits %v %v", h, err)
	}
	if h[0].Score < 0.99 || h[0].Score > 1 {
		t.Fatalf("cosine similarity %v", h[0].Score)
	}
	if h, err := p.Search(ctx, []float32{0, 0, 0}, 5); err != nil || len(h) != 0 {
		t.Fatalf("a zero query must return nothing: %v %v", h, err)
	}
	if _, err := p.Insert(ctx, []string{"bad"}, [][]float32{{1, 2}}); err == nil {
		t.Fatal("a vector of the wrong dimension must be rejected")
	}
	if _, err := db.Exec(`DROP TABLE test_tool_vectors`); err != nil {
		t.Fatal(err)
	}
}

func TestPGVectorRejectsUnsafeTableName(t *testing.T) {
	for _, name := range []string{"", "tools; DROP TABLE x", "1abc", "a-b"} {
		p := &PGVector{Table: name, Dim: 3}
		if err := p.Reset(context.Background()); err == nil {
			t.Errorf("table %q accepted", name)
		}
		if _, err := p.Search(context.Background(), []float32{1, 0, 0}, 1); err == nil {
			t.Errorf("table %q accepted by Search", name)
		}
	}
}
