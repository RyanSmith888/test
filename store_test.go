package main

import (
	"testing"
	"time"
)

func TestStore_AccountCRUD(t *testing.T) {
	store, _ := setupTestStore(t)

	// Create
	id, err := store.CreateAccount("test-acct", "tok-123", "fp-abc", 30, 3, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id < 1 {
		t.Fatal("expected positive id")
	}

	// List
	accts, err := store.ListAccounts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accts))
	}
	if accts[0].Name != "test-acct" || accts[0].RPM != 30 || accts[0].Fingerprint != "fp-abc" {
		t.Error("account fields mismatch")
	}

	// Get
	a, err := store.GetAccount(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.Token != "tok-123" {
		t.Error("token mismatch")
	}

	// Update status
	store.UpdateAccountStatus(id, "disabled")
	a, _ = store.GetAccount(id)
	if a.Status != "disabled" {
		t.Error("status not updated")
	}

	// Update token
	store.UpdateAccountToken(id, "new-tok", 99999)
	a, _ = store.GetAccount(id)
	if a.Token != "new-tok" || a.TokenExpiry != 99999 || a.Status != "active" {
		t.Error("token update failed")
	}

	// Delete
	store.DeleteAccount(id)
	accts, _ = store.ListAccounts()
	if len(accts) != 0 {
		t.Error("account not deleted")
	}
}

func TestStore_APIKeyCRUD(t *testing.T) {
	store, _ := setupTestStore(t)

	// Create
	id, err := store.CreateAPIKey("sk-gw-test123", "team-a", 500)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Get by key
	k, err := store.GetAPIKeyByKey("sk-gw-test123")
	if err != nil {
		t.Fatalf("get by key: %v", err)
	}
	if k.Name != "team-a" || k.DailyLimit != 500 {
		t.Error("key fields mismatch")
	}

	// Update
	store.UpdateAPIKey(id, 0, 200, "team-b")
	k, _ = store.GetAPIKeyByKey("sk-gw-test123")
	if k.Enabled != 0 || k.DailyLimit != 200 || k.Name != "team-b" {
		t.Error("key update failed")
	}

	// Delete
	store.DeleteAPIKey(id)
	_, err = store.GetAPIKeyByKey("sk-gw-test123")
	if err == nil {
		t.Error("key should be deleted")
	}
}

func TestStore_ProxyCRUD(t *testing.T) {
	store, _ := setupTestStore(t)

	id, err := store.CreateProxy("socks5://host:1080", "socks5")
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}

	proxies, _ := store.ListProxies()
	if len(proxies) != 1 || proxies[0].Status != "idle" {
		t.Error("proxy list mismatch")
	}

	// Batch create with duplicates
	count, err := store.CreateProxiesBatch(
		[]string{"socks5://host:1080", "socks5://host2:1080", "", "socks5://host3:1080"},
		"socks5",
	)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if count != 2 { // one duplicate skipped, one empty skipped
		t.Errorf("expected 2 new proxies, got %d", count)
	}

	proxies, _ = store.ListProxies()
	if len(proxies) != 3 {
		t.Errorf("expected 3 total proxies, got %d", len(proxies))
	}

	// Delete
	store.DeleteProxy(id)
	proxies, _ = store.ListProxies()
	if len(proxies) != 2 {
		t.Error("proxy not deleted")
	}
}

func TestStore_ProxyBinding(t *testing.T) {
	store, _ := setupTestStore(t)

	accID, _ := store.CreateAccount("acct", "tok", "", 60, 5, 0)
	proxyID, _ := store.CreateProxy("socks5://bind:1080", "socks5")

	// Bind
	if err := store.BindProxy(accID, proxyID); err != nil {
		t.Fatalf("bind: %v", err)
	}

	a, _ := store.GetAccount(accID)
	if a.ProxyID == nil || *a.ProxyID != proxyID {
		t.Error("account proxy not set")
	}

	px, _ := store.GetProxyByAccountID(accID)
	if px == nil || px.Status != "bound" {
		t.Error("proxy not bound")
	}

	// Unbind
	store.UnbindProxy(accID)
	a, _ = store.GetAccount(accID)
	if a.ProxyID != nil {
		t.Error("proxy should be unbound from account")
	}

	// Idle proxy should return
	idle, err := store.GetIdleProxy()
	if err != nil {
		t.Fatalf("get idle: %v", err)
	}
	if idle.ID != proxyID {
		t.Error("proxy should be idle again")
	}
}

func TestStore_AsyncLogWriter(t *testing.T) {
	store, _ := setupTestStore(t)

	accID, _ := store.CreateAccount("acct", "tok", "", 60, 5, 0)
	keyID, _ := store.CreateAPIKey("sk-test", "test", 1000)

	// Push multiple logs
	for i := 0; i < 50; i++ {
		store.PushLog(LogEntry{
			APIKeyID:  keyID,
			AccountID: accID,
			Model:     "claude-sonnet-4-20250514",
			Path:      "/v1/messages",
			Status:    200,
			Latency:   int64(100 + i),
			CreatedAt: time.Now().UTC(),
		})
	}

	// Wait for flush
	time.Sleep(3 * time.Second)

	logs, err := store.GetRecentLogs(100)
	if err != nil {
		t.Fatalf("get logs: %v", err)
	}
	if len(logs) < 50 {
		t.Errorf("expected >= 50 logs, got %d", len(logs))
	}
}

func TestStore_DailyUsage(t *testing.T) {
	store, _ := setupTestStore(t)

	keyID, _ := store.CreateAPIKey("sk-usage", "usage", 100)

	// Push some logs
	for i := 0; i < 5; i++ {
		store.PushLog(LogEntry{
			APIKeyID: keyID, AccountID: 1, Path: "/v1/messages",
			Status: 200, Latency: 100, CreatedAt: time.Now().UTC(),
		})
	}
	time.Sleep(3 * time.Second)

	usage, err := store.GetKeyDailyUsage(keyID)
	if err != nil {
		t.Fatalf("daily usage: %v", err)
	}
	if usage < 5 {
		t.Errorf("expected >= 5 usage, got %d", usage)
	}
}

func TestStore_OverviewStats(t *testing.T) {
	store, _ := setupTestStore(t)

	store.CreateAccount("a1", "t", "", 60, 5, 0)
	store.CreateAccount("a2", "t", "", 60, 5, 0)
	id3, _ := store.CreateAccount("a3", "t", "", 60, 5, 0)
	store.UpdateAccountStatus(id3, "disabled")
	store.CreateAPIKey("sk-1", "k1", 1000)
	store.CreateProxy("socks5://p:1080", "socks5")

	ov, err := store.GetOverviewStats()
	if err != nil {
		t.Fatal(err)
	}
	if ov.TotalAccounts != 3 || ov.ActiveAccounts != 2 {
		t.Errorf("accounts: total=%d active=%d", ov.TotalAccounts, ov.ActiveAccounts)
	}
	if ov.TotalKeys != 1 {
		t.Errorf("keys: %d", ov.TotalKeys)
	}
	if ov.TotalProxies != 1 {
		t.Errorf("proxies: %d", ov.TotalProxies)
	}
}

func TestStore_DeleteAccountUnbindsProxy(t *testing.T) {
	store, _ := setupTestStore(t)

	accID, _ := store.CreateAccount("acct", "tok", "", 60, 5, 0)
	proxyID, _ := store.CreateProxy("socks5://x:1080", "socks5")
	store.BindProxy(accID, proxyID)

	// Delete account should unbind proxy
	store.DeleteAccount(accID)

	proxies, _ := store.ListProxies()
	if len(proxies) != 1 {
		t.Fatal("proxy should still exist")
	}
	if proxies[0].Status != "idle" {
		t.Error("proxy should be idle after account deletion")
	}
}
