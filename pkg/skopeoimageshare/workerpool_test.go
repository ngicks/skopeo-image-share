package skopeoimageshare

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunPool_AllSucceed(t *testing.T) {
	t.Parallel()
	var ran atomic.Int32
	jobs := make([]Job, 5)
	for i := range jobs {
		jobs[i] = Job{
			ID:  "j",
			Run: func(ctx context.Context) error { ran.Add(1); return nil },
		}
	}
	res := RunPool(context.Background(), jobs, 3, RetryConfig{
		Retries:      0,
		InitialDelay: time.Millisecond,
		MaxDelay:     time.Millisecond,
	})
	if res.HasErrors() {
		t.Errorf("unexpected errors: %v", res.JobErrors)
	}
	if ran.Load() != 5 {
		t.Errorf("ran = %d, want 5", ran.Load())
	}
}

func TestRunPool_RetryUntilSuccess(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	job := Job{
		ID: "flaky",
		Run: func(ctx context.Context) error {
			n := attempts.Add(1)
			if n < 3 {
				return errors.New("transient")
			}
			return nil
		},
	}
	res := RunPool(context.Background(), []Job{job}, 1, RetryConfig{
		Retries:      5,
		InitialDelay: time.Millisecond,
		MaxDelay:     time.Millisecond,
	})
	if res.HasErrors() {
		t.Errorf("expected success, got %v", res.JobErrors)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
}

func TestRunPool_ExhaustedRetries(t *testing.T) {
	t.Parallel()
	job := Job{
		ID:  "doomed",
		Run: func(ctx context.Context) error { return errors.New("nope") },
	}
	res := RunPool(context.Background(), []Job{job}, 1, RetryConfig{
		Retries:      2,
		InitialDelay: time.Millisecond,
		MaxDelay:     time.Millisecond,
	})
	if !res.HasErrors() {
		t.Fatal("expected error")
	}
	if got := res.JobErrors["doomed"]; got == nil {
		t.Fatal("expected error for doomed")
	}
}

func TestRunPool_NonRetryableTerminates(t *testing.T) {
	t.Parallel()
	terminal := errors.New("terminal")
	var attempts atomic.Int32
	job := Job{
		ID: "term",
		Run: func(ctx context.Context) error {
			attempts.Add(1)
			return terminal
		},
	}
	res := RunPool(context.Background(), []Job{job}, 1, RetryConfig{
		Retries:      5,
		InitialDelay: time.Millisecond,
		MaxDelay:     time.Millisecond,
		IsRetryable: func(err error) bool {
			return !errors.Is(err, terminal)
		},
	})
	if !res.HasErrors() {
		t.Fatal("expected error")
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want 1 (terminal must not retry)", attempts.Load())
	}
}

func TestRunPool_ReconnectInvokedBetweenAttempts(t *testing.T) {
	t.Parallel()
	var (
		attempts  atomic.Int32
		reconnect atomic.Int32
	)
	job := Job{
		ID: "r",
		Run: func(ctx context.Context) error {
			n := attempts.Add(1)
			if n < 2 {
				return errors.New("net")
			}
			return nil
		},
	}
	res := RunPool(context.Background(), []Job{job}, 1, RetryConfig{
		Retries:      3,
		InitialDelay: time.Millisecond,
		MaxDelay:     time.Millisecond,
		Reconnect: func(ctx context.Context) error {
			reconnect.Add(1)
			return nil
		},
	})
	if res.HasErrors() {
		t.Fatalf("expected success, got %v", res.JobErrors)
	}
	if reconnect.Load() != 1 {
		t.Errorf("reconnect calls = %d, want 1 (one retry)", reconnect.Load())
	}
}

func TestRunPool_ContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	job := Job{
		ID: "loop",
		Run: func(ctx context.Context) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	}
	res := RunPool(ctx, []Job{job}, 1, RetryConfig{
		Retries:      3,
		InitialDelay: time.Millisecond,
	})
	if !res.HasErrors() {
		t.Fatal("expected ctx error")
	}
}

func TestNextDelay(t *testing.T) {
	t.Parallel()
	if got := nextDelay(time.Second, 5*time.Second); got != 2*time.Second {
		t.Errorf("got %v", got)
	}
	if got := nextDelay(3*time.Second, 5*time.Second); got != 5*time.Second {
		t.Errorf("got %v (cap)", got)
	}
}

func TestParseSSHTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		user string
		host string
		port int
	}{
		{"alice@host", "alice", "host", 0},
		{"alice@host:2222", "alice", "host", 2222},
		{"u@1.2.3.4:22", "u", "1.2.3.4", 22},
	}
	for _, tc := range cases {
		got, err := ParseSSHTarget(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got.User != tc.user || got.Host != tc.host || got.Port != tc.port {
			t.Errorf("%q: got %+v", tc.in, got)
		}
	}

	if _, err := ParseSSHTarget("nohost"); err == nil {
		t.Error("expected error for missing user@")
	}
	if _, err := ParseSSHTarget("u@host:bad"); err == nil {
		t.Error("expected error for bad port")
	}
}
