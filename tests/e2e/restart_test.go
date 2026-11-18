//go:build !windows

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestKillAndRestartFinishesWithoutRerunningSucceededSteps kills the engine
// (SIGKILL: no drain) while a run is inside its second step, and starts
// another: the run finishes, and the step that had succeeded is not run again.
func TestKillAndRestartFinishesWithoutRerunningSucceededSteps(t *testing.T) {
	// a claim lives 2s without a heartbeat, so a killed engine's job is
	// released soon enough for the test
	e := newEnv(t, "FLO_ENGINE_CLAIM_TTL=2s")
	e.applyTested("slow-chain")
	e.ok("flow", "set", "slow-chain", "--state", "active", "--yes")

	first := e.startEngine()
	var c created
	e.okJSON(&c, "run", "create", "--flow", "slow-chain")
	eventually(t, "step two to start", func() bool {
		_, err := os.Stat(filepath.Join(e.scratch, "two.running"))
		return err == nil
	})
	first.kill()
	if got := e.show(c.Run).Status; got != "running" {
		t.Fatalf("after the kill the run is %q, want running", got)
	}
	time.Sleep(2500 * time.Millisecond) // past the claim's life

	second := e.startEngine()
	defer second.stop()
	eventuallyWithin(t, 40*time.Second, "the run to finish on the second engine", func() bool {
		return e.show(c.Run).Status == "succeeded"
	})
	count := map[string]int{}
	for _, l := range readLines(t, filepath.Join(e.scratch, "steps.log")) {
		count[l]++
	}
	if count["one"] != 1 || count["three"] != 1 || count["two"] < 1 {
		t.Errorf("step runs after the restart: %v (one and three exactly once, two at least once)", count)
	}
	v := e.show(c.Run)
	for _, id := range []string{"one", "two", "three"} {
		if status, _ := v.step(id); status != "succeeded" {
			t.Errorf("step %s: %s", id, status)
		}
	}
}
