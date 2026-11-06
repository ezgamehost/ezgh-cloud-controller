package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUnknownSubcommandExits2(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}} {
		var out bytes.Buffer
		if code := dispatch(args, &out); code != 2 || !strings.Contains(out.String(), "autoscaler-provider|ccm|csr-approver|lb") {
			t.Fatalf("%v: exit %d, %q", args, code, out.String())
		}
	}
}
