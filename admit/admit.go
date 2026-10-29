// Package admit decides which tools stay loaded in an agent's context
// between turns: a cache whose capacity is a token budget and whose items
// cost their schema's tokens. LRU and LFU are the obvious baselines;
// W-TinyLFU (Einziger et al., "TinyLFU: A Highly Efficient Cache Admission
// Policy", 2017; Caffeine's design) adds an admission filter so a tool used
// once does not push out a tool used every few turns.
package admit

import (
	"container/list"
	"hash/maphash"
)

type Cache interface {
	Name() string
	// Access records that a turn used id and updates the cache. It reports
	// whether id was loaded before this access.
	Access(id string, cost int) bool
	Contains(id string) bool
	Items() []string
	Used() int
}

// ---- LRU

type entry struct {
	id   string
	cost int
	freq int
}

type LRU struct {
	budget, used int
	ll           *list.List
	m            map[string]*list.Element
}

func NewLRU(budget int) *LRU {
	return &LRU{budget: budget, ll: list.New(), m: map[string]*list.Element{}}
}

func (c *LRU) Name() string { return "lru" }
func (c *LRU) Used() int    { return c.used }
func (c *LRU) Contains(id string) bool {
	_, ok := c.m[id]
	return ok
}
func (c *LRU) Items() []string { return items(c.ll) }

func items(ll *list.List) []string {
	var out []string
	for e := ll.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*entry).id)
	}
	return out
}

func (c *LRU) Access(id string, cost int) bool {
	if e, ok := c.m[id]; ok {
		c.ll.MoveToFront(e)
		return true
	}
	if cost > c.budget {
		return false
	}
	for c.used+cost > c.budget {
		b := c.ll.Back()
		v := b.Value.(*entry)
		c.ll.Remove(b)
		delete(c.m, v.id)
		c.used -= v.cost
	}
	c.m[id] = c.ll.PushFront(&entry{id: id, cost: cost})
	c.used += cost
	return false
}

// ---- LFU (exact counts with periodic halving so it can forget)

type LFU struct {
	budget, used int
	freq         map[string]int
	in           map[string]int // id -> cost
	ops, resetAt int
}

func NewLFU(budget int) *LFU {
	return &LFU{budget: budget, freq: map[string]int{}, in: map[string]int{}, resetAt: 5000}
}

func (c *LFU) Name() string { return "lfu" }
func (c *LFU) Used() int    { return c.used }
func (c *LFU) Contains(id string) bool {
	_, ok := c.in[id]
	return ok
}
func (c *LFU) Items() []string {
	var out []string
	for id := range c.in {
		out = append(out, id)
	}
	return out
}

func (c *LFU) Access(id string, cost int) bool {
	c.freq[id]++
	if c.ops++; c.ops >= c.resetAt {
		c.ops = 0
		for k, v := range c.freq {
			if v/2 == 0 {
				delete(c.freq, k)
			} else {
				c.freq[k] = v / 2
			}
		}
	}
	if _, ok := c.in[id]; ok {
		return true
	}
	if cost > c.budget {
		return false
	}
	for c.used+cost > c.budget {
		victim, vf := "", int(^uint(0)>>1)
		for k := range c.in {
			if f := c.freq[k]; f < vf || f == vf && k < victim {
				victim, vf = k, f
			}
		}
		c.used -= c.in[victim]
		delete(c.in, victim)
	}
	c.in[id] = cost
	c.used += cost
	return false
}

// ---- W-TinyLFU

// sketch is a count-min sketch of 4-bit-style saturating counters with a
// doorkeeper; it is halved every `sample` additions (the "reset" that lets
// TinyLFU forget old popularity).
type sketch struct {
	rows   [4][]uint8
	mask   uint64
	seeds  [4]maphash.Seed
	door   map[uint64]bool
	adds   int
	sample int
}

func newSketch(width, sample int) *sketch {
	w := 1
	for w < width {
		w <<= 1
	}
	s := &sketch{mask: uint64(w - 1), door: map[uint64]bool{}, sample: sample}
	for i := range s.rows {
		s.rows[i] = make([]uint8, w)
		s.seeds[i] = maphash.MakeSeed()
	}
	return s
}

func (s *sketch) add(id string) {
	h := maphash.String(s.seeds[0], id)
	if !s.door[h] {
		s.door[h] = true // first sighting only sets the doorkeeper
	} else {
		for i := range s.rows {
			j := maphash.String(s.seeds[i], id) & s.mask
			if s.rows[i][j] < 15 {
				s.rows[i][j]++
			}
		}
	}
	if s.adds++; s.adds >= s.sample {
		s.adds = 0
		s.door = map[uint64]bool{}
		for i := range s.rows {
			for j := range s.rows[i] {
				s.rows[i][j] >>= 1
			}
		}
	}
}

func (s *sketch) estimate(id string) int {
	m := 255
	for i := range s.rows {
		m = min(m, int(s.rows[i][maphash.String(s.seeds[i], id)&s.mask]))
	}
	if s.door[maphash.String(s.seeds[0], id)] {
		m++
	}
	return m
}

// TinyLFU: a small LRU window (1% of the budget, at least one item) in
// front of a main LRU segment; an item leaving the window enters the main
// segment only if the sketch says it is used more often than the items it
// would evict.
type TinyLFU struct {
	budget    int
	win, main *LRU
	sk        *sketch
	Rejected  int
}

func NewTinyLFU(budget, maxItems int) *TinyLFU {
	wb := max(budget/100, 1)
	return &TinyLFU{budget: budget, win: NewLRU(wb), main: NewLRU(budget - wb), sk: newSketch(16*maxItems, 10*maxItems)}
}

func (c *TinyLFU) Name() string            { return "w-tinylfu" }
func (c *TinyLFU) Used() int               { return c.win.used + c.main.used }
func (c *TinyLFU) Contains(id string) bool { return c.win.Contains(id) || c.main.Contains(id) }
func (c *TinyLFU) Items() []string         { return append(c.win.Items(), c.main.Items()...) }

func (c *TinyLFU) Access(id string, cost int) bool {
	c.sk.add(id)
	if c.main.Contains(id) {
		c.main.Access(id, cost)
		return true
	}
	if c.win.Contains(id) {
		c.win.Access(id, cost)
		return true
	}
	if cost > c.main.budget {
		return false
	}
	// new item goes through the window; whatever the window evicts is a
	// candidate for the main segment
	var cands []*entry
	if cost > c.win.budget {
		cands = append(cands, &entry{id: id, cost: cost})
	} else {
		for c.win.used+cost > c.win.budget {
			b := c.win.ll.Back()
			v := b.Value.(*entry)
			c.win.ll.Remove(b)
			delete(c.win.m, v.id)
			c.win.used -= v.cost
			cands = append(cands, v)
		}
		c.win.m[id] = c.win.ll.PushFront(&entry{id: id, cost: cost})
		c.win.used += cost
	}
	for _, cand := range cands {
		c.admit(cand)
	}
	return false
}

func (c *TinyLFU) admit(cand *entry) {
	m := c.main
	if m.used+cand.cost <= m.budget {
		m.m[cand.id] = m.ll.PushFront(cand)
		m.used += cand.cost
		return
	}
	// collect victims from the LRU end until the candidate fits; admit only
	// if the candidate is used more often than every one of them
	need := m.used + cand.cost - m.budget
	f := c.sk.estimate(cand.id)
	var victims []*list.Element
	freed := 0
	for e := m.ll.Back(); e != nil && freed < need; e = e.Prev() {
		v := e.Value.(*entry)
		if c.sk.estimate(v.id) >= f {
			c.Rejected++
			return
		}
		victims = append(victims, e)
		freed += v.cost
	}
	if freed < need {
		c.Rejected++
		return
	}
	for _, e := range victims {
		v := e.Value.(*entry)
		m.ll.Remove(e)
		delete(m.m, v.id)
		m.used -= v.cost
	}
	m.m[cand.id] = m.ll.PushFront(cand)
	m.used += cand.cost
}
