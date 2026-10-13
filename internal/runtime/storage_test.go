package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestBucketedStorageRetainsAccountsAndChecksAttemptRevision(t *testing.T) {
	actorA := "account-1"
	actorB := ""
	for i := 2; i < 200; i++ {
		candidate := "account-" + strconv.Itoa(i)
		if attemptBucket(candidate) == attemptBucket(actorA) {
			actorB = candidate
			break
		}
	}
	if actorB == "" {
		t.Fatal("could not find a bucket collision")
	}
	type record struct {
		Revision uint64
		Values   map[string]json.RawMessage
	}
	data := &record{Values: map[string]json.RawMessage{}}
	var mu sync.Mutex
	directory, err := os.MkdirTemp("/tmp", "zbpay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "host.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Type     string          `json:"type"`
			Key      string          `json:"key"`
			Revision uint64          `json:"revision"`
			Value    json.RawMessage `json:"value"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if request.Type == "storage.put" {
			if request.Revision != data.Revision {
				w.WriteHeader(http.StatusConflict)
				return
			}
			data.Values[request.Key] = request.Value
			data.Revision++
		}
		value, found := data.Values[request.Key]
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": data.Revision, "found": found, "value": value})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	t.Setenv("ZBOARD_PLUGIN_HOST_SOCKET", socket)
	t.Setenv("ZBOARD_PLUGIN_HOST_TOKEN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	store, err := openStorage()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first, err := store.Put(ctx, payment.Attempt{AccountID: actorA, OrderID: 1, State: "pending",
		ChannelSnapshot: []byte(`{"type":"tdcloud","settings":{"client_secret":"test-only"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(ctx, payment.Attempt{AccountID: actorB, OrderID: 2, State: "pending",
		ChannelSnapshot: []byte(`{"type":"tdcloud","settings":{"client_secret":"test-only"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	first.State = "paid"
	first.ProviderOrderID = "mp-account-1"
	if _, err := store.Put(ctx, first); err != nil {
		t.Fatal(err)
	}
	located, err := store.FindByProviderOrder(ctx, "tdcloud", "mp-account-1")
	if err != nil || located.AccountID != actorA || located.OrderID != 1 || located.State != "paid" {
		t.Fatalf("provider order index: %+v %v", located, err)
	}
	if _, err := store.FindByProviderOrder(ctx, "tdcloud", "mp-unknown"); err != payment.ErrNotFound {
		t.Fatalf("unknown provider order: %v", err)
	}
	for i := 2; i < 200; i++ {
		candidate := "mp-account-" + strconv.Itoa(i)
		if providerOrderBucket("tdcloud", candidate) == providerOrderBucket("tdcloud", first.ProviderOrderID) {
			second.ProviderOrderID = candidate
			break
		}
	}
	if second.ProviderOrderID == "" {
		t.Fatal("could not find provider index bucket collision")
	}
	if _, err := store.Put(ctx, second); err != nil {
		t.Fatal(err)
	}
	located, err = store.FindByProviderOrder(ctx, "tdcloud", second.ProviderOrderID)
	if err != nil || located.AccountID != actorB || located.OrderID != 2 {
		t.Fatalf("provider index bucket collision: %+v %v", located, err)
	}
	if _, err := store.Put(ctx, payment.Attempt{AccountID: actorA, OrderID: 1, State: "failed"}); err != payment.ErrConflict {
		t.Fatalf("stale attempt revision accepted: %v", err)
	}
	for actor, id := range map[string]uint64{actorA: 1, actorB: 2} {
		loaded, err := store.Get(ctx, actor, id)
		if err != nil {
			t.Fatalf("attempt %s/%d: %v", actor, id, err)
		}
		if actor == actorA && string(loaded.ChannelSnapshot) != `{"type":"tdcloud","settings":{"client_secret":"test-only"}}` {
			t.Fatalf("provider snapshot did not survive storage round trip: %q", loaded.ChannelSnapshot)
		}
	}
	mu.Lock()
	if len(data.Values) != 2 {
		t.Error(fmt.Sprintf("expected attempt bucket and provider index, got %d keys", len(data.Values)))
	}
	mu.Unlock()
}
