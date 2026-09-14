package queue

import (
	"testing"
	"time"
)

// Go test conventions: file ends in _test.go, functions are TestXxx(t *testing.T),
// and there is no assertion library in the stdlib — you compare and call t.Errorf.
func TestBackoffGrowsExponentiallyAndIsCapped(t *testing.T) {
	const (
		base   = time.Second
		maxDur = 30 * time.Second
	)

	// Jitter is +/-20%, so assert on bounds rather than exact values.
	cases := []struct {
		attempt    int
		minD, maxD time.Duration
	}{
		{attempt: 1, minD: 800 * time.Millisecond, maxD: 1200 * time.Millisecond},
		{attempt: 2, minD: 1600 * time.Millisecond, maxD: 2400 * time.Millisecond},
		{attempt: 3, minD: 3200 * time.Millisecond, maxD: 4800 * time.Millisecond},
		{attempt: 20, minD: 0, maxD: maxDur},
	}

	for _, tc := range cases {
		got := Backoff(tc.attempt, base, maxDur)
		if got < tc.minD || got > tc.maxD {
			t.Errorf("Backoff(%d) = %s, want within [%s, %s]", tc.attempt, got, tc.minD, tc.maxD)
		}
	}
}

func TestBackoffNeverExceedsMax(t *testing.T) {
	for attempt := 1; attempt <= 64; attempt++ {
		if got := Backoff(attempt, 5*time.Second, time.Minute); got > time.Minute {
			t.Fatalf("Backoff(%d) = %s, exceeds max", attempt, got)
		}
	}
}

func TestDecodePayload(t *testing.T) {
	job := Job{Payload: []byte(`{"to":"a@b.com","subject":"hi"}`)}

	var p struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
	}
	if err := job.DecodePayload(&p); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if p.To != "a@b.com" || p.Subject != "hi" {
		t.Errorf("got %+v", p)
	}
}

func TestAttemptsRemaining(t *testing.T) {
	if got := (Job{Attempts: 1, MaxAttempts: 3}).AttemptsRemaining(); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
	if got := (Job{Attempts: 5, MaxAttempts: 3}).AttemptsRemaining(); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}
