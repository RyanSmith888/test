package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var ErrNoHealthyAccount = errors.New("no healthy account available")

// ============================================================================
// AccountState: runtime state for one upstream account
// ============================================================================

type AccountState struct {
	Account   Account
	InFlight  atomic.Int64
	RPMCount  atomic.Int64
	CoolUntil time.Time
	ProxyURL  string
	mu        sync.Mutex
}

func (as *AccountState) setCooldown(d time.Duration) {
	as.mu.Lock()
	as.CoolUntil = time.Now().Add(d)
	as.mu.Unlock()
}

func (as *AccountState) isCooling() bool {
	as.mu.Lock()
	c := time.Now().Before(as.CoolUntil)
	as.mu.Unlock()
	return c
}

// ============================================================================
// Pool: manages all upstream accounts
// ============================================================================

type Pool struct {
	store      *Store
	cfg        *Config
	accounts   []*AccountState
	sessionMap sync.Map // session_id -> account_id
	mu         sync.RWMutex
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

func NewPool(store *Store, cfg *Config) *Pool {
	return &Pool{
		store:  store,
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
}

func (p *Pool) Start() error {
	if err := p.Reload(); err != nil {
		return err
	}

	p.wg.Add(2)
	go p.backgroundLoop()
	go p.rpmResetLoop()

	logInfo("pool started with %d accounts", len(p.accounts))
	return nil
}

func (p *Pool) Stop() {
	close(p.stopCh)
	p.wg.Wait()
	logInfo("pool stopped")
}

// Reload fetches accounts from DB and syncs runtime state.
// Preserves in-flight counts and cooldowns for existing accounts.
func (p *Pool) Reload() error {
	accounts, err := p.store.ListAccounts()
	if err != nil {
		return err
	}

	// Also load proxy URLs
	proxies, _ := p.store.ListProxies()
	proxyMap := make(map[int64]string) // account_id -> proxy_url
	for _, px := range proxies {
		if px.AccountID != nil {
			proxyMap[*px.AccountID] = px.URL
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	existing := make(map[int64]*AccountState, len(p.accounts))
	for _, as := range p.accounts {
		existing[as.Account.ID] = as
	}

	newStates := make([]*AccountState, 0, len(accounts))
	for _, a := range accounts {
		if prev, ok := existing[a.ID]; ok {
			prev.Account = a
			prev.ProxyURL = proxyMap[a.ID]
			newStates = append(newStates, prev)
		} else {
			as := &AccountState{
				Account:  a,
				ProxyURL: proxyMap[a.ID],
			}
			newStates = append(newStates, as)
		}
	}

	p.accounts = newStates
	return nil
}

// Pick selects the best available account.
// Session affinity is attempted first; falls back to least-loaded selection.
func (p *Pool) Pick(sessionID string) (*AccountState, error) {
	// 1. Session affinity
	if sessionID != "" {
		if val, ok := p.sessionMap.Load(sessionID); ok {
			if as := p.findByID(val.(int64)); as != nil && p.isHealthy(as) {
				return as, nil
			}
		}
	}

	// 2. Least-loaded healthy account
	p.mu.RLock()
	defer p.mu.RUnlock()

	var best *AccountState
	var bestLoad int64 = 1<<63 - 1

	for _, as := range p.accounts {
		if !p.isHealthy(as) {
			continue
		}
		load := as.InFlight.Load()
		if load < bestLoad {
			bestLoad = load
			best = as
		}
	}

	if best == nil {
		return nil, ErrNoHealthyAccount
	}

	// Bind session
	if sessionID != "" {
		p.sessionMap.Store(sessionID, best.Account.ID)
	}

	// Auto-bind proxy
	if best.Account.ProxyID == nil && best.ProxyURL == "" {
		p.tryBindProxy(best)
	}

	return best, nil
}

// PickExcluding selects an account excluding the given IDs (for retry failover).
func (p *Pool) PickExcluding(sessionID string, excludeIDs map[int64]bool) (*AccountState, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var best *AccountState
	var bestLoad int64 = 1<<63 - 1

	for _, as := range p.accounts {
		if excludeIDs[as.Account.ID] {
			continue
		}
		if !p.isHealthy(as) {
			continue
		}
		load := as.InFlight.Load()
		if load < bestLoad {
			bestLoad = load
			best = as
		}
	}

	if best == nil {
		return nil, ErrNoHealthyAccount
	}

	if sessionID != "" {
		p.sessionMap.Store(sessionID, best.Account.ID)
	}

	if best.Account.ProxyID == nil && best.ProxyURL == "" {
		p.tryBindProxy(best)
	}

	return best, nil
}

func (p *Pool) Acquire(as *AccountState) {
	as.InFlight.Add(1)
	as.RPMCount.Add(1)
}

func (p *Pool) Release(as *AccountState) {
	as.InFlight.Add(-1)
}

func (p *Pool) MarkCooldown(as *AccountState, d time.Duration) {
	as.setCooldown(d)
	logWarn("account %d [%s] cooling down for %v", as.Account.ID, as.Account.Name, d)
}

func (p *Pool) isHealthy(as *AccountState) bool {
	if as.Account.Status != "active" {
		return false
	}
	if as.isCooling() {
		return false
	}
	if as.InFlight.Load() >= int64(as.Account.MaxConcur) {
		return false
	}
	if as.RPMCount.Load() >= int64(as.Account.RPM) {
		return false
	}
	return true
}

func (p *Pool) findByID(id int64) *AccountState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, as := range p.accounts {
		if as.Account.ID == id {
			return as
		}
	}
	return nil
}

func (p *Pool) tryBindProxy(as *AccountState) {
	proxy, err := p.store.GetIdleProxy()
	if err != nil {
		return
	}
	if err := p.store.BindProxy(as.Account.ID, proxy.ID); err != nil {
		logError("bind proxy %d -> account %d: %v", proxy.ID, as.Account.ID, err)
		return
	}
	as.Account.ProxyID = &proxy.ID
	as.ProxyURL = proxy.URL
	logInfo("bound proxy %d [%s] -> account %d [%s]", proxy.ID, proxy.URL, as.Account.ID, as.Account.Name)
}

// backgroundLoop handles periodic reload and token expiry checks.
func (p *Pool) backgroundLoop() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.cfg.PoolReloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.checkTokenExpiry()
			if err := p.Reload(); err != nil {
				logError("pool reload: %v", err)
			}
		}
	}
}

func (p *Pool) checkTokenExpiry() {
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now().Unix()
	leadSec := int64(p.cfg.TokenRefreshLeadTime.Seconds())

	for _, as := range p.accounts {
		if as.Account.TokenExpiry == 0 || as.Account.Status != "active" {
			continue
		}
		remaining := as.Account.TokenExpiry - now
		if remaining > 0 && remaining < leadSec {
			logWarn("account %d [%s] token expires in %ds, marking refreshing",
				as.Account.ID, as.Account.Name, remaining)
			p.store.UpdateAccountStatus(as.Account.ID, "refreshing")
		}
	}
}

func (p *Pool) rpmResetLoop() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.cfg.RPMResetInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.mu.RLock()
			for _, as := range p.accounts {
				as.RPMCount.Store(0)
			}
			p.mu.RUnlock()
		}
	}
}

// GetStates returns a snapshot for the admin dashboard.
func (p *Pool) GetStates() []map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]map[string]any, 0, len(p.accounts))
	for _, as := range p.accounts {
		as.mu.Lock()
		coolUntil := as.CoolUntil
		as.mu.Unlock()

		cooling := time.Until(coolUntil).Seconds()
		if cooling < 0 {
			cooling = 0
		}

		out = append(out, map[string]any{
			"id":              as.Account.ID,
			"name":            as.Account.Name,
			"status":          as.Account.Status,
			"in_flight":       as.InFlight.Load(),
			"rpm_count":       as.RPMCount.Load(),
			"rpm_limit":       as.Account.RPM,
			"max_concur":      as.Account.MaxConcur,
			"proxy_url":       as.ProxyURL,
			"cool_remaining_s": int(cooling),
			"total_reqs":      as.Account.TotalReqs,
			"token_expiry":    as.Account.TokenExpiry,
			"fingerprint":     as.Account.Fingerprint,
		})
	}
	return out
}

// AccountCount returns the total number of loaded accounts.
func (p *Pool) AccountCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}
