package runsupervisor

import (
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestSupervisorProcessConcurrentWaitersShareOneOutcome(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exec sleep 30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := newSupervisorProcess(command)
	const waiters = 8
	results := make(chan processWaitOutcome, waiters)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range waiters {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- process.waitForExit(25 * time.Millisecond)
		}()
	}
	close(start)
	group.Wait()
	close(results)
	var want processWaitOutcome
	for result := range results {
		if !result.timedOut || result.waitErr == nil {
			t.Fatalf("process timeout outcome = %#v", result)
		}
		if want == (processWaitOutcome{}) {
			want = result
			continue
		}
		if result != want {
			t.Fatalf("concurrent process outcome = %#v, want %#v", result, want)
		}
	}
}
