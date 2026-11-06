package l7

import (
	"cmp"
	"slices"
	"strings"
)

// Sort orders rules by Gateway API's HTTPRoute precedence, which Ingress's longest-match rule is a
// case of: the more specific host (an exact name, then the longest wildcard, then any host), an
// exact path, the longest prefix, a method, the most headers, the most query parameters, and on a
// tie the order the caller gave (route or Ingress age, then namespace/name, then rule order).
func Sort(rules []Rule) {
	slices.SortStableFunc(rules, func(a, b Rule) int {
		return cmp.Or(
			cmp.Compare(hostRank(b.Hosts), hostRank(a.Hosts)),
			cmp.Compare(pathRank(b.Paths), pathRank(a.Paths)),
			cmp.Compare(min(len(b.Methods), 1), min(len(a.Methods), 1)),
			cmp.Compare(len(b.Headers), len(a.Headers)),
			cmp.Compare(len(b.Query), len(a.Query)),
		)
	})
}

// hostRank: any exact name beats any wildcard, a longer wildcard beats a shorter one, and no host
// (any) comes last. splitHosts gives each rule hosts of one rank.
func hostRank(hosts []string) int {
	rank := 0
	for _, h := range hosts {
		r := 1<<16 + len(h)
		if !strings.HasPrefix(h, "*.") {
			r = 1 << 20
		}
		rank = max(rank, r)
	}
	return rank
}

// pathRank: an exact path beats any prefix, and a longer prefix a shorter one; no path is the
// prefix "/".
func pathRank(paths []PathMatch) int {
	rank := 1
	for _, p := range paths {
		r := len(p.Value)
		if p.Type == "exact" {
			r = 1 << 20
		}
		rank = max(rank, r)
	}
	return rank
}
