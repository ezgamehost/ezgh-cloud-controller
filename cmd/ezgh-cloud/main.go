// Command ezgh-cloud runs EZGH's in-cluster controllers on each control-plane VM, one subcommand
// per systemd unit (ezgh-cloud@<subcommand>.service), each leader-elected through its own Lease.
// They act on EZGH's public APIs as the cluster's Bot. The CSI controllers and the pod identity
// webhook, once subcommands here, are ezgamehost/ezgh-csi and ezgamehost/ezgh-pod-identity.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// subcommands: each file's init adds its own.
var subcommands = map[string]func(args []string) error{}

func main() { os.Exit(dispatch(os.Args[1:], os.Stderr)) }

func dispatch(args []string, stderr io.Writer) int {
	names := make([]string, 0, len(subcommands))
	for name := range subcommands {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(args) == 0 || subcommands[args[0]] == nil {
		fmt.Fprintf(stderr, "usage: ezgh-cloud <%s>\n", strings.Join(names, "|"))
		return 2
	}
	if err := subcommands[args[0]](args[1:]); err != nil {
		fmt.Fprintf(stderr, "ezgh-cloud %s: %v\n", args[0], err)
		return 1
	}
	return 0
}
