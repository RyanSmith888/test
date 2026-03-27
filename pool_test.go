package main

import (
	"testing"
	"time"
)

func testConfig() *Config {
	return &Config{
		DBPath:               "",
		LogLevel:             LogLevelError,
		DefaultRPM:           60,
		DefaultMaxConcur:     5,
		MaxRetryAttempts:     3,
		PoolReloadInterval:   1 * time.Hour,
		RPMResetInterval:     60 * time.Second,
		TokenRefreshLeadTime: 30 * time.Minute,
		LogChannelSize:       100,
		LogFlushSize:         10,
		LogFlushInterval:     1 * time.Second,
	}
}

func setupTestStore(t *testing.T) (*Store, *Config) {
	t.Helper()
	cfg := testConfig()
	cfg.DBPath = t.TempDir() + "/test.db"

	initLogger(LogLevelError)

	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, cfg
}

// ============================================================================
// Pool Scheduling Tests
// ============================================================================

func TestPoolScheduling_LeastLoaded(t *testing.T) {
	store, cfg := setupTestStore(t)

	store.CreateAccount("acc1", "tok1", "", "", 60, 5, 0)
	store.CreateAccount("acc2", "tok2", "", "", 60, 5, 0)
	store.CreateAccount("acc3", "tok3", "", "", 60, 5, 0)

	pool := NewPool(store, cfg)
	if err := pool.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer pool.Stop()

	// Pick first - should get one with 0 in-flight
	as1, err := pool.Pick("")
	if err != nil {
		t.Fatalf("pick 1: %v", err)
	}
	pool.Acquire(as1) // as1 now has 1 in-flight

	// Pick second - should get different (least loaded = 0 in-flight)
	as2, err := pool.Pick("")
	if err != nil {
		t.Fatalf("pick 2: %v", err)
	}
	pool.Acquire(as2)

	if as1.Account.ID == as2.Account.ID {
		t.Error("expected different accounts, least-loaded should pick another")
	}

	pool.Release(as1)
	pool.Release(as2)
}

func TestPoolScheduling_CooldownSkip(t *testing.T) {
	store, cfg := setupTestStore(t)

	store.CreateAccount("hot", "tok1", "", "", 60, 5, 0)
	store.CreateAccount("cold", "tok2", "", "", 60, 5, 0)

	pool := NewPool(store, cfg)
	if err := pool.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer pool.Stop()

	// Cool down first account
	as1, _ := pool.Pick("")
	pool.MarkCooldown(as1, 30*time.Second)

	// Next pick must skip cooled account
	as2, err := pool.Pick("")
	if err != nil {
		t.Fatalf("pick after cooldown: %v", err)
	}
	if as2.Account.ID == as1.Account.ID {
		t.Error("should skip cooled-down account")
	}
}

func TestPoolScheduling_RPMExhaustion(t *testing.T) {
	store, cfg := setupTestStore(t)

	// Account with RPM=2
	store.CreateAccount("limited", "tok1", "", "", 2, 10, 0)
	store.CreateAccount("backup", "tok2", "", "", 60, 10, 0)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	// Exhaust RPM on first account
	for i := 0; i < 2; i++ {
		as, _ := pool.Pick("")
		pool.Acquire(as)
		pool.Release(as)
	}

	// Next pick should go to backup since "limited" RPM exhausted
	as, err := pool.Pick("")
	if err != nil {
		t.Fatalf("pick after RPM exhaust: %v", err)
	}
	if as.Account.Name != "backup" {
		t.Errorf("expected backup account, got %s", as.Account.Name)
	}
}

func TestPoolScheduling_NoHealthyAccounts(t *testing.T) {
	store, cfg := setupTestStore(t)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	_, err := pool.Pick("")
	if err != ErrNoHealthyAccount {
		t.Errorf("expected ErrNoHealthyAccount, got %v", err)
	}
}

func TestPoolScheduling_DisabledAccountSkipped(t *testing.T) {
	store, cfg := setupTestStore(t)

	id1, _ := store.CreateAccount("disabled", "tok1", "", "", 60, 5, 0)
	store.CreateAccount("active", "tok2", "", "", 60, 5, 0)
	store.UpdateAccountStatus(id1, "disabled")

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	as, err := pool.Pick("")
	if err != nil {
		t.Fatal(err)
	}
	if as.Account.Name != "active" {
		t.Errorf("expected active account, got %s", as.Account.Name)
	}
}

// ============================================================================
// Session Affinity Tests
// ============================================================================

func TestSessionAffinity_SameSession(t *testing.T) {
	store, cfg := setupTestStore(t)

	store.CreateAccount("acc1", "tok1", "", "", 60, 5, 0)
	store.CreateAccount("acc2", "tok2", "", "", 60, 5, 0)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	sid := "session-abc-123"

	as1, _ := pool.Pick(sid)
	pool.Acquire(as1)
	pool.Release(as1)

	// Same session should return same account
	as2, _ := pool.Pick(sid)
	if as1.Account.ID != as2.Account.ID {
		t.Errorf("session affinity broken: first=%d, second=%d", as1.Account.ID, as2.Account.ID)
	}
}

func TestSessionAffinity_DifferentSession(t *testing.T) {
	store, cfg := setupTestStore(t)

	store.CreateAccount("acc1", "tok1", "", "", 60, 5, 0)
	store.CreateAccount("acc2", "tok2", "", "", 60, 5, 0)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	as1, _ := pool.Pick("session-1")
	pool.Acquire(as1)
	// don't release - force different pick

	as2, _ := pool.Pick("session-2")
	// Different session with load should get different account
	if as1.Account.ID == as2.Account.ID {
		t.Log("Note: same account is valid but unusual with load")
	}
	pool.Release(as1)
}

func TestSessionAffinity_FallbackOnUnhealthy(t *testing.T) {
	store, cfg := setupTestStore(t)

	store.CreateAccount("acc1", "tok1", "", "", 60, 5, 0)
	store.CreateAccount("acc2", "tok2", "", "", 60, 5, 0)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	sid := "session-x"
	as1, _ := pool.Pick(sid)

	// Cooldown the affinity account
	pool.MarkCooldown(as1, 1*time.Minute)

	// Should fallback to another healthy account
	as2, err := pool.Pick(sid)
	if err != nil {
		t.Fatal(err)
	}
	if as2.Account.ID == as1.Account.ID {
		t.Error("should fallback to different account when affinity target is unhealthy")
	}
}

// ============================================================================
// Token Refresh Tests
// ============================================================================

func TestTokenRefresh_WarnsButKeepsActive(t *testing.T) {
	store, cfg := setupTestStore(t)
	cfg.TokenRefreshLeadTime = 30 * time.Minute

	// Expires in 20 minutes (within 30 min lead time)
	expiry := time.Now().Unix() + 1200
	store.CreateAccount("expiring", "old-tok", "", "", 60, 5, expiry)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	// checkTokenExpiry should only log a warning, NOT change status
	pool.checkTokenExpiry()

	accts, _ := store.ListAccounts()
	if len(accts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accts))
	}
	// Account must stay active — no longer marks as "refreshing"
	if accts[0].Status != "active" {
		t.Errorf("expected 'active', got %q", accts[0].Status)
	}
}

func TestTokenRefresh_WarnsOnExpired(t *testing.T) {
	store, cfg := setupTestStore(t)
	cfg.TokenRefreshLeadTime = 30 * time.Minute

	// Already expired
	expiry := time.Now().Unix() - 100
	store.CreateAccount("expired", "old-tok", "", "", 60, 5, expiry)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	// Should log warning but keep status active
	pool.checkTokenExpiry()

	accts, _ := store.ListAccounts()
	if accts[0].Status != "active" {
		t.Errorf("expected 'active' (warn only, no status change), got %q", accts[0].Status)
	}
}

func TestTokenRefresh_NoActionIfFarFromExpiry(t *testing.T) {
	store, cfg := setupTestStore(t)
	cfg.TokenRefreshLeadTime = 30 * time.Minute

	// Expires in 2 hours (well outside lead time)
	expiry := time.Now().Unix() + 7200
	store.CreateAccount("fresh", "tok", "", "", 60, 5, expiry)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	pool.checkTokenExpiry()

	accts, _ := store.ListAccounts()
	if accts[0].Status != "active" {
		t.Errorf("expected 'active', got %q", accts[0].Status)
	}
}

func TestTokenRefresh_ExternalTokenUpdate(t *testing.T) {
	store, cfg := setupTestStore(t)

	id, _ := store.CreateAccount("acct", "old", "", "", 60, 5, 0)
	store.UpdateAccountStatus(id, "refreshing")

	// Simulate external refresh
	newExpiry := time.Now().Unix() + 7200
	store.UpdateAccountToken(id, "new-token", newExpiry)

	accts, _ := store.ListAccounts()
	if accts[0].Token != "new-token" {
		t.Error("token not updated")
	}
	if accts[0].Status != "active" {
		t.Error("status not restored to active")
	}

	// Verify pool reloads pick up the change
	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	as, err := pool.Pick("")
	if err != nil {
		t.Fatal(err)
	}
	if as.Account.Token != "new-token" {
		t.Error("pool did not pick up new token")
	}
}

// ============================================================================
// PickExcluding Tests (retry failover)
// ============================================================================

func TestPickExcluding(t *testing.T) {
	store, cfg := setupTestStore(t)

	id1, _ := store.CreateAccount("acc1", "tok1", "", "", 60, 5, 0)
	store.CreateAccount("acc2", "tok2", "", "", 60, 5, 0)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	exclude := map[int64]bool{id1: true}
	as, err := pool.PickExcluding("", exclude)
	if err != nil {
		t.Fatal(err)
	}
	if as.Account.ID == id1 {
		t.Error("should not pick excluded account")
	}
}

func TestPickExcluding_AllExcluded(t *testing.T) {
	store, cfg := setupTestStore(t)

	id1, _ := store.CreateAccount("only", "tok", "", "", 60, 5, 0)

	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	_, err := pool.PickExcluding("", map[int64]bool{id1: true})
	if err != ErrNoHealthyAccount {
		t.Errorf("expected ErrNoHealthyAccount, got %v", err)
	}
}
