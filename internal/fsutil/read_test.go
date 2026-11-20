package fsutil

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestReadFileWhileAnotherWriterReplacesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFileAtomic(path, []byte(`{"n":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = WriteFileAtomic(path, []byte(`{"n":1}`), 0o600)
		}
	}()
	for range 500 {
		data, err := ReadFile(path)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("read during concurrent replace: %v", err)
		}
		if len(data) == 0 {
			close(stop)
			wg.Wait()
			t.Fatal("read an empty file during an atomic replace")
		}
	}
	close(stop)
	wg.Wait()
}

func TestReadFileReportsMissingFilesAtOnce(t *testing.T) {
	if _, err := ReadFile(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}
