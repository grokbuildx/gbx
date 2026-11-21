package runtimelog

import (
	"sync"
	"testing"
)

func TestAcquireSessionLogRefcountsConcurrentRuntimes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GBX_LOG_FILE", "")
	ConfigureLogLevel(LogLevelInfo)
	t.Cleanup(func() { _ = SetLogLevel(LogLevelOff, "") })

	path1, release1, err := AcquireSessionLog("runtime")
	if err != nil || path1 == "" || !LogEnabled() {
		t.Fatalf("first acquire: path=%q enabled=%t err=%v", path1, LogEnabled(), err)
	}
	path2, release2, err := AcquireSessionLog("runtime")
	if err != nil || path2 != path1 {
		t.Fatalf("second acquire: path=%q want=%q err=%v", path2, path1, err)
	}

	// Releasing the first runtime must keep the log open for the second.
	release1()
	if !LogEnabled() {
		t.Fatal("closing first runtime closed log still used by second runtime")
	}
	// Concurrent logging while closing must remain race-free.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				LogInfof("concurrent runtime log entry")
			}
		}()
	}
	release2()
	wg.Wait()
	if LogEnabled() || !LogConfigured() {
		t.Fatalf("after final release: enabled=%t configured=%t", LogEnabled(), LogConfigured())
	}
}
