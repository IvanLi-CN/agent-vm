package state

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFreshRQLiteRefusesExistingStateWithoutWriting(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/db/query" {
			writes++
			http.Error(w, "unexpected write", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"columns":["type","name","sql"],"types":["text","text","text"],"values":[["table","existing","CREATE TABLE existing(id INTEGER)"]]}]}`))
	}))
	defer server.Close()
	store, err := OpenFreshRQLite(context.Background(), server.URL)
	if store != nil {
		store.Close()
		t.Fatal("existing state opened")
	}
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("error: %v", err)
	}
	if writes != 0 {
		t.Fatal("existing target was modified")
	}
}
func TestFreshRQLiteRejectsSQLiteTarget(t *testing.T) {
	if store, err := OpenFreshRQLite(context.Background(), "/tmp/not-a-remote-state.sqlite3"); err == nil || store != nil {
		t.Fatal("local state accepted")
	}
}
