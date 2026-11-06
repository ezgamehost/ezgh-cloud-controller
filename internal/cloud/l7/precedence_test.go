package l7

import (
	"fmt"
	"strings"
	"testing"
)

func names(rules []Rule) string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Source)
	}
	return strings.Join(out, " ")
}

// The ordering examples of Gateway API's HTTPRoute (HTTPRouteRule, "Proxy or Load Balancer routing
// configuration… MUST prioritize matches"), hostnames first.
func TestSortGatewayPrecedence(t *testing.T) {
	exact := func(v string) []PathMatch { return []PathMatch{{Type: "exact", Value: v}} }
	prefix := func(v string) []PathMatch { return []PathMatch{{Type: "prefix", Value: v}} }
	h := []NameValue{{Name: "a", Value: "1"}}
	cases := []struct {
		name  string
		rules []Rule
		want  string
	}{
		{"exact path before any prefix", []Rule{{Source: "p", Paths: prefix("/foo/bar/baz")}, {Source: "e", Paths: exact("/foo")}}, "e p"},
		{"longest prefix first", []Rule{{Source: "short", Paths: prefix("/a")}, {Source: "long", Paths: prefix("/a/b")}, {Source: "none"}}, "long short none"},
		{"method before headers", []Rule{{Source: "h", Headers: h}, {Source: "m", Methods: []string{"GET"}}}, "m h"},
		{"most headers, then most query params", []Rule{
			{Source: "q1", Query: h}, {Source: "h1", Headers: h}, {Source: "h2", Headers: append(h, NameValue{Name: "b", Value: "2"})}, {Source: "q2h1", Headers: h, Query: h},
		}, "h2 q2h1 h1 q1"},
		{"exact host, longest wildcard, any", []Rule{{Source: "any"}, {Source: "w1", Hosts: []string{"*.example.com"}}, {Source: "w2", Hosts: []string{"*.foo.example.com"}}, {Source: "e", Hosts: []string{"a.example.com"}}}, "e w2 w1 any"},
		{"host beats path", []Rule{{Source: "path", Paths: exact("/x")}, {Source: "host", Hosts: []string{"a.example.com"}}}, "host path"},
		{"a tie keeps the caller's order", []Rule{{Source: "older", Paths: prefix("/a")}, {Source: "newer", Paths: prefix("/a")}}, "older newer"},
	}
	for _, c := range cases {
		Sort(c.rules)
		if got := names(c.rules); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// A rule with an exact and a wildcard host becomes one rule per specificity, so each sorts
// correctly; more than 5 hosts split into fives.
func TestSplitHosts(t *testing.T) {
	r := Rule{Source: "r", Hosts: []string{"*.example.com", "a.example.com", "b", "c", "d", "e", "f"}}
	got := splitHosts(r)
	if len(got) != 3 || fmt.Sprint(got[0].Hosts) != "[*.example.com]" || len(got[1].Hosts) != 5 || len(got[2].Hosts) != 1 {
		t.Fatal(got)
	}
}

func TestRuleLimit(t *testing.T) {
	m := Model{Listeners: []Listener{{Port: 80, Protocol: "http", Default: fixed(404)}}}
	for i := range 101 {
		m.Listeners[0].Rules = append(m.Listeners[0].Rules, Rule{Source: fmt.Sprintf("HTTPRoute default/r%d", i), Paths: []PathMatch{{Type: "prefix", Value: fmt.Sprintf("/%03d", i)}}, Action: fixed(200)})
	}
	refused := m.limit()
	if len(m.Listeners[0].Rules) != MaxRules || len(refused) != 1 || refused[0].Reason != RuleLimit || !strings.Contains(refused[0].Message, "HTTPRoute default/r") {
		t.Fatal(len(m.Listeners[0].Rules), refused)
	}
}

func TestListenerLimit(t *testing.T) {
	var m Model
	for p := range 11 {
		m.Listeners = append(m.Listeners, Listener{Port: 8000 + p, Protocol: "http", Default: fixed(404)})
	}
	refused := m.limit()
	if len(m.Listeners) != MaxListeners || len(refused) != 1 || !strings.Contains(refused[0].Message, "8010") {
		t.Fatal(refused)
	}
}
