package app

import (
	"strings"
	"testing"
)

// A write a quiet routine could not hold names the switch that decides it first: the routine's
// own Writes (Ask first / Run without asking). It used to point only at the connection's Writes
// and at allow rules, which change every other turn in the channel too, and the person reading it
// went looking for the fix everywhere but the routine.
func TestAHeldRoutineWriteNamesTheRoutinesOwnSwitch(t *testing.T) {
	c := &Call{}
	got := c.silentRefusal("POST https://api.example.com/tasks")
	if !strings.Contains(got, "Run without asking") {
		t.Errorf("the refusal does not name the routine's own Writes switch: %s", got)
	}
	if len(c.silentHeld) != 1 {
		t.Errorf("the held write was not recorded for the run's report: %v", c.silentHeld)
	}
}
