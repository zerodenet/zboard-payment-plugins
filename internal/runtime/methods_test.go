package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func testTDCloudSettings(apiURL, ignored string) json.RawMessage {
	raw, _ := json.Marshal(tdcloud.Settings{APIURL: apiURL, ClientID: "client", ClientSecret: "secret", Currencies: map[string]int{"USD": 2, "TDC": 8}})
	return raw
}

func TestCheckoutPublishesOneCashierForMultipleInvoiceCurrencies(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer merchant-token" {
			t.Error("merchant OAuth missing")
		}
		// Internal method currency/login/IDs are owned by TDCloud, not the outer order.
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "internal-stripe", "currency": "JPY", "login_required": true}}})
	}))
	defer server.Close()
	s := &Server{httpClient: server.Client(), config: config{Channels: []channelConfig{{ID: "tdcloud", Name: "TDCloud", Type: "tdcloud", Settings: testTDCloudSettings(server.URL, ""), Enabled: true}}}}
	for _, currency := range []string{"USD", "TDC"} {
		methods, err := s.ListMethods(t.Context(), currency)
		if err != nil || len(methods) != 1 || methods[0].ID != "tdcloud" || methods[0].Currency != currency || methods[0].LoginRequired || methods[0].TestMode {
			t.Fatalf("cashier projection: %+v %v", methods, err)
		}
		raw, _ := json.Marshal(methods)
		if strings.Contains(string(raw), "internal-stripe") || strings.Contains(string(raw), server.URL) {
			t.Fatal("cashier internals leaked")
		}
	}
	if methods, err := s.ListMethods(t.Context(), "EUR"); err != nil || len(methods) != 0 {
		t.Fatalf("unconfigured invoice currency: %+v %v", methods, err)
	}
}

func TestCheckoutMethodsAreConcurrentCachedAndInvalidated(t *testing.T) {
	var directoryCalls atomic.Int32
	bothStarted := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			if directoryCalls.Add(1) == 2 {
				close(bothStarted)
			}
			select {
			case <-bothStarted:
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
					{"payment_no": "channel-a", "currency": "USD", "currency_decimals": 2},
					{"payment_no": "channel-b", "currency": "USD", "currency_decimals": 2},
				}})
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := config{Channels: []channelConfig{
		{ID: "channel-a", Name: "A", Type: "tdcloud", Settings: testTDCloudSettings(server.URL, "channel-a"), Enabled: true},
		{ID: "channel-b", Name: "B", Type: "tdcloud", Settings: testTDCloudSettings(server.URL, "channel-b"), Enabled: true},
	}}
	s := &Server{httpClient: server.Client(), config: cfg}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	methods, err := s.ListMethods(ctx, "USD")
	if err != nil || len(methods) != 2 || methods[0].ID != "channel-a" || methods[1].ID != "channel-b" {
		t.Fatalf("parallel methods: %+v %v", methods, err)
	}
	methods[0].Name = "caller mutation"
	methods, err = s.ListMethods(ctx, "USD")
	if err != nil || len(methods) != 2 || methods[0].Name != "A" || directoryCalls.Load() != 2 {
		t.Fatalf("cached methods: %+v %v; directory calls %d", methods, err, directoryCalls.Load())
	}
	raw, _ := json.Marshal(cfg)
	if _, err := s.ApplyConfig(ctx, &pluginv1.ConfigRequest{ConfigJson: raw, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	methods, err = s.ListMethods(ctx, "USD")
	if err == nil || len(methods) != 0 || directoryCalls.Load() != 2 {
		t.Fatalf("new configuration reused stale/manual invoice metadata: %+v %v", methods, err)
	}
	useLegacyFixtureConfig(s, cfg)
	methods, err = s.ListMethods(ctx, "USD")
	if err != nil || len(methods) != 2 || directoryCalls.Load() != 4 {
		t.Fatalf("invalidated methods: %+v %v; directory calls %d", methods, err, directoryCalls.Load())
	}
}

// These injected legacy values isolate coordinator/cache behavior. Production
// ApplyConfig removes manual invoice units, so these fixtures are not evidence
// of authoritative metadata discovery or a closed merchant setup flow.
func useLegacyFixtureConfig(s *Server, cfg config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = cfg
	s.configGeneration++
	s.methodsCache = nil
	s.methodsFlights = nil
}

func TestCheckoutMethodsKeepHealthyChannelWhenAnotherFails(t *testing.T) {
	var directoryCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			directoryCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer merchant-token" {
				t.Error("missing merchant token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "healthy", "currency": "USD", "currency_decimals": 2}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer bad.Close()
	s := &Server{httpClient: server.Client(), config: config{Channels: []channelConfig{
		{ID: "healthy", Name: "Available", Type: "tdcloud", Settings: testTDCloudSettings(server.URL, "healthy"), Enabled: true},
		{ID: "missing", Name: "Unavailable", Type: "tdcloud", Settings: testTDCloudSettings(bad.URL, ""), Enabled: true},
	}}}
	for range 2 {
		methods, err := s.ListMethods(t.Context(), "USD")
		if err != nil || len(methods) != 1 || methods[0].ID != "healthy" {
			t.Fatalf("partial channel discovery: %+v %v", methods, err)
		}
	}
	if directoryCalls.Load() != 2 {
		t.Fatalf("partial result must be retried, directory calls=%d", directoryCalls.Load())
	}
}
