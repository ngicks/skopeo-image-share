package skopeoimageshare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ngicks/go-common/contextkey"
)

// Default retry knobs used by [RunPool] when zero values are passed.
const (
	DefaultRetries      = 5
	DefaultInitialDelay = 1 * time.Second
	DefaultMaxDelay     = 30 * time.Second
)

// Job is one unit of work for [RunPool].
type Job struct {
	// ID is included in slog records; pick something human-readable
	// (e.g. the digest being transferred).
	ID string
	// Run does the work. It will be retried up to RetryConfig.Retries
	// times if it returns an error that the classifier considers
	// retryable. The ctx passed in is the per-attempt context.
	Run func(ctx context.Context) error
}

// RetryConfig controls retry/backoff behavior.
type RetryConfig struct {
	// Retries is the number of additional attempts after the first
	// failure (so total attempts = 1 + Retries). Zero = use
	// [DefaultRetries].
	Retries int
	// InitialDelay is the first backoff delay. Zero = use
	// [DefaultInitialDelay].
	InitialDelay time.Duration
	// MaxDelay caps the exponential backoff. Zero = use
	// [DefaultMaxDelay].
	MaxDelay time.Duration
	// IsRetryable classifies an error as retryable (network glitch,
	// peer hangup, etc). Nil means "every error is retryable".
	IsRetryable func(error) bool
	// Reconnect is invoked after a retryable failure, before the next
	// attempt. Use it to redial SSH/SFTP. Errors from Reconnect abort
	// the job (they are treated as terminal).
	Reconnect func(ctx context.Context) error
}

func (r RetryConfig) effectiveRetries() int {
	if r.Retries <= 0 {
		return DefaultRetries
	}
	return r.Retries
}

func (r RetryConfig) effectiveInitial() time.Duration {
	if r.InitialDelay <= 0 {
		return DefaultInitialDelay
	}
	return r.InitialDelay
}

func (r RetryConfig) effectiveMax() time.Duration {
	if r.MaxDelay <= 0 {
		return DefaultMaxDelay
	}
	return r.MaxDelay
}

// PoolResult summarizes a [RunPool] invocation.
type PoolResult struct {
	// JobErrors maps Job.ID to the final error for jobs that failed
	// past the retry budget. Successful jobs do not appear here.
	JobErrors map[string]error
}

// HasErrors reports whether any job failed.
func (r PoolResult) HasErrors() bool { return len(r.JobErrors) > 0 }

// JoinedError returns a single error joining all per-job failures, or
// nil when there are none.
func (r PoolResult) JoinedError() error {
	if len(r.JobErrors) == 0 {
		return nil
	}
	errs := make([]error, 0, len(r.JobErrors))
	for id, e := range r.JobErrors {
		errs = append(errs, fmt.Errorf("%s: %w", id, e))
	}
	return errors.Join(errs...)
}

// RunPool drives jobs over `parallelism` goroutines with per-job retry.
// The function returns when all jobs finished (success or terminal
// failure) or ctx is cancelled.
//
// Jobs are distributed via a buffered channel. There is no preemption
// of in-flight jobs across workers — once a worker picks up a job, it
// runs that job's full retry budget (if needed) before reaching for
// the next one.
func RunPool(ctx context.Context, jobs []Job, parallelism int, rc RetryConfig) PoolResult {
	if parallelism < 1 {
		parallelism = 1
	}
	logger := contextkey.ValueSlogLoggerDefault(ctx)

	jobCh := make(chan Job, len(jobs))
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	var (
		mu     sync.Mutex
		errMap = make(map[string]error)
		wg     sync.WaitGroup
	)

	for w := 0; w < parallelism; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := range jobCh {
				if err := runWithRetry(ctx, j, rc, logger, workerID); err != nil {
					mu.Lock()
					errMap[j.ID] = err
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()

	return PoolResult{JobErrors: errMap}
}

func runWithRetry(ctx context.Context, j Job, rc RetryConfig, logger *slog.Logger, workerID int) error {
	maxAttempts := 1 + rc.effectiveRetries()
	delay := rc.effectiveInitial()
	maxDelay := rc.effectiveMax()

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := j.Run(ctx)
		if err == nil {
			if attempt > 1 {
				logger.LogAttrs(ctx, slog.LevelInfo, "pool.job.recovered",
					slog.String("id", j.ID),
					slog.Int("attempts", attempt),
					slog.Int("worker", workerID),
				)
			}
			return nil
		}
		lastErr = err

		if rc.IsRetryable != nil && !rc.IsRetryable(err) {
			logger.LogAttrs(ctx, slog.LevelDebug, "pool.job.terminal",
				slog.String("id", j.ID),
				slog.Any("err", err),
			)
			return err
		}

		if attempt == maxAttempts {
			break
		}

		logger.LogAttrs(ctx, slog.LevelInfo, "pool.job.retry",
			slog.String("id", j.ID),
			slog.Int("attempt", attempt),
			slog.Int("maxAttempts", maxAttempts),
			slog.Duration("delay", delay),
			slog.Any("err", err),
		)

		if rc.Reconnect != nil {
			if reErr := rc.Reconnect(ctx); reErr != nil {
				return fmt.Errorf("pool.reconnect failed: %w", reErr)
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = nextDelay(delay, maxDelay)
	}

	return fmt.Errorf("job %s exhausted %d attempts: %w", j.ID, maxAttempts, lastErr)
}

func nextDelay(cur, maxD time.Duration) time.Duration {
	d := cur * 2
	if d <= 0 || d > maxD {
		return maxD
	}
	return d
}
