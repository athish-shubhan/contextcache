package memory

import "time"

type Score struct {
	Store   string            `json:"store"`
	ByKind  map[QKind]float64 `json:"accuracy_by_kind"`
	Overall float64           `json:"accuracy"`
	N       map[QKind]int     `json:"queries_by_kind"`
	Wrong   []map[string]any  `json:"examples_wrong,omitempty"`
}

// Run ingests the dataset into s in record order and scores every query.
func Run(ds Dataset, s Store) (Score, error) {
	ev := append([]Event(nil), ds.Events...)
	sortByRecorded(ev)
	for _, e := range ev {
		if err := s.Apply(e); err != nil {
			return Score{}, err
		}
	}
	sc := Score{Store: s.Name(), ByKind: map[QKind]float64{}, N: map[QKind]int{}}
	ok := map[QKind]int{}
	tot := 0
	for _, q := range ds.Queries {
		a, err := s.Answer(q, ds.Now)
		if err != nil {
			return sc, err
		}
		sc.N[q.Kind]++
		if a == q.Answer {
			ok[q.Kind]++
			tot++
		} else if len(sc.Wrong) < 5 {
			sc.Wrong = append(sc.Wrong, map[string]any{"query": q.Text, "kind": q.Kind, "want": q.Answer, "got": a})
		}
	}
	for k, n := range sc.N {
		sc.ByKind[k] = float64(ok[k]) / float64(n)
	}
	sc.Overall = float64(tot) / float64(len(ds.Queries))
	return sc, nil
}

func sortByRecorded(ev []Event) {
	// insertion order is per user; merge into one global record order
	type kv struct {
		t time.Time
		i int
	}
	idx := make([]kv, len(ev))
	for i, e := range ev {
		idx[i] = kv{e.Recorded, i}
	}
	sortKV(idx, func(a, b kv) bool { return a.t.Before(b.t) || a.t.Equal(b.t) && a.i < b.i })
	out := make([]Event, len(ev))
	for i, x := range idx {
		out[i] = ev[x.i]
	}
	copy(ev, out)
}
