package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFixSchema(t *testing.T) {
	in := `{"type":"dict","properties":{
		"ratio":{"type":"float"},
		"pair":{"type":"tuple","items":{"type":"float"}},
		"tags":{"type":"list","items":{"type":"string"}},
		"anything":{"type":"any","description":"x"},
		"type":{"type":"dict","properties":{"n":{"type":"float"}}}
	},"required":["ratio"]}`
	got := decode(t, fixSchema(json.RawMessage(in)))
	want := decode(t, []byte(`{"type":"object","properties":{
		"ratio":{"type":"number"},
		"pair":{"type":"array","items":{"type":"number"}},
		"tags":{"type":"array","items":{"type":"string"}},
		"anything":{"description":"x"},
		"type":{"type":"object","properties":{"n":{"type":"number"}}}
	},"required":["ratio"]}`))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	for _, raw := range []string{`not json`, `[1,2]`, `{"properties":{}}`} {
		if m := decode(t, fixSchema(json.RawMessage(raw))); m["type"] != "object" {
			t.Fatalf("%s: every schema must end up an object, got %v", raw, m)
		}
	}
}

func TestRetrievalTextSortsParameters(t *testing.T) {
	tl := Tool{Name: "get_weather", Description: "Weather now.",
		Schema: json.RawMessage(`{"type":"object","properties":{"zip":{"description":"postal code"},"city":{"description":"city name"}}}`)}
	if got, want := tl.RetrievalText(), "get_weather Weather now. city city name zip postal code"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	var p map[string]any
	if err := json.Unmarshal(tl.PromptJSON(), &p); err != nil || p["name"] != "get_weather" || p["input_schema"] == nil {
		t.Fatalf("prompt JSON %v %v", p, err)
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func q(user string) string {
	return `[[{"role":"user","content":"ignored"},{"role":"assistant","content":"x"}],[{"role":"user","content":"` + user + `"}]]`
}

func TestLoadBFCL(t *testing.T) {
	d := t.TempDir()
	fn := func(name string) string {
		return `{"name":"` + name + `","description":"` + name + ` desc","parameters":{"type":"dict","properties":{"x":{"type":"float"}}}}`
	}
	// an older version of "simple" that must be ignored, the newest "simple",
	// "live_simple" that must not match "simple", and irrelevance questions
	writeLines(t, filepath.Join(d, "BFCL_v2_simple.json"), `{"id":"old_0","question":`+q("old")+`,"function":[`+fn("old_tool")+`]}`)
	writeLines(t, filepath.Join(d, "BFCL_v3_simple.json"),
		`{"id":"s_0","question":`+q("weather in Goa?")+`,"function":[`+fn("get_weather")+`]}`,
		``,
		`{"id":"s_1","question":`+q("weather again")+`,"function":[`+fn("get_weather")+`]}`,
		`{"id":"s_2","question":`+q("no answer recorded")+`,"function":[`+fn("orphan")+`]}`)
	writeLines(t, filepath.Join(d, "possible_answer", "BFCL_v3_simple.json"),
		`{"id":"s_0","ground_truth":[{"get_weather":{"x":[1.0]}}]}`,
		`{"id":"s_1","ground_truth":[{"get_weather":{"x":[2.0]}}]}`)
	writeLines(t, filepath.Join(d, "BFCL_v3_live_simple.json"), `{"id":"l_0","question":`+q("live")+`,"function":[`+fn("live_tool")+`]}`)
	writeLines(t, filepath.Join(d, "possible_answer", "BFCL_v3_live_simple.json"), `{"id":"l_0","ground_truth":[{"live_tool":{}}]}`)
	writeLines(t, filepath.Join(d, "BFCL_v3_irrelevance.json"), `{"id":"i_0","question":`+q("tell me a joke")+`,"function":[`+fn("get_weather")+`]}`)

	c, err := LoadBFCL(d, []string{"simple", "irrelevance"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tl := range c.Tools {
		ids = append(ids, tl.ID)
	}
	if !reflect.DeepEqual(ids, []string{"get_weather", "orphan"}) {
		t.Fatalf("tools %v: one per name, newest file only, no live_simple", ids)
	}
	if c.Get("get_weather").Source != "simple" || !strings.Contains(string(c.Get("get_weather").Schema), `"number"`) {
		t.Fatalf("tool %+v", c.Get("get_weather"))
	}
	want := []Query{
		{ID: "s_0", Text: "weather in Goa?", Tool: "get_weather", Category: "simple"},
		{ID: "s_1", Text: "weather again", Tool: "get_weather", Category: "simple"},
		{ID: "i_0", Text: "tell me a joke", Tool: "", Category: "irrelevance"},
	}
	if !reflect.DeepEqual(c.Queries, want) {
		t.Fatalf("queries %+v", c.Queries)
	}
	if c.Get("nope") != nil {
		t.Fatal("unknown id")
	}

	if _, err := LoadBFCL(d, []string{"multiple"}); err == nil {
		t.Fatal("a missing category must be an error")
	}

	path := filepath.Join(d, "catalog.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil || len(back.Tools) != 2 || len(back.Queries) != 3 || back.Get("orphan") == nil {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

// The committed catalog loads and is consistent: unique ids, every needle
// query points at a tool in the catalog.
func TestCommittedCatalog(t *testing.T) {
	c, err := Load("../data/catalog.json")
	if err != nil {
		t.Skip("data/catalog.json not present:", err)
	}
	seen := map[string]bool{}
	for _, tl := range c.Tools {
		if seen[tl.ID] {
			t.Fatalf("duplicate tool id %s", tl.ID)
		}
		seen[tl.ID] = true
	}
	needles := 0
	for _, qq := range c.Queries {
		if qq.Tool == "" {
			continue
		}
		needles++
		if !seen[qq.Tool] {
			t.Fatalf("query %s points at missing tool %s", qq.ID, qq.Tool)
		}
	}
	if len(c.Tools) != 1520 || needles != 1911 {
		t.Fatalf("catalog has %d tools and %d needle queries; the README reports 1,520 and 1,911", len(c.Tools), needles)
	}
}
