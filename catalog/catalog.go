// Package catalog builds a tool universe and a set of needle queries from
// the Berkeley Function Calling Leaderboard (BFCL) data: every function
// definition from the simple, multiple, live_simple and live_multiple
// categories becomes a tool, and every question whose answer names one of
// its functions becomes a query with a known correct tool.
package catalog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Tool struct {
	ID          string          `json:"id"` // the function name (unique in the catalog)
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"` // JSON Schema (BFCL "dict" mapped to "object")
	Source      string          `json:"source"`       // BFCL category file
	Tokens      int             `json:"tokens"`       // cl100k tokens of the tool as sent to a model
}

type Query struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Tool     string `json:"tool"` // correct Tool.ID; empty for irrelevance questions
	Category string `json:"category"`
}

type Catalog struct {
	Tools   []Tool  `json:"tools"`
	Queries []Query `json:"queries"`
	byID    map[string]int
}

func (c *Catalog) Get(id string) *Tool {
	if c.byID == nil {
		c.byID = map[string]int{}
		for i, t := range c.Tools {
			c.byID[t.ID] = i
		}
	}
	if i, ok := c.byID[id]; ok {
		return &c.Tools[i]
	}
	return nil
}

// RetrievalText is what the retrievers index: name, description and the
// parameter names and descriptions.
func (t *Tool) RetrievalText() string {
	var b strings.Builder
	b.WriteString(t.Name)
	b.WriteString(" ")
	b.WriteString(t.Description)
	var s struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if json.Unmarshal(t.Schema, &s) == nil {
		keys := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" ")
			b.WriteString(k)
			b.WriteString(" ")
			b.WriteString(s.Properties[k].Description)
		}
	}
	return b.String()
}

// PromptJSON is the tool as it would appear in a model request
// ({name, description, input_schema}); its token count is the context cost.
func (t *Tool) PromptJSON() []byte {
	b, _ := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}{t.Name, t.Description, t.Schema})
	return b
}

type bfclFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type bfclItem struct {
	ID       string           `json:"id"`
	Question json.RawMessage  `json:"question"`
	Function []bfclFunc       `json:"function"`
	GT       []map[string]any `json:"ground_truth"`
}

// fixSchema maps BFCL's Python-flavoured types to JSON Schema.
func fixSchema(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	var walk func(x any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, vv := range t {
				if k == "type" {
					if s, ok := vv.(string); ok {
						switch s {
						case "dict":
							t[k] = "object"
						case "float":
							t[k] = "number"
						case "tuple", "list":
							t[k] = "array"
						case "any":
							delete(t, k)
						}
					}
					continue
				}
				t[k] = walk(vv)
			}
			return t
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return x
	}
	v = walk(v)
	m, ok := v.(map[string]any)
	if !ok {
		m = map[string]any{"type": "object"}
	}
	if _, ok := m["type"]; !ok {
		m["type"] = "object"
	}
	b, _ := json.Marshal(m)
	return b
}

func readLines(path string, f func([]byte) error) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		if err := f(sc.Bytes()); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return sc.Err()
}

// lastUser returns the last user message of a BFCL question (a list of
// turns, each a list of messages).
func lastUser(raw json.RawMessage) string {
	var turns [][]struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &turns) != nil {
		return ""
	}
	s := ""
	for _, t := range turns {
		for _, m := range t {
			if m.Role == "user" {
				s = m.Content
			}
		}
	}
	return s
}

// LoadBFCL reads BFCL data files (JSON lines) for the given categories from
// dataDir and their answers from dataDir/possible_answer.
func LoadBFCL(dataDir string, categories []string) (*Catalog, error) {
	c := &Catalog{}
	seen := map[string]bool{}
	for _, cat := range categories {
		// exact category match: "simple" must not pick up "live_simple"
		re := regexp.MustCompile(`^BFCL_v\d+_` + regexp.QuoteMeta(cat) + `\.json$`)
		var matches []string
		ents, _ := os.ReadDir(dataDir)
		for _, e := range ents {
			if re.MatchString(e.Name()) {
				matches = append(matches, filepath.Join(dataDir, e.Name()))
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no BFCL file for %q in %s", cat, dataDir)
		}
		sort.Strings(matches)
		path := matches[len(matches)-1] // newest version
		answers := map[string]string{}  // question id -> function name
		irrelevant := strings.Contains(cat, "irrelevance")
		ap := filepath.Join(dataDir, "possible_answer", filepath.Base(path))
		if !irrelevant {
			if err := readLines(ap, func(b []byte) error {
				var it bfclItem
				if err := json.Unmarshal(b, &it); err != nil {
					return err
				}
				if len(it.GT) == 1 {
					for name := range it.GT[0] {
						answers[it.ID] = name
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}
		}
		if err := readLines(path, func(b []byte) error {
			var it bfclItem
			if err := json.Unmarshal(b, &it); err != nil {
				return err
			}
			want := answers[it.ID]
			correct := ""
			for _, f := range it.Function {
				// BFCL repeats many APIs across questions with small wording or
				// schema differences (47 variants of get_current_weather). A real
				// deployment has one tool per name, so the catalog keeps the first
				// definition of each name and every question maps to it.
				id := f.Name
				if !seen[id] {
					seen[id] = true
					c.Tools = append(c.Tools, Tool{ID: id, Name: f.Name, Description: f.Description, Schema: fixSchema(f.Parameters), Source: cat})
				}
				if f.Name == want {
					correct = id
				}
			}
			if q := lastUser(it.Question); q != "" && (correct != "" || irrelevant) {
				// irrelevance questions have no correct tool: the right move is to load nothing
				c.Queries = append(c.Queries, Query{ID: it.ID, Text: q, Tool: correct, Category: cat})
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Catalog) ToolsLen() int { return len(c.Tools) }

func (c *Catalog) Save(path string) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	return &c, json.Unmarshal(b, &c)
}
