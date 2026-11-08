package memory

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// The Postgres store must answer every query exactly like the in-memory
// bi-temporal store (and the reference answer) on a generated dataset.
// Runs when CONTEXTCACHE_TEST_DSN points at a Postgres database.
func TestPGBiTemporalMatchesInMemory(t *testing.T) {
	dsn := os.Getenv("CONTEXTCACHE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTEXTCACHE_TEST_DSN not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	pg, err := NewPGBiTemporal(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	ds := Generate(7, 3, 400, 60)
	mem := NewBiTemporal()
	for _, e := range ds.Events {
		if err := pg.Apply(e); err != nil {
			t.Fatalf("pg apply %+v: %v", e, err)
		}
		if err := mem.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
	kinds := map[string]int{}
	for _, q := range ds.Queries {
		a, err := pg.Answer(q, ds.Now)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := mem.Answer(q, ds.Now)
		if a != b || a != q.Answer {
			t.Fatalf("query %d (%s %s): postgres %q, in-memory %q, reference %q", q.ID, q.Kind, q.Attr, a, b, q.Answer)
		}
		kinds[string(q.Kind)]++
	}
	if len(ds.Queries) != 180 || len(kinds) < 3 {
		t.Fatalf("%d queries, kinds %v", len(ds.Queries), kinds)
	}
	if _, err := db.Exec(`DROP TABLE memrevise_facts`); err != nil {
		t.Fatal(err)
	}
}
