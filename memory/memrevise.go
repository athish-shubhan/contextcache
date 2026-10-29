// Package memory holds MemRevise, a benchmark for agent memory under
// revision, and the stores it compares.
//
// MemRevise generates, per user, a long history of network-operator
// preferences ("for eu-fra optimise cost over latency during auctions") in
// which facts are set, changed, corrected retroactively, deleted and
// sometimes re-set, mixed with chatter that mentions the same regions and
// words. Queries ask for current values, values as of a past date, values
// "as we had them on record" at a past date, and facts that were deleted and
// must not come back. Every answer is computed by an independent reference
// replay, so scoring is exact.
package memory

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"time"
)

const None = "NONE" // the answer for a fact that does not exist (never set, or deleted)

type Op string

const (
	OpSet     Op = "set"     // valid from the recorded time
	OpCorrect Op = "correct" // valid from an earlier date (retroactive), recorded now
	OpDelete  Op = "delete"  // no longer valid from the recorded time
	OpChatter Op = "chatter" // no fact
)

type Event struct {
	ID        int       `json:"id"`
	User      string    `json:"user"`
	Op        Op        `json:"op"`
	Attr      string    `json:"attr,omitempty"`
	Value     string    `json:"value,omitempty"`
	ValidFrom time.Time `json:"valid_from"`
	Recorded  time.Time `json:"recorded_at"`
	Text      string    `json:"text"`
}

type QKind string

const (
	QCurrent  QKind = "current"
	QDeleted  QKind = "deleted"     // current, and the fact was deleted
	QAsOf     QKind = "as_of"       // valid time in the past, as known now
	QRecorded QKind = "as_recorded" // what was on record at a past time
)

type Query struct {
	ID     int       `json:"id"`
	User   string    `json:"user"`
	Kind   QKind     `json:"kind"`
	Attr   string    `json:"attr"`
	At     time.Time `json:"at"` // valid time (and transaction time for as_recorded)
	Text   string    `json:"text"`
	Answer string    `json:"answer"`
}

type Dataset struct {
	Seed    uint64    `json:"seed"`
	Now     time.Time `json:"now"`
	Events  []Event   `json:"events"`
	Queries []Query   `json:"queries"`
}

var (
	regions  = []string{"apac-sg", "apac-syd", "apac-tokyo", "eu-fra", "eu-lon", "us-east", "us-west", "latam-sp"}
	events   = []string{"auctions", "battles", "normal traffic"}
	goals    = []string{"cost", "latency", "quality"}
	vendors  = []string{"vendor-a", "vendor-b", "vendor-c", "vendor-d"}
	channels = []string{"pager", "slack", "email", "sms"}
	windows  = []string{"Sun 02:00-04:00", "Sat 01:00-03:00", "Tue 03:00-05:00", "Wed 22:00-24:00"}
	bitrates = []string{"720p", "1080p", "1440p", "4k"}
)

type attrKind struct {
	key    func(r *rand.Rand) string
	value  func(r *rand.Rand) string
	phrase func(attr string) string
}

func attrKinds() []attrKind {
	return []attrKind{
		{func(r *rand.Rand) string {
			return "objective/" + regions[r.IntN(len(regions))] + "/" + events[r.IntN(len(events))]
		}, func(r *rand.Rand) string { return goals[r.IntN(len(goals))] },
			func(a string) string { p := split(a); return "optimisation goal for " + p[1] + " during " + p[2] }},
		{func(r *rand.Rand) string { return "primary_cdn/" + regions[r.IntN(len(regions))] },
			func(r *rand.Rand) string { return vendors[r.IntN(len(vendors))] },
			func(a string) string { return "primary CDN for " + split(a)[1] }},
		{func(r *rand.Rand) string { return "budget_cap/" + regions[r.IntN(len(regions))] },
			func(r *rand.Rand) string { return fmt.Sprint(100 + 50*r.IntN(99)) },
			func(a string) string { return "hourly budget cap in dollars for " + split(a)[1] }},
		{func(r *rand.Rand) string { return "alert_channel" },
			func(r *rand.Rand) string { return channels[r.IntN(len(channels))] },
			func(string) string { return "alert channel" }},
		{func(r *rand.Rand) string { return "maintenance_window/" + regions[r.IntN(len(regions))] },
			func(r *rand.Rand) string { return windows[r.IntN(len(windows))] },
			func(a string) string { return "maintenance window for " + split(a)[1] }},
		{func(r *rand.Rand) string { return "bitrate_ceiling/" + regions[r.IntN(len(regions))] },
			func(r *rand.Rand) string { return bitrates[r.IntN(len(bitrates))] },
			func(a string) string { return "bitrate ceiling for " + split(a)[1] }},
	}
}

func split(a string) []string {
	var out []string
	cur := ""
	for _, c := range a {
		if c == '/' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	return append(out, cur)
}

var kindIndex = map[string]int{"objective": 0, "primary_cdn": 1, "budget_cap": 2, "alert_channel": 3, "maintenance_window": 4, "bitrate_ceiling": 5}

func kindOf(attr string) attrKind { return attrKinds()[kindIndex[split(attr)[0]]] }

func phrase(attr string) string { return kindOf(attr).phrase(attr) }

func day(t time.Time) string { return t.Format("2 January 2006") }

func text(r *rand.Rand, e Event) string {
	ph := ""
	if e.Op != OpChatter {
		ph = phrase(e.Attr)
	}
	switch e.Op {
	case OpSet:
		t := []string{"From now on, set the %s to %s.", "Please use %[2]s as the %[1]s going forward.", "Update: the %s should be %s."}
		return fmt.Sprintf(t[r.IntN(len(t))], ph, e.Value)
	case OpCorrect:
		return fmt.Sprintf("Correction: since %s the %s has actually been %s.", day(e.ValidFrom), ph, e.Value)
	case OpDelete:
		t := []string{"Forget the %s, we no longer have one.", "Remove the %s from my preferences.", "Drop the %s setting entirely."}
		return fmt.Sprintf(t[r.IntN(len(t))], ph)
	}
	reg := regions[r.IntN(len(regions))]
	t := []string{"Auction traffic in %s was heavy today; the %s team asked about cost.",
		"We saw a latency spike in %s during the battle, but no change to the %s plan.",
		"Quality complaints from %s viewers again; let's discuss the budget and CDN for %s next week.",
		"Reminder that %s has a maintenance window coming up; check with the %s on-call."}
	return fmt.Sprintf(t[r.IntN(len(t))], reg, regions[r.IntN(len(regions))])
}

// Generate builds a dataset: users x eventsPerUser events over a year,
// then queriesPerUser queries per user asked at the end.
func Generate(seed uint64, users, eventsPerUser, queriesPerUser int) Dataset {
	r := rand.New(rand.NewPCG(seed, 11))
	start := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	now := start.AddDate(1, 0, 0)
	ds := Dataset{Seed: seed, Now: now}
	kinds := attrKinds()
	id := 0
	for u := 0; u < users; u++ {
		user := fmt.Sprintf("op%03d", u)
		// each user cares about a fixed set of 30 attributes
		attrs := map[string]int{}
		var keys []string
		for len(keys) < 30 {
			k := kinds[r.IntN(len(kinds))]
			a := k.key(r)
			if _, ok := attrs[a]; !ok {
				attrs[a] = len(keys)
				keys = append(keys, a)
			}
		}
		live := map[string]bool{}
		times := make([]time.Time, eventsPerUser)
		for i := range times {
			times[i] = start.Add(time.Duration(r.Int64N(int64(now.Sub(start) - 24*time.Hour))))
		}
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		for i := 0; i < eventsPerUser; i++ {
			t := times[i]
			e := Event{ID: id, User: user, Recorded: t, ValidFrom: t}
			id++
			x := r.Float64()
			a := keys[r.IntN(len(keys))]
			valueOf := func() string { return kindOf(a).value(r) }
			switch {
			case x < 0.55:
				e.Op = OpChatter
			case x < 0.85 || !live[a]:
				e.Op, e.Attr, e.Value = OpSet, a, valueOf()
				live[a] = true
			case x < 0.93:
				// retroactive correction: valid from 3 to 60 days before now
				back := time.Duration(3+r.IntN(58)) * 24 * time.Hour
				e.Op, e.Attr, e.Value = OpCorrect, a, valueOf()
				e.ValidFrom = t.Add(-back).Truncate(24 * time.Hour)
				live[a] = true
			default:
				e.Op, e.Attr = OpDelete, a
				live[a] = false
			}
			e.Text = text(r, e)
			ds.Events = append(ds.Events, e)
		}
		// queries
		ref := Replay(ds.Events, user)
		qid := 0
		for q := 0; q < queriesPerUser; q++ {
			a := keys[r.IntN(len(keys))]
			var k QKind
			x := r.Float64()
			at := now
			switch {
			case x < 0.35:
				k = QCurrent
			case x < 0.75:
				k = QAsOf
				at = start.Add(time.Duration(r.Int64N(int64(now.Sub(start))))).Truncate(24 * time.Hour).Add(12 * time.Hour)
			default:
				k = QRecorded
				at = start.Add(time.Duration(r.Int64N(int64(now.Sub(start))))).Truncate(24 * time.Hour).Add(12 * time.Hour)
			}
			ans := ref.AsOf(a, at, at)
			if k == QCurrent {
				ans = ref.AsOf(a, now, now)
				if ans == None && ref.EverSet(a, now) {
					k = QDeleted
				}
			} else if k == QAsOf {
				ans = ref.AsOf(a, at, now)
			}
			qq := Query{ID: u*1000 + qid, User: user, Kind: k, Attr: a, At: at, Answer: ans}
			switch k {
			case QCurrent, QDeleted:
				qq.Text = fmt.Sprintf("What is the current %s?", phrase(a))
			case QAsOf:
				qq.Text = fmt.Sprintf("What was the %s on %s?", phrase(a), day(at))
			case QRecorded:
				qq.Text = fmt.Sprintf("On %s, what did we have on record as the %s?", day(at), phrase(a))
			}
			ds.Queries = append(ds.Queries, qq)
			qid++
		}
	}
	return ds
}
