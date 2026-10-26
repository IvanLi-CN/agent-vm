package native

import "context"

// WorkerIdentity is the durable identity of one native command. PID alone is
// never sufficient for recovery because it can be reused after a daemon
// restart; Linux also records the process start time from /proc.
type WorkerIdentity struct {
	PID       int    `json:"pid"`
	StartTime string `json:"start_time"`
}

// WorkerRecorder binds a native command to the operation journal. The
// callbacks are synchronous so a command is never considered owned until its
// identity has been durably recorded.
type WorkerRecorder interface {
	WorkerStarted(context.Context, string, WorkerIdentity) error
	WorkerFinished(context.Context, string, WorkerIdentity, error) error
}

type workerContextKey struct{}

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

func workerBindingFromContext(ctx context.Context) workerBinding {
	if ctx == nil {
		return workerBinding{}
	}
	value, _ := ctx.Value(workerContextKey{}).(workerBinding)
	return value
}
