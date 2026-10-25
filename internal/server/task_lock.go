package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// stuckTaskLockThreshold is how long a scheduled task's advisory lock may be
// held before a skipped invocation reports it as stuck instead of as a normal
// "already_running" skip. It is twice the longest budget any run path gives a
// task: the Lambda timeout is capped at 900s (terraform/modules/compute/aws/
// lambda variables.tf), the /api/scheduled/ HTTP path stops at
// scheduledTaskHTTPTimeout (300s), and the --task CLI defaults to 15 minutes
// (TASK_TIMEOUT). A CLI run deliberately given a TASK_TIMEOUT above this makes
// overlapping invocations report the lock as stuck while it runs, which errs
// toward visibility.
const stuckTaskLockThreshold = 30 * time.Minute

// ErrTaskLockStuck is returned by HandleScheduledTask when the advisory lock
// for the task has been held past stuckTaskLockThreshold. Returning an error
// (rather than the 200 "already_running" skip) makes a wedged lock visible:
// the /api/scheduled/ HTTP path answers 500, the Lambda invocation fails and
// counts toward the function's Errors metric, and the --task CLI exits
// non-zero (issue #105's third ask, tracked in #394).
var ErrTaskLockStuck = errors.New("scheduled task lock held longer than any run can last")

// stuckTaskLockError returns a wrapped ErrTaskLockStuck when the advisory lock
// that just caused a skip has been held past stuckTaskLockThreshold, and nil
// otherwise.
//
// A failure to read the holder's age is logged and treated as "not stuck": the
// skip itself is still correct, and failing the invocation on a diagnostics
// query would make the Lambda async invoke retry for no benefit. A lock
// released (or re-acquired by a fresh run) between the failed TryAdvisoryLock
// and this check reports not-found or a short age, so it is also a normal skip.
func stuckTaskLockError(ctx context.Context, locker TaskLocker, taskType ScheduledTaskType, lockID int64) error {
	heldFor, found, err := locker.AdvisoryLockHeldFor(ctx, lockID)
	if err != nil {
		log.Printf("Could not check how long the %q task lock has been held: %v", taskType, err) // #nosec G706 -- taskType is looked up from a known-value map; %q quotes the value to prevent CR/LF log injection
		return nil
	}
	if !found || heldFor <= stuckTaskLockThreshold {
		return nil
	}
	return fmt.Errorf("%w: task %q lock held for %s (threshold %s)",
		ErrTaskLockStuck, taskType, heldFor.Round(time.Second), stuckTaskLockThreshold)
}
