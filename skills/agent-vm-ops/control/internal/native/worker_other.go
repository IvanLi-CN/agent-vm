//go:build !linux

package native

func workerIdentity(pid int) (WorkerIdentity, error) {
	return WorkerIdentity{PID: pid}, nil
}

func WorkerAlive(_ WorkerIdentity) (bool, error) { return false, nil }
func KillWorker(_ WorkerIdentity) error          { return nil }
