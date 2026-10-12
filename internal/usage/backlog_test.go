package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcceptedRecordsSurviveWriteBacklog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	j, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	// Completion of already admitted requests must still be recorded after a write fault.
	note := strings.Repeat("x", 256<<10)
	for i := 0; i < 130; i++ {
		j.Record(Entry{Status: 200, Error: note})
	}
	if err := j.Flush(); err == nil {
		t.Fatal("expected write failure")
	}
	if j.Err() == nil {
		t.Fatal("write backlog not visible")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	if j.Err() != nil {
		t.Fatalf("recovery not visible: %v", j.Err())
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got := restored.Spend("*", j.now().AddDate(0, 0, -1)).Requests; got != 130 {
		t.Fatalf("accepted records dropped at buffer limit: %d", got)
	}
}
