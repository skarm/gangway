// Command gangway exposes the small subset of the Docker Engine API that
// SPIRE's Docker workload attestor needs, over a private Unix socket, and
// strips every response field outside that subset.
package main

import (
	"os"

	"github.com/skarm/gangway/internal/gangway"
)

func main() {
	os.Exit(gangway.Main(os.Args[1:], os.Stdout, os.Stderr))
}
