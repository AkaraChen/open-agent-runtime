package cli

import (
	"testing"
	"time"
)

// startVM polls the supervisor's state quickly at first and gently later: a
// fixed 100 ms step rounded every measured boot up to a multiple of 100 ms.
func TestVMPollInterval(t *testing.T) {
	for _, c := range []struct {
		waited, want time.Duration
	}{
		{0, 2 * time.Millisecond},
		{999 * time.Millisecond, 2 * time.Millisecond},
		{time.Second, 20 * time.Millisecond},
		{9999 * time.Millisecond, 20 * time.Millisecond},
		{10 * time.Second, 100 * time.Millisecond},
		{5 * time.Minute, 100 * time.Millisecond},
	} {
		if got := vmPollInterval(c.waited); got != c.want {
			t.Errorf("vmPollInterval(%v) = %v, want %v", c.waited, got, c.want)
		}
	}
}
