//go:build !linux

package native

import "os/exec"

type processTracker struct{}

func newProcessTracker() (*processTracker, error) { return &processTracker{}, nil }
func startTrackedCommand(command *exec.Cmd, tracker *processTracker) (bool, error) {
	if err := command.Start(); err != nil {
		return false, err
	}
	tracker.Register(command.Process.Pid)
	return true, tracker.Discover(command.Process.Pid)
}
func (tracker *processTracker) Identity(root int) (WorkerIdentity, error) {
	return workerIdentity(root)
}
func (*processTracker) Register(_ int)        {}
func (*processTracker) Discover(_ int) error  { return nil }
func (*processTracker) Live() (int, error)    { return 0, nil }
func (*processTracker) Present() (int, error) { return 0, nil }
func (*processTracker) Kill() error           { return nil }
func (*processTracker) Reap() error           { return nil }
func (*processTracker) Close()                {}
