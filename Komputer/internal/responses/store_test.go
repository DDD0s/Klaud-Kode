package responses

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorePutPersistsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "responses.json")
	store := NewStore(file, 10)
	rec := &Record{ID: "resp_test", Response: []byte("{}")}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	loaded := NewStore(file, 10)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}
	if loaded.Get("resp_test") == nil {
		t.Fatal("record did not reload")
	}
}
