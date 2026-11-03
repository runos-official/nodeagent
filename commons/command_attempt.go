package commons

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// attemptWaitDelay bounds how long a finished command may keep its output pipe
// open. A command that starts a background child which inherits stdout/stderr
// would otherwise keep Wait from returning until that child exits.
const attemptWaitDelay = 2 * time.Second

// lineCollector is the io.Writer behind a command's stdout and stderr in the
// activity-timeout runners. It collects every line (a final line without a
// newline is completed with one), and calls onLine for each complete line so
// the runner can reset its inactivity timer.
//
// Because the process writes into it directly, exec.Cmd.Wait does not return
// until every byte the process wrote has been delivered here. The earlier
// runners read the StdoutPipe/StderrPipe in a separate goroutine while another
// goroutine called Wait; Wait closes those pipes as soon as the process exits,
// so a fast command lost part or all of its output (FCR 1661).
type lineCollector struct {
	mu     sync.Mutex
	out    strings.Builder
	part   bytes.Buffer
	onLine func()
}

func (c *lineCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	lines := 0
	for _, b := range p {
		if b == '\n' {
			c.flushLocked()
			lines++
			continue
		}
		c.part.WriteByte(b)
	}
	c.mu.Unlock()
	if c.onLine != nil {
		for i := 0; i < lines; i++ {
			c.onLine()
		}
	}
	return len(p), nil
}

// flushLocked moves the pending partial line into the output as a full line,
// dropping one trailing carriage return like bufio.ScanLines does.
func (c *lineCollector) flushLocked() {
	line := c.part.Bytes()
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	c.out.Write(line)
	c.out.WriteByte('\n')
	c.part.Reset()
}

// String returns everything collected so far, including a trailing line that
// had no newline. Call it after the command has been waited for.
func (c *lineCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.part.Len() > 0 {
		c.flushLocked()
	}
	return c.out.String()
}

// runAttempt runs one attempt of command through /bin/sh with an inactivity
// timeout. It returns the collected output, the command's own error (nil on
// success) and whether the attempt was killed for silence. The output is
// complete whenever the command exited by itself, and holds everything printed
// before the kill when it timed out. onLine is called once per output line.
func runAttempt(command string, inactivityTimeout time.Duration, onLine func(), onTimeout func()) (output string, runErr error, timedOut bool, startErr error) {
	activityCh := make(chan struct{}, 100)
	collector := &lineCollector{onLine: func() {
		if onLine != nil {
			onLine()
		}
		select {
		case activityCh <- struct{}{}:
		default:
		}
	}}

	systemCmd := exec.Command("/bin/sh", "-c", command)
	// Own process group so a timeout can kill the command and its children.
	systemCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Same writer for both streams: exec gives them one pipe, so the lines keep
	// the order the command wrote them in.
	systemCmd.Stdout = collector
	systemCmd.Stderr = collector
	systemCmd.WaitDelay = attemptWaitDelay

	if err := systemCmd.Start(); err != nil {
		return "", nil, false, err
	}

	errCh := make(chan error, 1)
	go func() { errCh <- systemCmd.Wait() }()

	timer := time.NewTimer(inactivityTimeout)
	defer timer.Stop()

	for {
		select {
		case <-activityCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(inactivityTimeout)

		case <-timer.C:
			if onTimeout != nil {
				onTimeout()
			}
			// A negative PID kills the whole process group, children included.
			syscall.Kill(-systemCmd.Process.Pid, syscall.SIGKILL)
			<-errCh
			return collector.String(), nil, true, nil

		case err := <-errCh:
			// The process exited and every byte it wrote is in the collector.
			// ErrWaitDelay only means a background child still held the pipe:
			// the command itself succeeded.
			if errors.Is(err, exec.ErrWaitDelay) {
				err = nil
			}
			return collector.String(), err, false, nil
		}
	}
}
