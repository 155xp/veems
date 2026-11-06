package main

import (
	"os"
	"strings"
	"testing"
)

// The test binary stands in for veems when it starts supervisors and pool
// fillers, so it has to answer those hidden commands itself.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "_") {
		os.Exit(run(os.Args[1], os.Args[2:]))
	}
	os.Exit(m.Run())
}
