package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

type channelDiagnostic struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Connected    *bool  `json:"connected"`
	PaymentReady bool   `json:"payment_ready"`
	Code         string `json:"code"`
}

func diagnosticsKey(c config) string {
	c.Diagnostics = nil
	raw, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (s *Server) configDiagnostics(c config) []channelDiagnostic {
	s.mu.RLock()
	cached := append([]channelDiagnostic(nil), s.diagnostics[diagnosticsKey(c)]...)
	s.mu.RUnlock()
	if len(cached) > 0 {
		return cached
	}
	// DescribeConfig stays fast and never performs network calls. Actual access
	// is verified only by TestConfig; unknown is not a credential failure.
	for _, item := range c.Channels {
		entry := channelDiagnostic{ID: item.ID, Name: item.Name, Code: "not_tested"}
		if !item.Enabled {
			entry.Code = "disabled"
		}
		cached = append(cached, entry)
	}
	return cached
}

func (s *Server) testChannels(ctx context.Context, c config) *pluginv1.HealthResult {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	results := make(chan channelDiagnostic, len(c.Channels))
	checks := 0
	for _, item := range c.Channels {
		if !item.Enabled {
			continue
		}
		checks++
		go func(item channelConfig) {
			entry := channelDiagnostic{ID: item.ID, Name: item.Name}
			channel, err := s.channelFor(item)
			if err != nil {
				entry.Code = "configuration_invalid"
				results <- entry
				return
			}
			_, err = channel.Describe(ctx)
			connected := err == nil
			entry.PaymentReady = err == nil
			if errors.Is(err, payment.ErrProviderMetadataUnavailable) {
				entry.Code = "provider_metadata_missing"
				if probe, ok := channel.(payment.ConnectionProbe); ok {
					connected = probe.Probe(ctx) == nil
					entry.Connected = &connected
					if !connected {
						entry.Code = "connection_failed"
					}
				}
			} else {
				entry.Connected = &connected
				if err != nil {
					entry.Code = "connection_failed"
					if diagnose := channelProviders[item.Type].diagnose; diagnose != nil {
						entry.Code, connected = diagnose(err)
					}
				}
			}
			results <- entry
		}(item)
	}
	byID := map[string]channelDiagnostic{}
	for n := 0; n < checks; n++ {
		select {
		case entry := <-results:
			byID[entry.ID] = entry
		case <-ctx.Done():
			n = checks
		}
	}
	entries := make([]channelDiagnostic, 0, len(c.Channels))
	healthy := checks > 0
	for _, item := range c.Channels {
		entry, ok := byID[item.ID]
		if !ok {
			entry = channelDiagnostic{ID: item.ID, Name: item.Name, Code: "check_timeout"}
		}
		if !item.Enabled {
			entry.Code = "disabled"
		} else if !entry.PaymentReady {
			healthy = false
		}
		entries = append(entries, entry)
	}
	s.mu.Lock()
	if len(s.diagnostics) >= 16 {
		s.diagnostics = nil
	}
	if s.diagnostics == nil {
		s.diagnostics = map[string][]channelDiagnostic{}
	}
	s.diagnostics[diagnosticsKey(c)] = entries
	s.mu.Unlock()
	message := "payment channels verified"
	if !healthy {
		message = "payment channels are not ready; inspect plugin channel diagnostics"
	}
	return &pluginv1.HealthResult{Healthy: healthy, Message: message}
}
