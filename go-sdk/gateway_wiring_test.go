package ix

// gateway_wiring_test.go — TDD for Change 1: per-chat env injection.
//
// Tests that buildEnvSlice appends IX_BROWSER_MODE and IX_CHAT_ID when
// BrowserGatewayURL is set, and omits both when it is empty.

import (
	"strings"
	"testing"
)

// buildMinimalManager returns an *IXManager with only the fields needed to
// call buildEnvSlice. Mirrors the pattern in manager_test.go (e.g. TestResolveOpts).
func buildMinimalManager(cfg ManagerConfig) *IXManager {
	cfg.applyDefaults()
	return &IXManager{cfg: cfg}
}

func TestBuildEnvSlice_BrowserGatewayURL_Set(t *testing.T) {
	const gwURL = "http://browser-gw.internal:9867"
	const chatID = "abc123"

	m := buildMinimalManager(ManagerConfig{
		BrowserGatewayURL: gwURL,
	})

	env := m.buildEnvSlice(nil, chatID, nil)

	if !sliceContains(env, "IX_BROWSER_MODE=remote="+gwURL) {
		t.Errorf("env missing IX_BROWSER_MODE=remote=%s; got %v", gwURL, env)
	}
	if !sliceContains(env, "IX_CHAT_ID="+chatID) {
		t.Errorf("env missing IX_CHAT_ID=%s; got %v", chatID, env)
	}
}

func TestBuildEnvSlice_BrowserGatewayURL_Empty(t *testing.T) {
	m := buildMinimalManager(ManagerConfig{
		BrowserGatewayURL: "",
	})

	env := m.buildEnvSlice(nil, "some-id", nil)

	for _, e := range env {
		if strings.HasPrefix(e, "IX_BROWSER_MODE=") {
			t.Errorf("env should NOT contain IX_BROWSER_MODE when BrowserGatewayURL is empty; got %v", env)
		}
		if strings.HasPrefix(e, "IX_CHAT_ID=") {
			t.Errorf("env should NOT contain IX_CHAT_ID when BrowserGatewayURL is empty; got %v", env)
		}
	}
}

func TestBuildEnvSlice_BrowserGatewayURL_EmptyChatID(t *testing.T) {
	// When chatID is "" (pool entry), IX_CHAT_ID should not be injected
	// even if BrowserGatewayURL is set.
	const gwURL = "http://browser-gw.internal:9867"

	m := buildMinimalManager(ManagerConfig{
		BrowserGatewayURL: gwURL,
	})

	env := m.buildEnvSlice(nil, "", nil)

	if !sliceContains(env, "IX_BROWSER_MODE=remote="+gwURL) {
		t.Errorf("env missing IX_BROWSER_MODE=remote=%s; got %v", gwURL, env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "IX_CHAT_ID=") {
			t.Errorf("env should NOT contain IX_CHAT_ID when chatID is empty; got %v", env)
		}
	}
}

func TestBuildEnvSlice_UserEnvPreserved(t *testing.T) {
	// Existing behavior: user env is passed through regardless of gateway URL.
	m := buildMinimalManager(ManagerConfig{
		BrowserGatewayURL: "http://gw.example",
	})

	env := m.buildEnvSlice(map[string]string{"MY_VAR": "hello"}, "chat1", nil)

	if !sliceContains(env, "MY_VAR=hello") {
		t.Errorf("user env MY_VAR=hello missing; got %v", env)
	}
}

func TestBuildEnvSlice_LightChatDisablesBrowser(t *testing.T) {
	m := buildMinimalManager(ManagerConfig{
		BrowserGatewayURL: "http://gw:9100",
	})
	no := false
	env := m.buildEnvSlice(nil, "chat-light", &no)

	if !sliceContains(env, "IX_BROWSER_MODE=disabled") {
		t.Errorf("light chat should get IX_BROWSER_MODE=disabled; got %v", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "IX_BROWSER_MODE=remote=") {
			t.Errorf("light chat must not get remote mode; got %v", env)
		}
	}
}

func TestBuildEnvSlice_BrowserChatUsesRemote(t *testing.T) {
	const gw = "http://gw:9100"
	m := buildMinimalManager(ManagerConfig{BrowserGatewayURL: gw})
	yes := true
	env := m.buildEnvSlice(nil, "chat-b", &yes)
	if !sliceContains(env, "IX_BROWSER_MODE=remote="+gw) {
		t.Errorf("browser chat should get remote mode; got %v", env)
	}
	if !sliceContains(env, "IX_CHAT_ID=chat-b") {
		t.Errorf("browser chat should get IX_CHAT_ID for gateway routing; got %v", env)
	}
}

func TestBuildEnvSlice_GatewayTokenForwarded(t *testing.T) {
	const gw = "http://gw:9100"
	// Token set: it is forwarded to the daemon alongside remote mode.
	m := buildMinimalManager(ManagerConfig{BrowserGatewayURL: gw, GatewayToken: "tok"})
	env := m.buildEnvSlice(nil, "chat-t", nil)
	if !sliceContains(env, "IX_BROWSER_GATEWAY_TOKEN=tok") {
		t.Errorf("expected IX_BROWSER_GATEWAY_TOKEN=tok; got %v", env)
	}

	// No token: the key must be absent.
	mNoTok := buildMinimalManager(ManagerConfig{BrowserGatewayURL: gw})
	for _, e := range mNoTok.buildEnvSlice(nil, "chat-t", nil) {
		if strings.HasPrefix(e, "IX_BROWSER_GATEWAY_TOKEN=") {
			t.Errorf("token key must be absent when GatewayToken is empty; got %v", e)
		}
	}
}

func TestBuildEnvSlice_NilBrowserDefaultsToRemoteWhenGatewaySet(t *testing.T) {
	const gw = "http://gw:9100"
	m := buildMinimalManager(ManagerConfig{BrowserGatewayURL: gw})
	env := m.buildEnvSlice(nil, "chat-d", nil) // nil = manager default
	if !sliceContains(env, "IX_BROWSER_MODE=remote="+gw) {
		t.Errorf("nil Browser with gateway set should default to remote; got %v", env)
	}
}

// sliceContains reports whether s is present in slice.
func sliceContains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
