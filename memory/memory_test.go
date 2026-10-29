package memory

import (
	"testing"
	"time"
)

func d(day int) time.Time { return time.Date(2025, 1, day, 12, 0, 0, 0, time.UTC) }

func TestReferenceSemantics(t *testing.T) {
	ev := []Event{
		{ID: 1, User: "u", Op: OpSet, Attr: "a", Value: "x", ValidFrom: d(1), Recorded: d(1)},
		{ID: 2, User: "u", Op: OpSet, Attr: "a", Value: "y", ValidFrom: d(10), Recorded: d(10)},
		{ID: 3, User: "u", Op: OpCorrect, Attr: "a", Value: "z", ValidFrom: d(5), Recorded: d(20)}, // since day 5 it was z
		{ID: 4, User: "u", Op: OpDelete, Attr: "a", ValidFrom: d(25), Recorded: d(25)},
	}
	r := Replay(ev, "u")
	cases := []struct {
		vt, tx time.Time
		want   string
	}{
		{d(3), d(30), "x"},   // before the corrected range
		{d(7), d(30), "z"},   // corrected
		{d(12), d(30), "z"},  // the day-10 update is overridden by the correction
		{d(12), d(15), "y"},  // as recorded on day 15: correction not yet known
		{d(26), d(30), None}, // deleted
		{d(22), d(30), "z"},  // before the delete
		{d(30), d(24), "z"},  // as recorded on day 24 (before the delete), asking about day 30
	}
	for _, c := range cases {
		if got := r.AsOf("a", c.vt, c.tx); got != c.want {
			t.Errorf("AsOf(vt=%d, tx=%d) = %s, want %s", c.vt.Day(), c.tx.Day(), got, c.want)
		}
	}
	// the bi-temporal store must agree with the reference everywhere
	b := NewBiTemporal()
	for _, e := range ev {
		b.Apply(e)
	}
	for _, c := range cases {
		q := Query{User: "u", Attr: "a", Kind: QRecorded, At: c.vt}
		if !c.vt.Equal(c.tx) {
			// emulate (vt, tx) through the store's query kinds where possible
			if c.tx.Equal(d(30)) {
				q.Kind = QAsOf
			} else {
				continue
			}
		}
		if got, _ := b.Answer(q, d(30)); got != c.want {
			t.Errorf("store vt=%d tx=%d: %s want %s", c.vt.Day(), c.tx.Day(), got, c.want)
		}
	}
}

func TestBiTemporalMatchesReferenceOnGeneratedData(t *testing.T) {
	ds := Generate(7, 5, 400, 200)
	sc, err := Run(ds, NewBiTemporal())
	if err != nil {
		t.Fatal(err)
	}
	if sc.Overall != 1 {
		t.Fatalf("bi-temporal store disagrees with the reference: %+v", sc)
	}
	for _, s := range []Store{NewKVLatest(), NewAppendLog(), NewSimilarity(false)} {
		sc, _ := Run(ds, s)
		if sc.Overall >= 1 {
			t.Fatalf("%s should not be perfect", s.Name())
		}
	}
}
