package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func TestConnectionDiagnosticsSeparateMerchantAccessFromPaymentReadiness(t *testing.T) {
	for _, authorized := range []bool{true, false} {
		t.Run(fmt.Sprint(authorized), func(t *testing.T) {
			var requests, creates atomic.Int32
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/oauth/token":
					if !authorized {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "private-token", "expires_in": 3600})
				case "/openapi/v1/merchant-payment-channels":
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "wallet", "currency": "TDC", "currency_decimals": 8}}})
				default:
					creates.Add(1)
					http.NotFound(w, r)
				}
			}))
			defer provider.Close()
			raw := []byte(fmt.Sprintf(`{"channels":[{"id":"tdcloud-main","name":"TDCloud","type":"tdcloud","enabled":true,"settings":{"api_url":%q,"client_id":"merchant","client_secret":"private-credential"}}]}`, provider.URL))
			s := &Server{httpClient: provider.Client()}
			view, err := s.DescribeConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: raw})
			if err != nil {
				t.Fatal(err)
			}
			var projected config
			if err = json.Unmarshal(view.NormalizedJson, &projected); err != nil {
				t.Fatal(err)
			}
			if projected.Diagnostics[0].Connected != nil || requests.Load() != 0 {
				t.Fatal("loading untested configuration must remain fast and report unknown access")
			}
			health, err := s.TestConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: raw})
			if err != nil || health.Healthy != authorized || requests.Load() == 0 || creates.Load() != 0 {
				t.Fatalf("decimal contract readiness must depend on merchant access without creating orders: %+v %v", health, err)
			}
			view, err = s.DescribeConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: raw})
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(view.NormalizedJson, &projected); err != nil {
				t.Fatal(err)
			}
			report := projected.Diagnostics[0]
			code := "oauth_failed"
			if authorized {
				code = ""
			}
			if report.Connected == nil || *report.Connected != authorized || report.PaymentReady != authorized || report.Code != code {
				t.Fatalf("wrong diagnostic: %+v", report)
			}
			if strings.Contains(string(view.NormalizedJson), "private-credential") || strings.Contains(string(view.NormalizedJson), "private-token") {
				t.Fatal("diagnostics exposed credentials")
			}
			validated, err := s.ValidateConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: view.NormalizedJson, PreviousConfigJson: raw})
			if err != nil || strings.Contains(string(validated.NormalizedJson), "diagnostics") {
				t.Fatalf("read-only diagnostics must never persist as configuration: %v", err)
			}
			changed := []byte(strings.Replace(string(raw), "private-credential", "new-private-credential", 1))
			view, err = s.DescribeConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: changed})
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(view.NormalizedJson, &projected); err != nil {
				t.Fatal(err)
			}
			if projected.Diagnostics[0].Connected != nil || projected.Diagnostics[0].Code != "not_tested" {
				t.Fatal("changed credentials must not reuse earlier connection results")
			}
		})
	}
}
