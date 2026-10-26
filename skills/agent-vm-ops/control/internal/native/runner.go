package native

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Result struct {
	Code            int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
}

type Runner struct {
	OutputLimit int
	StdoutLimit int
	StderrLimit int
}

func NewRunner() *Runner {
	return &Runner{OutputLimit: 64 * 1024, StdoutLimit: 2 * 1024 * 1024, StderrLimit: 64 * 1024}
}

func (r *Runner) Run(ctx context.Context, argv []string, input []byte) (result Result, runErr error) {
	if r == nil {
		r = NewRunner()
	}
	if len(argv) == 0 || argv[0] == "" {
		return Result{}, errors.New("invalid native command")
	}
	for _, arg := range argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return Result{}, errors.New("native command contains NUL")
		}
	}
	tracker, err := newProcessTracker()
	if err != nil {
		return Result{}, fmt.Errorf("prepare native process ownership: %w", err)
	}
	defer tracker.Close()
	command := exec.Command(argv[0], argv[1:]...)
	configureCommand(command)
	command.Stdin = bytes.NewReader(input)
	stdout := &limitedBuffer{limit: r.stdoutLimit()}
	stderr := &limitedBuffer{limit: r.stderrLimit()}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("prepare stdout pipe: %w", err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return Result{}, fmt.Errorf("prepare stderr pipe: %w", err)
	}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	started := time.Now()
	if err := command.Start(); err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		return Result{}, fmt.Errorf("start %s: %w", argv[0], err)
	}
	tracker.Register(command.Process.Pid)
	// The child owns the duplicated write ends. Closing the parent's copies is
	// required so successful commands can reach EOF, while a detached child
	// cannot keep the parent-side descriptor alive indefinitely.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	trackingErr := tracker.Discover(command.Process.Pid)
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go drain(stdoutReader, stdout, stdoutDone)
	go drain(stderrReader, stderr, stderrDone)
	pidfd := openPIDFD(command.Process.Pid)
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	binding := workerBindingFromContext(ctx)
	var identity WorkerIdentity
	if binding.recorder != nil {
		var identityErr error
		identity, identityErr = tracker.Identity(command.Process.Pid)
		if identityErr != nil {
			killProcess(command.Process.Pid, pidfd)
			_ = cleanupTracked(tracker, command.Process.Pid)
			_ = stdoutReader.Close()
			_ = stderrReader.Close()
			<-wait
			<-stdoutDone
			<-stderrDone
			closePIDFD(pidfd)
			_ = tracker.Reap()
			return Result{}, fmt.Errorf("identify native worker: %w", identityErr)
		}
	}
	workerStarted := false
	defer func() {
		if !workerStarted || binding.recorder == nil {
			return
		}
		if err := binding.recorder.WorkerFinished(ctx, binding.operationID, identity, runErr); err != nil {
			if runErr == nil {
				runErr = fmt.Errorf("record native worker completion: %w", err)
			} else {
				runErr = errors.Join(runErr, fmt.Errorf("record native worker completion: %w", err))
			}
		}
	}()
	if binding.recorder != nil {
		if err := binding.recorder.WorkerStarted(ctx, binding.operationID, identity); err != nil {
			killProcess(command.Process.Pid, pidfd)
			_ = cleanupTracked(tracker, command.Process.Pid)
			_ = stdoutReader.Close()
			_ = stderrReader.Close()
			<-wait
			<-stdoutDone
			<-stderrDone
			closePIDFD(pidfd)
			_ = tracker.Reap()
			return Result{}, fmt.Errorf("record native worker start: %w", err)
		}
		workerStarted = true
	}
	var waitErr error
	select {
	case waitErr = <-wait:
		if trackingErr == nil {
			trackingErr = tracker.Discover(command.Process.Pid)
		}
		if trackingErr == nil {
			if live, liveErr := tracker.Live(); liveErr != nil {
				trackingErr = liveErr
			} else if live > 0 {
				trackingErr = errors.New("native command left child processes behind")
			}
		}
		if trackingErr != nil {
			killProcess(command.Process.Pid, pidfd)
			if cleanupErr := cleanupTracked(tracker, command.Process.Pid); cleanupErr != nil {
				trackingErr = fmt.Errorf("%w: %v", trackingErr, cleanupErr)
			}
		}
		pipesDrained := waitForPipes(stdoutDone, stderrDone, time.Second)
		if !pipesDrained {
			_ = stdoutReader.Close()
			_ = stderrReader.Close()
		}
	case <-ctx.Done():
		if trackingErr == nil {
			trackingErr = tracker.Discover(command.Process.Pid)
		}
		killProcess(command.Process.Pid, pidfd)
		if cleanupErr := cleanupTracked(tracker, command.Process.Pid); trackingErr == nil && cleanupErr != nil {
			trackingErr = cleanupErr
		}
		waited := true
		select {
		case waitErr = <-wait:
		case <-time.After(2 * time.Second):
			// Closing the read ends breaks inherited-pipe waits even if a
			// detached descendant ignored the process-group signal.
			_ = stdoutReader.Close()
			_ = stderrReader.Close()
			killProcess(command.Process.Pid, pidfd)
			_ = cleanupTracked(tracker, command.Process.Pid)
			select {
			case waitErr = <-wait:
			case <-time.After(250 * time.Millisecond):
				waited = false
				waitErr = errors.New("native process did not exit after process-group cleanup")
			}
		}
		_ = stdoutReader.Close()
		_ = stderrReader.Close()
		<-stdoutDone
		<-stderrDone
		closePIDFD(pidfd)
		_ = tracker.Reap()
		if !waited {
			return Result{Code: exitCode(waitErr), Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), StdoutTruncated: stdout.Truncated, StderrTruncated: stderr.Truncated}, fmt.Errorf("native command timeout after %s: %w; %v", time.Since(started).Round(time.Millisecond), ctx.Err(), waitErr)
		}
		if trackingErr != nil {
			return Result{Code: exitCode(waitErr), Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), StdoutTruncated: stdout.Truncated, StderrTruncated: stderr.Truncated}, fmt.Errorf("native command timeout after %s: %w; cleanup: %v", time.Since(started).Round(time.Millisecond), ctx.Err(), trackingErr)
		}
		return Result{Code: exitCode(waitErr), Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), StdoutTruncated: stdout.Truncated, StderrTruncated: stderr.Truncated}, fmt.Errorf("native command timeout after %s: %w", time.Since(started).Round(time.Millisecond), ctx.Err())
	}
	closePIDFD(pidfd)
	_ = stdoutReader.Close()
	_ = stderrReader.Close()
	<-stdoutDone
	<-stderrDone
	if reapErr := tracker.Reap(); trackingErr == nil {
		trackingErr = reapErr
	}
	result = Result{Code: exitCode(waitErr), Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), StdoutTruncated: stdout.Truncated, StderrTruncated: stderr.Truncated}
	if trackingErr != nil {
		return result, trackingErr
	}
	if waitErr != nil {
		return result, fmt.Errorf("native command failed (%d): %s: %w", result.Code, argv[0], waitErr)
	}
	return result, nil
}

func drain(reader *os.File, buffer *limitedBuffer, done chan<- struct{}) {
	defer close(done)
	defer reader.Close()
	_, _ = io.Copy(buffer, reader)
}

func waitForPipes(stdoutDone, stderrDone <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for stdoutDone != nil || stderrDone != nil {
		select {
		case <-stdoutDone:
			stdoutDone = nil
		case <-stderrDone:
			stderrDone = nil
		case <-timer.C:
			return false
		}
	}
	return true
}

func cleanupTracked(tracker *processTracker, root int) error {
	var first error
	for attempt := 0; attempt < 10; attempt++ {
		if err := tracker.Discover(root); err != nil && first == nil {
			first = err
		}
		if err := tracker.Kill(); err != nil && first == nil {
			first = err
		}
		if err := tracker.Reap(); err != nil && first == nil {
			first = err
		}
		present, err := tracker.Present()
		if err != nil {
			if first == nil {
				first = err
			}
			break
		}
		if present == 0 {
			return first
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := tracker.Reap(); err != nil && first == nil {
		first = err
	}
	if present, err := tracker.Present(); err != nil && first == nil {
		first = err
	} else if err == nil && present > 0 && first == nil {
		first = errors.New("tracked native descendants remain after cleanup")
	}
	return first
}

func (r *Runner) stdoutLimit() int {
	if r == nil {
		return 2 * 1024 * 1024
	}
	if r.StdoutLimit > 0 {
		return r.StdoutLimit
	}
	return 2 * 1024 * 1024
}

func (r *Runner) stderrLimit() int {
	if r == nil {
		return 64 * 1024
	}
	if r.StderrLimit > 0 {
		return r.StderrLimit
	}
	if r.OutputLimit > 0 {
		return r.OutputLimit
	}
	return 64 * 1024
}

type limitedBuffer struct {
	value     bytes.Buffer
	limit     int
	Truncated bool
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := b.limit - b.value.Len()
	if remaining <= 0 {
		b.Truncated = true
		return written, nil
	}
	if len(value) > remaining {
		b.Truncated = true
		value = value[:remaining]
	}
	_, _ = b.value.Write(value)
	return written, nil
}

func (b *limitedBuffer) Bytes() []byte { return append([]byte(nil), b.value.Bytes()...) }

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}
