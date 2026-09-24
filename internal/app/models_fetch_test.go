package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The provider's catalogue is fetched outside the lock the cached list is read under, and refreshes
// that arrive while a fetch is running wait for it rather than starting their own. Before, a refresh
// held the lock across the network call, so any member asking for fresh models made every caller
// that only wanted the cached list — own-key pricing, image turns, model checks — wait on the
// provider.
func TestModelRefreshesDoNotBlockCachedReads(t *testing.T) {
	var fetches int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") && !strings.HasSuffix(r.URL.Path, "/embeddings/models") {
			if atomic.AddInt32(&fetches, 1) > 1 {
				<-release // every fetch after the warm-up is held until the test lets it go
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "gpt-4o"}}})
	}))
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	l := NewLLM(Config{LLMBaseURL: srv.URL, LLMKey: "k", Model: "m"})
	ctx := context.Background()
	if _, err := l.ListModels(ctx, false); err != nil {
		t.Fatal(err)
	}

	go l.ListModels(ctx, true) // the refresh that fetches, held by the server
	for deadline := time.Now().Add(2 * time.Second); atomic.LoadInt32(&fetches) < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the refresh never reached the provider")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var wg sync.WaitGroup
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.ListModels(ctx, true) }()
	}

	read := make(chan struct{})
	go func() { l.ListModels(ctx, false); close(read) }()
	select {
	case <-read:
	case <-time.After(2 * time.Second):
		t.Fatal("a cached read waited on a refresh in flight")
	}

	time.Sleep(100 * time.Millisecond)
	if n := atomic.LoadInt32(&fetches); n != 2 {
		t.Errorf("refreshes started %d fetches while one was already in flight, want none", n-2)
	}
	close(release)
	wg.Wait()
}
