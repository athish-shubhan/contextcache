package admit

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
)

func TestBudgetsHold(t *testing.T) {
	for _, c := range []Cache{NewLRU(1000), NewLFU(1000), NewTinyLFU(1000, 200)} {
		r := rand.New(rand.NewPCG(1, 2))
		for i := 0; i < 5000; i++ {
			c.Access(fmt.Sprint(r.IntN(200)), 50+r.IntN(100))
			if c.Used() > 1000 {
				t.Fatalf("%s over budget: %d", c.Name(), c.Used())
			}
		}
	}
}

// A hot set accessed every few turns plus a one-off scan: LRU flushes the
// hot set, TinyLFU keeps it.
func TestTinyLFUResistsScans(t *testing.T) {
	hit := map[string]int{}
	for _, c := range []Cache{NewLRU(1000), NewTinyLFU(1000, 1000)} {
		r := rand.New(rand.NewPCG(3, 4))
		h := 0
		for i := 0; i < 20000; i++ {
			var id string
			if r.Float64() < 0.5 {
				id = fmt.Sprint("hot", r.IntN(8)) // 8 hot tools x 100 tokens fit
			} else {
				id = fmt.Sprint("scan", i) // never seen again
			}
			if c.Access(id, 100) && id[0] == 'h' {
				h++
			}
		}
		hit[c.Name()] = h
	}
	if hit["w-tinylfu"] < hit["lru"]*3/2 {
		t.Fatalf("tinylfu %d vs lru %d hot hits", hit["w-tinylfu"], hit["lru"])
	}
}

func TestZipfOrdering(t *testing.T) {
	// on a stationary Zipf stream LFU-like policies beat LRU
	z := func(r *rand.Rand) int {
		// inverse-CDF sampling over 500 items, s=1.0
		u := r.Float64() * hn
		acc := 0.0
		for i := 1; i <= 500; i++ {
			acc += 1 / float64(i)
			if acc >= u {
				return i
			}
		}
		return 500
	}
	res := map[string]int{}
	for _, c := range []Cache{NewLRU(2000), NewLFU(2000), NewTinyLFU(2000, 500)} {
		r := rand.New(rand.NewPCG(5, 6))
		h := 0
		for i := 0; i < 30000; i++ {
			if c.Access(fmt.Sprint(z(r)), 100) {
				h++
			}
		}
		res[c.Name()] = h
	}
	if res["w-tinylfu"] <= res["lru"] || res["lfu"] <= res["lru"] {
		t.Fatalf("%v", res)
	}
}

var hn = func() float64 {
	s := 0.0
	for i := 1; i <= 500; i++ {
		s += 1 / float64(i)
	}
	return s
}()

var _ = math.Inf
