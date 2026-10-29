package memory

import (
	"sort"
	"time"

	"contextcache/retrieve"
)

// Store is a memory system under test: it ingests events in record order
// and answers queries. Events carry structured payloads, so no store has to
// extract facts from text; this isolates storage and retrieval semantics
// from extraction and reading errors (no LLM is involved).
type Store interface {
	Name() string
	Apply(e Event) error
	Answer(q Query, now time.Time) (string, error)
}

func times(q Query, now time.Time) (vt, tx time.Time) {
	switch q.Kind {
	case QCurrent, QDeleted:
		return now, now
	case QAsOf:
		return q.At, now
	default:
		return q.At, q.At
	}
}

// ---- bi-temporal store (in memory; the Postgres one in pg.go is the same model in SQL)

type row struct {
	value      string
	validFrom  time.Time
	validTo    time.Time // zero = open
	recorded   time.Time
	superseded time.Time // zero = current belief
}

type BiTemporal struct {
	rows map[string][]row // user \x00 attr
}

func NewBiTemporal() *BiTemporal { return &BiTemporal{rows: map[string][]row{}} }

func (b *BiTemporal) Name() string { return "bi-temporal" }

func (b *BiTemporal) Apply(e Event) error {
	if e.Op == OpChatter {
		return nil
	}
	k := e.User + "\x00" + e.Attr
	cut := e.ValidFrom
	if e.Op == OpDelete {
		cut = e.Recorded
	}
	rs := b.rows[k]
	n := len(rs)
	for i := 0; i < n; i++ {
		r := &rs[i]
		if !r.superseded.IsZero() || (!r.validTo.IsZero() && !r.validTo.After(cut)) {
			continue // not current, or ends before the change
		}
		r.superseded = e.Recorded
		if r.validFrom.Before(cut) { // keep the part before the change as a new version
			rs = append(rs, row{value: r.value, validFrom: r.validFrom, validTo: cut, recorded: e.Recorded})
			r = &rs[i]
		}
	}
	if e.Op != OpDelete {
		rs = append(rs, row{value: e.Value, validFrom: e.ValidFrom, recorded: e.Recorded})
	}
	b.rows[k] = rs
	return nil
}

func (b *BiTemporal) Answer(q Query, now time.Time) (string, error) {
	vt, tx := times(q, now)
	best := None
	var bestRec time.Time
	for _, r := range b.rows[q.User+"\x00"+q.Attr] {
		if r.recorded.After(tx) || (!r.superseded.IsZero() && !r.superseded.After(tx)) {
			continue
		}
		if vt.Before(r.validFrom) || (!r.validTo.IsZero() && !vt.Before(r.validTo)) {
			continue
		}
		if best == None || r.recorded.After(bestRec) {
			best, bestRec = r.value, r.recorded
		}
	}
	return best, nil
}

// ---- latest-value key-value store: what most "user preference" memories are

type KVLatest struct{ m map[string]string }

func NewKVLatest() *KVLatest     { return &KVLatest{m: map[string]string{}} }
func (k *KVLatest) Name() string { return "key-value, latest only" }
func (k *KVLatest) Apply(e Event) error {
	switch e.Op {
	case OpSet, OpCorrect:
		k.m[e.User+"\x00"+e.Attr] = e.Value
	case OpDelete:
		delete(k.m, e.User+"\x00"+e.Attr)
	}
	return nil
}
func (k *KVLatest) Answer(q Query, _ time.Time) (string, error) {
	if v, ok := k.m[q.User+"\x00"+q.Attr]; ok {
		return v, nil
	}
	return None, nil
}

// ---- append-only log with timestamps (record time used as valid time)

type logEntry struct {
	at    time.Time
	value string
}

type AppendLog struct{ m map[string][]logEntry }

func NewAppendLog() *AppendLog    { return &AppendLog{m: map[string][]logEntry{}} }
func (l *AppendLog) Name() string { return "timestamped log (no valid time)" }
func (l *AppendLog) Apply(e Event) error {
	switch e.Op {
	case OpSet, OpCorrect:
		l.m[e.User+"\x00"+e.Attr] = append(l.m[e.User+"\x00"+e.Attr], logEntry{e.Recorded, e.Value})
	case OpDelete:
		l.m[e.User+"\x00"+e.Attr] = append(l.m[e.User+"\x00"+e.Attr], logEntry{e.Recorded, None})
	}
	return nil
}
func (l *AppendLog) Answer(q Query, now time.Time) (string, error) {
	vt, _ := times(q, now)
	v := None
	for _, x := range l.m[q.User+"\x00"+q.Attr] {
		if !x.at.After(vt) {
			v = x.value
		}
	}
	return v, nil
}

// ---- similarity retrieval over memory texts (a vector memory without
// temporal fields; BM25 stands in for embeddings, which are not available
// offline). Scoped to the user, as any multi-tenant memory would be.

type Similarity struct {
	Recency bool               // pick the most recent of the top 5 instead of the top 1
	Embed   *retrieve.Embedder // dense vectors (GloVe SIF) instead of BM25
	byUser  map[string][]Event
	index   map[string]retrieve.Ranker
	cache   map[string][]float32
}

// NewSimilarityDense is the same baseline with dense retrieval.
func NewSimilarityDense(e *retrieve.Embedder, recency bool) *Similarity {
	s := NewSimilarity(recency)
	s.Embed, s.cache = e, map[string][]float32{}
	return s
}

func NewSimilarity(recency bool) *Similarity {
	return &Similarity{Recency: recency, byUser: map[string][]Event{}, index: map[string]retrieve.Ranker{}}
}

func (s *Similarity) Name() string {
	kind := "similarity"
	if s.Embed != nil {
		kind = "dense similarity (GloVe SIF)"
	}
	if s.Recency {
		return kind + ", most recent of top 5"
	}
	return kind + ", top 1"
}

func (s *Similarity) Apply(e Event) error {
	s.byUser[e.User] = append(s.byUser[e.User], e)
	delete(s.index, e.User)
	return nil
}

func (s *Similarity) Answer(q Query, _ time.Time) (string, error) {
	ev := s.byUser[q.User]
	idx := s.index[q.User]
	if idx == nil {
		docs := make([]retrieve.Doc, len(ev))
		for i, e := range ev {
			docs[i] = retrieve.Doc{ID: itoa(i), Text: e.Text}
		}
		if s.Embed != nil {
			for i := range docs {
				docs[i].ID = itoa(i)
			}
			idx = retrieve.NewDense(s.Embed, docs, nil)
		} else {
			idx = retrieve.NewBM25(docs)
		}
		s.index[q.User] = idx
	}
	k := 1
	if s.Recency {
		k = 5
	}
	hits := idx.Rank(q.Text, k)
	if len(hits) == 0 {
		return None, nil
	}
	pick := hits[0]
	if s.Recency {
		sort.Slice(hits, func(i, j int) bool { return atoi(hits[i].ID) > atoi(hits[j].ID) })
		pick = hits[0]
	}
	e := ev[atoi(pick.ID)]
	if e.Op == OpSet || e.Op == OpCorrect {
		return e.Value, nil
	}
	return None, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
