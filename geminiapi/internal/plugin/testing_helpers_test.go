package plugin

// Test-only helpers kept in their own file so the production build carries none
// of them.

import (
	"time"
)

// sleepMs sleeps for a duration expressed in milliseconds, so a test can write
// "8" instead of a magic duration literal.
func sleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// timeAfterMs is time.After with a millisecond argument.
func timeAfterMs(ms int) <-chan time.Time {
	return time.After(time.Duration(ms) * time.Millisecond)
}
