package tdcloud

import (
	"encoding/json"
	"testing"
)

func TestPlatformNotificationKeyIsRequiredPrivateAndRetained(t *testing.T) {
	settings := Settings{APIURL: "https://cloud.example.test", ClientID: "merchant", ClientSecret: "oauth-secret", NotifyURL: "https://board.example.test" + NotificationPath, NotifyMode: "merchant_default"}
	raw, _ := json.Marshal(settings)
	if _, err := NormalizeSettings(raw, nil); err == nil {
		t.Fatal("plugin must not generate a replacement for the platform signing key")
	}
	settings.NotifySecret = "platform-issued-signing-key-123456789"
	raw, _ = json.Marshal(settings)
	normalized, err := NormalizeSettings(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := ProjectSettings(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var public map[string]json.RawMessage
	_ = json.Unmarshal(view, &public)
	if public["notify_secret"] != nil || public["client_secret"] != nil {
		t.Fatal("editor received private credentials")
	}
	retained, err := NormalizeSettings(view, normalized)
	var next Settings
	_ = json.Unmarshal(retained, &next)
	if err != nil || next.NotifySecret != settings.NotifySecret {
		t.Fatal("redacted edit lost platform signing key")
	}
	settings.NotifySecret = ""
	settings.NotifyURL = "https://callbacks.example.test" + NotificationPath
	raw, _ = json.Marshal(settings)
	changed, err := NormalizeSettings(raw, normalized)
	_ = json.Unmarshal(changed, &next)
	if err != nil || next.NotifySecret == "" || next.NotifySecret != "platform-issued-signing-key-123456789" {
		t.Fatal("receiver edit must not redefine a platform-issued key")
	}
	// Legacy per-order keys cannot be silently reinterpreted as platform defaults.
	var old Settings
	_ = json.Unmarshal(normalized, &old)
	old.NotifyMode = ""
	legacy, _ := json.Marshal(old)
	if _, err := NormalizeSettings(raw, legacy); err == nil {
		t.Fatal("legacy override key was reused as a platform default")
	}
	old.AmountMetadata = &AmountMetadata{Version: 1, Currency: "USD", Decimals: 2}
	snapshot, _ := json.Marshal(old)
	restored, err := NewFromSnapshot("tdcloud", snapshot, nil)
	if err != nil || restored.notifyMode != "" || restored.notifySecret != old.NotifySecret {
		t.Fatal("legacy attempt notification contract changed")
	}
}

func TestNotificationPathCannotBeChangedAndDisabledConfigDoesNotGenerateKey(t *testing.T) {
	settings := Settings{APIURL: "https://cloud.example.test", ClientID: "merchant", ClientSecret: "oauth-secret"}
	raw, _ := json.Marshal(settings)
	normalized, err := NormalizeSettings(raw, nil)
	var saved Settings
	_ = json.Unmarshal(normalized, &saved)
	if err != nil || saved.NotifySecret != "" || saved.NotifyEnabled == nil || *saved.NotifyEnabled {
		t.Fatal("disabled notifications must remain unconfigured")
	}
	settings.NotifySecret = "platform-issued-signing-key-123456789"
	settings.NotifyMode = "merchant_default"
	for _, suffix := range []string{"/custom", NotificationPath + "?x=1", NotificationPath + "#fragment"} {
		settings.NotifyURL = "https://board.example.test" + suffix
		raw, _ = json.Marshal(settings)
		if _, err := NormalizeSettings(raw, nil); err == nil {
			t.Fatal("accepted modified notification path")
		}
	}
}
