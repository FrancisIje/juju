package config

import "testing"

func TestRoles(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"ingest,worker,api", "ingest,worker,api", true},
		{"all", "ingest,worker,api", true},
		{" Worker ", "worker", true},
		{"ingest,api", "ingest,api", true},
		{"ingester", "", false},
		{"", "", false},
	} {
		r, err := ParseRoles(tc.in)
		if (err == nil) != tc.ok || (tc.ok && r.String() != tc.want) {
			t.Errorf("ParseRoles(%q) = %v, %v", tc.in, r, err)
		}
	}
}

// Splitting ingest from workers needs a queue both can reach.
func TestSplitRolesNeedASharedQueue(t *testing.T) {
	t.Setenv("JUJU_ROLES", "ingest,api")
	t.Setenv("JUJU_NATS_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("ingest without workers and no JUJU_NATS_URL should be refused")
	}
	t.Setenv("JUJU_NATS_URL", "nats://queue.internal:4222")
	if _, err := Load(); err != nil {
		t.Fatalf("with a shared queue: %v", err)
	}
	t.Setenv("JUJU_ROLES", "api") // no ingest, no worker: nothing to share
	t.Setenv("JUJU_NATS_URL", "")
	if _, err := Load(); err != nil {
		t.Fatalf("api only: %v", err)
	}
}
