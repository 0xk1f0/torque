// ABOUTME: Small shared test helpers for the sendspin package
package sendspin

import (
	"testing"
	"time"
)

// waitFor polls cond until it returns true or the timeout elapses, failing
// the test with desc on timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}
