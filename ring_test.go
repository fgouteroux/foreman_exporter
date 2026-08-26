package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNodeRoleCollector(t *testing.T) {
	tests := []struct {
		name     string
		isLeader bool
		err      error
		want     string
	}{
		{name: "leader", isLeader: true, want: "1"},
		{name: "follower", want: "2"},
		// An unresolvable ring must not silently look like a follower, otherwise
		// a cluster left without a leader would go unnoticed.
		{name: "unknown", err: errors.New("no healthy instance"), want: "0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := nodeRoleCollector{
				leaderFn: func() (bool, error) { return tc.isLeader, tc.err },
				logger:   testLogger(),
			}

			expected := `
# HELP foreman_exporter_node_role Node role, 0 = unknown, 1 = leader, 2 = follower
# TYPE foreman_exporter_node_role gauge
foreman_exporter_node_role ` + tc.want + `
`
			if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNodeRoleCollectorCountsLookupErrors(t *testing.T) {
	c := nodeRoleCollector{
		leaderFn: func() (bool, error) { return false, errors.New("boom") },
		logger:   testLogger(),
	}

	before := testutil.ToFloat64(ringLeaderLookupErrorsMetric)
	if n := testutil.CollectAndCount(c); n != 1 {
		t.Fatalf("collected %d series, want 1", n)
	}
	if got := testutil.ToFloat64(ringLeaderLookupErrorsMetric) - before; got != 1 {
		t.Fatalf("lookup errors delta = %v, want 1", got)
	}
}
