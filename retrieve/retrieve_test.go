package retrieve

import (
	"reflect"
	"testing"
)

func TestTokens(t *testing.T) {
	got := Tokens("getWeatherForecast(city_name) for the US-East region")
	want := []string{"get", "weather", "forecast", "city", "name", "us", "east", "region"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestRankersFindTheRightTool(t *testing.T) {
	docs := []Doc{
		{"weather.get_forecast", "get_forecast: weather forecast for a city over the next days"},
		{"finance.convert_currency", "convert_currency: convert an amount between currencies using exchange rates"},
		{"maps.route", "route: driving directions between two places"},
		{"math.factorial", "factorial: compute n factorial"},
	}
	for _, r := range []Ranker{NewBM25(docs), NewTrigram(docs), Hybrid(docs)} {
		if h := r.Rank("how many euros is 300 dollars at today's exchange rate", 1); len(h) == 0 || h[0].ID != "finance.convert_currency" {
			t.Fatalf("%s: %v", r.Name(), h)
		}
		if h := r.Rank("what's the forecasted weather in Sydney", 1); len(h) == 0 || h[0].ID != "weather.get_forecast" {
			t.Fatalf("%s: %v", r.Name(), h)
		}
	}
	// trigrams survive word-form changes BM25 misses
	if h := NewTrigram(docs).Rank("factorials", 1); len(h) == 0 || h[0].ID != "math.factorial" {
		t.Fatalf("trigram: %v", h)
	}
}
