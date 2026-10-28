package native

import "context"

// WorkerIdentity is the durable identity of one native command. PID alone is
// never sufficient for recovery because it can be reused after a daemon
// restart; Linux also records the process start time from /proc.
type WorkerIdentity struct {
	PID          int    `json:"pid"`
	StartTime    string `json:"start_time"`
	ProcessGroup int    `json:"process_group,omitempty"`
}

// WorkerRecorder binds a native command to the operation journal. The
// callbacks are synchronous so a command is never considered owned until its
// identity has been durably recorded.
type WorkerRecorder interface {
	WorkerStarted(context.Context, string, WorkerIdentity) error
	WorkerFinished(context.Context, string, WorkerIdentity, error) error
}

// WorkerStartGuard can refuse a native start before the process exists when
// the operation already has unresolved worker ownership.
type WorkerStartGuard interface {
	WorkerStartAllowed(context.Context, string) error
}

// WorkerStartFailureRecorder clears a pre-spawn admission barrier after the
// runner has confirmed that no native process was created.
type WorkerStartFailureRecorder interface {
	WorkerStartFailed(context.Context, string, error) error
}

type workerContextKey struct{}
type workerRecoveryContextKey struct{}

type workerBinding struct {
	operationID string
	recorder    WorkerRecorder
}

func WithWorkerRecorder(ctx context.Context, operationID string, recorder WorkerRecorder) context.Context {
	if ctx == nil || operationID == "" || recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, workerContextKey{}, workerBinding{operationID: operationID, recorder: recorder})
}

// WithWorkerRecovery marks a native action that is being replayed by a
// currently active maintenance operation. The coordinator's unattempted
// journal barrier is intentionally ignored only for that nested recovery.
func WithWorkerRecovery(ctx context.Context, coordinatorOperationID string) context.Context {
	if ctx == nil || coordinatorOperationID == "" {
		return ctx
	}
	return context.WithValue(ctx, workerRecoveryContextKey{}, coordinatorOperationID)
}

func WorkerOperationID(ctx context.Context) string {
	return workerBindingFromContext(ctx).operationID
}

func WorkerRecoveryCoordinator(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	coordinator, _ := ctx.Value(workerRecoveryContextKey{}).(string)
	return coordinator
}

func workerBindingFromContext(ctx context.Context) workerBinding {
	if ctx == nil {
		return workerBinding{}
	}
	value, _ := ctx.Value(workerContextKey{}).(workerBinding)
	return value
}
