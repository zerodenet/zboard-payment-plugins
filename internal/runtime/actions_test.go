package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

// This host fixture exercises the real storage SDK over its process socket.
// Optional commerce responses exercise the public native SDK; coordinator
// tests can also inject business ports directly.
func storageHost(t *testing.T, services ...http.Handler) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "zbpay-actions-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "host.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	var mu sync.Mutex
	var revision uint64
	values := map[string]json.RawMessage{}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/service" && r.Header.Get("X-Plugin-Host") == token && len(services) == 1 {
			services[0].ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/storage" || r.Method != "POST" || r.Header.Get("X-Plugin-Host") != token {
			w.WriteHeader(403)
			return
		}
		var request struct {
			Type     string          `json:"type"`
			Key      string          `json:"key"`
			Revision uint64          `json:"revision"`
			Value    json.RawMessage `json:"value"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if request.Type == "storage.put" {
			if request.Revision != revision {
				w.WriteHeader(409)
				return
			}
			values[request.Key] = request.Value
			revision++
		}
		value, found := values[request.Key]
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": revision, "found": found, "value": value})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	t.Setenv("ZBOARD_PLUGIN_HOST_SOCKET", socket)
	t.Setenv("ZBOARD_PLUGIN_HOST_TOKEN", token)
}

func action(t *testing.T, s *Server, actor, name, payload string) actionResult {
	t.Helper()
	response, err := s.HandlePageAction(t.Context(), &pluginv1.PageActionRequest{Surface: "account", ActorId: actor, PageId: "payment-methods", Action: name, PayloadJson: []byte(payload)})
	if err != nil {
		t.Fatal(err)
	}
	var result actionResult
	if err := json.Unmarshal(response.ResultJson, &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(response.ResultJson), "merchant-secret") {
		t.Fatal("browser response leaked provider credentials")
	}
	return result
}

func TestProcessStorageAndAuthenticatedActionsResumeAfterRestart(t *testing.T) {
	storageHost(t)
	var creates atomic.Int32
	var lookups atomic.Int32
	occurred := time.Now().UTC().Truncate(time.Second)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "stripe-usd", "currency": "USD", "currency_decimals": 2}}})
		case "/openapi/v1/merchant-payments":
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "payment_no": "stripe-usd", "return_url": "https://board.example/orders/42", "amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": "pending", "checkout_url": "https://pay.example/42", "expires_at": time.Now().Add(time.Hour)})
		case "/openapi/v1/merchant-payments/mp-42":
			lookups.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "payment_no": "stripe-usd", "amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": "paid", "event_id": "event-42", "provider_trade_no": "trade-42", "occurred_at": occurred})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	status := action(t, s, "buyer", "payments.status", "{}").Data.(map[string]any)
	if status["storage_ready"] != true || status["checkout_ready"] != true {
		t.Fatalf("readiness: %+v", status)
	}
	blocked := action(t, New(), "buyer", "payments.start", `{"order_id":"42","channel_id":"tdcloud"}`)
	if blocked.Error == nil || blocked.Error.Code != "host_capability_missing" || creates.Load() != 0 {
		t.Fatalf("unbound checkout: %+v", blocked)
	}
	facts := &checkoutFacts{events: map[string]bool{}}
	ports := CommercePorts{Orders: checkoutOrder{}, Facts: facts, OrderReturnURL: checkoutReturnURL}
	if err := s.BindCommerce(ports); err != nil {
		t.Fatal(err)
	}
	s.httpClient = provider.Client()
	settings := testTDCloudSettings(provider.URL, "stripe-usd")
	cfg, _ := json.Marshal(config{Channels: []channelConfig{{ID: "tdcloud", Name: "TDCloud", Type: "tdcloud", Enabled: true, Settings: settings}}})
	if _, err := s.ApplyConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: cfg}); err != nil {
		t.Fatal(err)
	}
	if result := action(t, s, "buyer", "payments.methods", `{"currency":"USD"}`); result.Error != nil {
		t.Fatalf("methods: %+v", result)
	}
	started := action(t, s, "buyer", "payments.start", `{"order_id":"42","channel_id":"tdcloud"}`)
	if started.Error != nil || started.Data.(map[string]any)["order_id"] != "42" {
		t.Fatalf("start: %+v", started)
	}
	s.Close()
	// A new process instance uses the same SDK persistence, not an in-memory attempt.
	resumed, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.httpClient = provider.Client()
	if err := resumed.BindCommerce(ports); err != nil {
		t.Fatal(err)
	}
	retried := action(t, resumed, "buyer", "payments.start", `{"order_id":"42","channel_id":"tdcloud"}`)
	if retried.Error != nil || creates.Load() != 1 {
		t.Fatalf("restart must reuse saved checkout: %+v creates=%d", retried, creates.Load())
	}
	denied := action(t, resumed, "other-account", "payments.confirm", `{"order_id":"42"}`)
	if denied.Error == nil || lookups.Load() != 0 {
		t.Fatal("another account reached provider lookup")
	}
	responses := make(chan *pluginv1.PageActionResponse, 2)
	for range 2 {
		go func() {
			response, err := resumed.HandlePageAction(context.Background(), &pluginv1.PageActionRequest{Surface: "account", ActorId: "buyer", Action: "payments.confirm", PayloadJson: []byte(`{"order_id":"42"}`)})
			if err != nil {
				responses <- nil
			} else {
				responses <- response
			}
		}()
	}
	for range 2 {
		response := <-responses
		if response == nil || !strings.Contains(string(response.ResultJson), `"state":"paid"`) {
			t.Fatalf("concurrent confirmation: %+v", response)
		}
	}
	if len(facts.events) != 1 {
		t.Fatalf("expected one payment event: %+v", facts.events)
	}
	if result := action(t, resumed, "buyer", "payments.order", `{"order_id":"42"}`); result.Error != nil {
		t.Fatalf("owned order: %+v", result)
	}
}

func TestActionsRejectUntrustedFieldsAndMalformedPayloads(t *testing.T) {
	s := New()
	for _, payload := range []string{"", " ", "null", "[]", "{} {}", `{"order_id":42}`, `{"order_id":"042"}`, `{"order_id":"0"}`, `{"order_id":"42","account_id":"buyer"}`, `{"order_id":"42","amount_minor":1}`, `{"order_id":"42","return_url":"https://evil.example"}`, `{"order_id":"42","channel_id":"tdcloud"}`} {
		result := action(t, s, "buyer", "payments.confirm", payload)
		if result.Error == nil || result.Error.Code != "invalid_argument" {
			t.Fatalf("accepted %q: %+v", payload, result)
		}
	}
	for _, r := range []*pluginv1.PageActionRequest{nil, {Surface: "admin", ActorId: "buyer"}, {Surface: "account"}, {Surface: "account", ActorId: "buyer", PageId: "other"}} {
		response, err := s.HandlePageAction(t.Context(), r)
		if err != nil || !strings.Contains(string(response.ResultJson), "permission_denied") {
			t.Fatalf("untrusted context accepted: %+v %v", response, err)
		}
	}
	if err := s.BindCommerce(CommercePorts{}); err == nil {
		t.Fatal("commerce bound without persistence")
	}
	t.Setenv("ZBOARD_PLUGIN_HOST_SOCKET", "")
	t.Setenv("ZBOARD_PLUGIN_HOST_TOKEN", "")
	if _, err := Open(); err == nil {
		t.Fatal("process initialized without storage authorization")
	}
	if actionErrorCode(payment.ErrConflict) != "conflict" {
		t.Fatal("conflict classification")
	}
}
