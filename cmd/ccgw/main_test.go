package main

import (
	"strings"
	"testing"
)

func TestParseTenants(t *testing.T) {
	toks, allowed, err := parseTenants("tokA=acme:farm0, farm1;tokB=beta:farm2;")
	if err != nil {
		t.Fatal(err)
	}
	if toks["tokA"] != "acme" || toks["tokB"] != "beta" || len(toks) != 2 {
		t.Fatalf("tokens: %v", toks)
	}
	if strings.Join(allowed["acme"], ",") != "farm0,farm1" || strings.Join(allowed["beta"], ",") != "farm2" {
		t.Fatalf("upstreams: %v", allowed)
	}
	if _, ok := toks[""]; ok {
		t.Fatal("a trailing ';' must not create an empty token")
	}
	for _, bad := range []string{"", ";", "=acme:farm0", "tok=acme", "tok=:farm0", "tok=acme:", "tok=acme:,", "t=a:f;t=b:g"} {
		if _, _, err := parseTenants(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, spec := range []string{"secret-token=a:f;secret-token=b:g", "secret-token=a"} {
		if _, _, err := parseTenants(spec); err == nil || strings.Contains(err.Error(), "secret-token") {
			t.Errorf("error must not echo the token: %v", err)
		}
	}
}
