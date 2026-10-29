package memory

import (
	"sort"
	"time"
)

// Reference is the ground truth: an independent replay of one user's events
// into per-attribute valid-time timelines, kept for every transaction time.
// It shares no code with the stores it scores.
type Reference struct {
	byAttr map[string][]Event // set/correct/delete events in record order
}

func Replay(all []Event, user string) *Reference {
	r := &Reference{byAttr: map[string][]Event{}}
	for _, e := range all {
		if e.User == user && e.Op != OpChatter {
			r.byAttr[e.Attr] = append(r.byAttr[e.Attr], e)
		}
	}
	for a := range r.byAttr {
		ev := r.byAttr[a]
		sort.SliceStable(ev, func(i, j int) bool { return ev[i].Recorded.Before(ev[j].Recorded) })
	}
	return r
}

type segment struct {
	from, to time.Time // to.IsZero() = open
	value    string
}

// timeline applies the events recorded up to tx in order.
func timeline(ev []Event, tx time.Time) []segment {
	var segs []segment
	for _, e := range ev {
		if e.Recorded.After(tx) {
			break
		}
		cut := e.ValidFrom
		if e.Op == OpDelete {
			cut = e.Recorded
		}
		var kept []segment
		for _, s := range segs {
			if !s.from.Before(cut) {
				continue // starts at or after the cut: overridden
			}
			if s.to.IsZero() || s.to.After(cut) {
				s.to = cut
			}
			kept = append(kept, s)
		}
		segs = kept
		if e.Op != OpDelete {
			segs = append(segs, segment{from: e.ValidFrom, value: e.Value})
		}
	}
	return segs
}

// AsOf answers "what was attr at valid time vt, according to everything
// recorded by tx".
func (r *Reference) AsOf(attr string, vt, tx time.Time) string {
	for _, s := range timeline(r.byAttr[attr], tx) {
		if !vt.Before(s.from) && (s.to.IsZero() || vt.Before(s.to)) {
			return s.value
		}
	}
	return None
}

// EverSet reports whether attr was ever given a value by tx.
func (r *Reference) EverSet(attr string, tx time.Time) bool {
	for _, e := range r.byAttr[attr] {
		if !e.Recorded.After(tx) && e.Op != OpDelete {
			return true
		}
	}
	return false
}
