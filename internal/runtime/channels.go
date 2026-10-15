package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
)

type channelProvider struct {
	normalize func(json.RawMessage, json.RawMessage) (json.RawMessage, error)
	project   func(json.RawMessage) (json.RawMessage, error)
	build     func(string, json.RawMessage, *http.Client) (payment.Channel, error)
	restore   func(string, json.RawMessage, *http.Client) (payment.Channel, error)
	diagnose  func(error) (code string, connected bool)
}

// New providers join this registry while checkout, reconciliation and method
// projection continue to consume the payment.Channel contract.
var channelProviders = map[string]channelProvider{
	"tdcloud": {
		normalize: tdcloud.NormalizeSettings,
		project:   tdcloud.ProjectSettings,
		diagnose:  tdcloud.ConnectionDiagnostic,
		build: func(id string, raw json.RawMessage, client *http.Client) (payment.Channel, error) {
			return tdcloud.NewFromSettings(id, raw, client)
		},
		restore: func(id string, raw json.RawMessage, client *http.Client) (payment.Channel, error) {
			return tdcloud.NewFromSnapshot(id, raw, client)
		},
	},
}

func buildChannel(c channelConfig, client *http.Client) (payment.Channel, error) {
	provider, ok := channelProviders[c.Type]
	if !ok {
		return nil, errors.New("unsupported payment channel type")
	}
	return provider.build(c.ID, c.Settings, client)
}

// channelFor reuses the provider's short-lived OAuth token across method
// discovery, checkout creation and repeated confirmation checks. The key is a
// digest of the full normalized configuration, so credential rotation creates
// a separate client without exposing the credential in the cache key.
func (s *Server) channelFor(c channelConfig) (payment.Channel, error) {
	return s.cachedChannel(c, false)
}

func channelKey(c channelConfig, saved bool) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%t", saved) + c.ID + "\x00" + c.Type + "\x00" + string(c.Settings)))
	return fmt.Sprintf("%x", digest)
}

func (s *Server) cachedChannel(c channelConfig, saved bool) (payment.Channel, error) {
	key := channelKey(c, saved)
	s.mu.Lock()
	defer s.mu.Unlock()
	if channel := s.channelCache[key]; channel != nil {
		return channel, nil
	}
	var channel payment.Channel
	var err error
	if saved && channelProviders[c.Type].restore != nil {
		channel, err = channelProviders[c.Type].restore(c.ID, c.Settings, s.httpClient)
	} else {
		channel, err = buildChannel(c, s.httpClient)
	}
	if err != nil {
		return nil, err
	}
	if len(s.channelCache) >= 64 {
		s.channelCache = nil
	}
	if s.channelCache == nil {
		s.channelCache = make(map[string]payment.Channel)
	}
	s.channelCache[key] = channel
	return channel, nil
}

func (s *Server) prepareSnapshot(ctx context.Context, item channelConfig, active payment.Channel, currency string) (payment.Channel, []byte, error) {
	prepared := active
	if snapshotter, ok := active.(payment.SnapshotChannel); ok {
		channel, settings, err := snapshotter.PrepareSnapshot(ctx, item.Settings, currency)
		if err != nil {
			return nil, nil, err
		}
		prepared = channel
		item.Settings = settings
	}
	raw, err := snapshotChannel(item)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if len(s.channelCache) >= 64 {
		s.channelCache = nil
	}
	if s.channelCache == nil {
		s.channelCache = map[string]payment.Channel{}
	}
	s.channelCache[channelKey(item, true)] = prepared
	s.mu.Unlock()
	return prepared, raw, nil
}
