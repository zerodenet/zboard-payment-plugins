package tdcloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// This fixture models documented normalization, rejection and idempotency; it
// never accepts an arbitrary body or echoes success without checking options.
type contractInput struct {
	Reference     string `json:"external_reference"`
	Key           string `json:"idempotency_key"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	PaymentNo     string `json:"payment_no"`
	Description   string `json:"description"`
	ReturnURL     string `json:"return_url"`
	ExpiresIn     *int64 `json:"expires_in"`
	NotifyEnabled *bool  `json:"notify_enabled"`
	NotifyURL     string `json:"notify_url"`
	NotifySecret  string `json:"notify_secret"`
}

type contractFixture struct {
	server               *httptest.Server
	mu                   sync.Mutex
	canonical            []byte
	view                 merchantPayment
	bodies               []map[string]json.RawMessage
	defaultNotifications bool
	firstResponse        int
	firstCode            string
	rejectBeforeCreate   bool
}

func newContractFixture(t *testing.T, defaults bool, firstResponse int) *contractFixture {
	t.Helper()
	p := &contractFixture{defaultNotifications: defaults, firstResponse: firstResponse, firstCode: "provider_result_pending"}
	p.server = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}

func (p *contractFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth/token" {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant", "expires_in": 3600})
		return
	}
	if r.URL.Path != "/openapi/v1/merchant-payments" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer merchant" {
		http.NotFound(w, r)
		return
	}
	var raw map[string]json.RawMessage
	if json.NewDecoder(r.Body).Decode(&raw) != nil {
		w.WriteHeader(400)
		return
	}
	encoded, _ := json.Marshal(raw)
	var in contractInput
	if json.Unmarshal(encoded, &in) != nil {
		w.WriteHeader(400)
		return
	}
	in.Reference, in.Key = strings.TrimSpace(in.Reference), strings.TrimSpace(in.Key)
	in.Description, in.ReturnURL, in.PaymentNo = strings.TrimSpace(in.Description), strings.TrimSpace(in.ReturnURL), strings.TrimSpace(in.PaymentNo)
	in.NotifyURL = strings.TrimSpace(in.NotifyURL)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bodies = append(p.bodies, raw)
	fail := func(code string) { w.WriteHeader(400); _ = json.NewEncoder(w).Encode(map[string]string{"error": code}) }
	if in.Reference == "" || in.Key == "" || in.Amount != "10.50" || in.Currency != "USD" || raw["amount_minor"] != nil {
		fail("invalid_request")
		return
	}
	seconds := int64(1800)
	if in.ExpiresIn != nil {
		seconds = *in.ExpiresIn
	}
	if seconds < 60 || seconds > 7200 {
		fail("invalid_expires_in")
		return
	}
	in.ExpiresIn = &seconds // explicit 1800 and omitted/null share one hash
	pair := in.NotifyURL != "" && in.NotifySecret != ""
	if (in.NotifyURL == "") != (in.NotifySecret == "") || (in.NotifyEnabled != nil && !*in.NotifyEnabled && pair) {
		fail("invalid_notify_config")
		return
	}
	if in.NotifyEnabled != nil && *in.NotifyEnabled && !pair && !p.defaultNotifications {
		fail("invalid_notify_config")
		return
	}
	canonical, _ := json.Marshal(in)
	rejectFirst := func() {
		w.WriteHeader(p.firstResponse)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": p.firstCode})
	}
	if len(p.bodies) == 1 && p.firstResponse != 0 && p.rejectBeforeCreate {
		rejectFirst()
		return
	}
	if p.canonical != nil && string(canonical) != string(p.canonical) {
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "payment_conflict"})
		return
	}
	if p.canonical == nil {
		p.canonical = canonical
		p.view = merchantPayment{OrderNo: "mp", ExternalRef: in.Reference, Amount: "10.50", Currency: in.Currency,
			ReturnURL: in.ReturnURL, Status: "pending", CheckoutURL: "https://cashier.example.test/mp", ExpiresAt: time.Now().UTC().Add(time.Duration(seconds) * time.Second)}
	}
	if len(p.bodies) == 1 && p.firstResponse != 0 {
		rejectFirst()
		return
	}
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(p.view)
}

func (p *contractFixture) lastBody() map[string]json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bodies[len(p.bodies)-1]
}
