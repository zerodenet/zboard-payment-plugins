package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func TestRuntimePaymentReadinessDoesNotRequireInvoiceMetadata(t *testing.T) {
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "wallet", "currency": "TDC", "currency_decimals": 8}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	s := &Server{httpClient: provider.Client()}
	health, err := s.Health(context.Background(), &pluginv1.Empty{})
	if err != nil || !health.Healthy {
		t.Fatalf("runtime health must allow plugin startup: %+v %v", health, err)
	}
	config, _ := json.Marshal(map[string]any{"channels": []map[string]any{{
		"id": "tdcloud-main", "name": "TDCloud", "type": "tdcloud", "enabled": true,
		"settings": tdcloud.Settings{Currencies: map[string]int{"USD": 2, "CNY": 2, "TDC": 8, "JPY": 0}, APIURL: provider.URL, ClientID: "client", ClientSecret: "secret", PaymentNo: "wallet"},
	}}})
	result, err := s.TestConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: config})
	if err != nil || !result.Healthy {
		t.Fatalf("decimal contract must allow configuration-only merchant access: %+v %v", result, err)
	}
}

func TestConfigPreservesExistingChannelSecret(t *testing.T) {
	previous := []byte(`{"channels":[{"id":"tdcloud-main","name":"TDCloud","type":"tdcloud","settings":{"currencies":{"USD":2,"CNY":2,"TDC":8,"JPY":0},"api_url":"https://cloud.example.test","client_id":"merchant-client","client_secret":"secret-value","payment_no":"stripe-usd"},"enabled":true}]}`)
	updated := []byte(`{"channels":[{"id":"tdcloud-main","name":"TDCloud USD","type":"tdcloud","settings":{"currencies":{"USD":2,"CNY":2,"TDC":8,"JPY":0},"api_url":"https://cloud.example.test","client_id":"merchant-client","payment_no":"stripe-usd"},"enabled":true}]}`)
	c, err := parseConfig(updated, previous)
	var settings tdcloud.Settings
	if err == nil && len(c.Channels) == 1 {
		err = json.Unmarshal(c.Channels[0].Settings, &settings)
	}
	if err != nil || len(c.Channels) != 1 || settings.ClientSecret != "secret-value" {
		t.Fatalf("secret retention: %+v %v", c, err)
	}
	if _, err := parseConfig([]byte(`{"channels":[{"id":"tdcloud-main","name":"TDCloud","type":"tdcloud","settings":{"currencies":{"USD":2,"CNY":2,"TDC":8,"JPY":0},"api_url":"https://cloud.example.test","client_id":"different-client","payment_no":"stripe-usd"},"enabled":true}]}`), previous); err == nil {
		t.Fatal("secret must not transfer to a different OAuth client")
	}
	if _, err := parseConfig([]byte(`{"channels":[{"id":"tdcloud-main","name":"TDCloud","type":"tdcloud","settings":{"currencies":{"USD":2,"CNY":2,"TDC":8,"JPY":0},"api_url":"https://other.example.test","client_id":"merchant-client","payment_no":"stripe-usd"},"enabled":true}]}`), previous); err == nil {
		t.Fatal("secret must not transfer to a different API origin")
	}
}

func TestConfigRejectsEmptyAndDuplicateChannels(t *testing.T) {
	if _, err := parseConfig(nil, nil); err == nil {
		t.Fatal("nil config accepted")
	}
	if _, err := parseConfig([]byte(`   `), nil); err == nil {
		t.Fatal("blank config accepted")
	}
	if _, err := parseConfig([]byte(`{"channels":[{"id":"same","name":"One","type":"tdcloud","settings":{"currencies":{"USD":2,"CNY":2,"TDC":8,"JPY":0},"api_url":"https://cloud.example.test","client_id":"client","client_secret":"secret","payment_no":"stripe"},"enabled":true},{"id":"same","name":"Two","type":"tdcloud","settings":{"currencies":{"USD":2,"CNY":2,"TDC":8,"JPY":0},"api_url":"https://cloud.example.test","client_id":"client","client_secret":"secret","payment_no":"stripe"},"enabled":true}]}`), nil); err == nil {
		t.Fatal("duplicate channel accepted")
	}
}

func TestConfigurationViewOmitsCredentialsAndRetainsThemOnEdit(t *testing.T) {
	s := New()
	previous := []byte(`{"channels":[{"id":"tdcloud-main","name":"TDCloud","type":"tdcloud","enabled":true,"settings":{"currencies":{"USD":2,"CNY":2,"TDC":8,"JPY":0},"api_url":"https://cloud.example.test","client_id":"merchant","client_secret":"merchant-secret-value","payment_no":"test-channel","notify_url":"https://board.example.test/.well-known/zboard-payments/v1/tdcloud-notify","notify_secret":"independent-notification-secret-test-42"}}]}`)
	view, err := s.DescribeConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: previous})
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if json.Unmarshal(view.NormalizedJson, &cfg) != nil {
		t.Fatal("invalid configuration view")
	}
	var settings tdcloud.Settings
	if json.Unmarshal(cfg.Channels[0].Settings, &settings) != nil || settings.ClientSecret != "" || settings.NotifySecret != "" {
		t.Fatal("configuration view exposed private credentials")
	}
	edited, err := s.ValidateConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: view.NormalizedJson, PreviousConfigJson: previous})
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(edited.NormalizedJson, &cfg) != nil || json.Unmarshal(cfg.Channels[0].Settings, &settings) != nil || settings.ClientSecret != "merchant-secret-value" || settings.NotifySecret != "independent-notification-secret-test-42" {
		t.Fatal("edit discarded pinned secrets")
	}
}
