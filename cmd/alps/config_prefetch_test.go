package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alps.toml")
	config := "[smtp]\nserver = \"smtps://smtp.example.com:465\"\n[provider.imap]\nserver = \"imaps://imap.example.com:993\"\n" + body
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigPrefetchInbox_DefaultsOffWithDefaultLimit(t *testing.T) {
	c := writeConfig(t, "")
	opts, err := c.ToOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opts.PrefetchInboxOnLogin {
		t.Error("PrefetchInboxOnLogin should default to false")
	}
	if opts.PrefetchInboxLimit != 100 {
		t.Errorf("PrefetchInboxLimit = %d, want default 100", opts.PrefetchInboxLimit)
	}
}

func TestConfigPrefetchInbox_EnabledWithCustomLimit(t *testing.T) {
	c := writeConfig(t, "[cache]\nenabled = true\nprefetch_inbox = true\nprefetch_inbox_limit = 25\n")
	opts, err := c.ToOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !opts.PrefetchInboxOnLogin {
		t.Error("PrefetchInboxOnLogin should be true")
	}
	if opts.PrefetchInboxLimit != 25 {
		t.Errorf("PrefetchInboxLimit = %d, want 25", opts.PrefetchInboxLimit)
	}
}

// TestConfigPrefetchInbox_NoOpWhenCacheDisabled verifies that enabling
// prefetch_inbox has no effect when the session cache itself is disabled
// (sessions then get a 1-second dummy cache, so prefetching into it would
// just be wasted IMAP traffic).
func TestConfigPrefetchInbox_NoOpWhenCacheDisabled(t *testing.T) {
	c := writeConfig(t, "[cache]\nenabled = false\nprefetch_inbox = true\n")
	opts, err := c.ToOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opts.PrefetchInboxOnLogin {
		t.Error("PrefetchInboxOnLogin should be forced off when cache.enabled is false")
	}
}
