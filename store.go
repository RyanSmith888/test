package main

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ============================================================================
// Data Models
// ============================================================================

type Account struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Token       string `json:"token"`
	Status      string `json:"status"`       // active | disabled | refreshing | cooldown
	RPM         int    `json:"rpm"`           // requests per minute limit
	MaxConcur   int    `json:"max_concur"`    // max concurrent requests
	ProxyID     *int64 `json:"proxy_id"`      // bound proxy
	TotalReqs   int64  `json:"total_reqs"`    // lifetime request counter
	TokenExpiry int64  `json:"token_expiry"`  // unix timestamp, 0 = never
	Fingerprint  string `json:"fingerprint"`              // optional identifier
	RefreshToken string `json:"refresh_token,omitempty"` // OAuth refresh token
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type APIKey struct {
	ID         int64  `json:"id"`
	Key        string `json:"key"`
	Name       string `json:"name"`
	DailyLimit int    `json:"daily_limit"`
	Enabled    int    `json:"enabled"`
	TotalReqs  int64  `json:"total_reqs"`
	CreatedAt  string `json:"created_at"`
}

type Proxy struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	Type      string `json:"type"`       // socks5 | http
	AccountID *int64 `json:"account_id"` // bound account
	Status    string `json:"status"`     // idle | bound
	CreatedAt string `json:"created_at"`
}

type LogEntry struct {
	APIKeyID  int64
	AccountID int64
	Model     string
	Path      string
	Status    int
	Latency   int64 // milliseconds
	CreatedAt time.Time
}

type DailyStat struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	AvgLatMs int64  `json:"avg_latency_ms"`
	ErrCount int64  `json:"error_count"`
}

type KeyStat struct {
	KeyID    int64  `json:"key_id"`
	KeyName  string `json:"key_name"`
	Requests int64  `json:"requests"`
	AvgLatMs int64  `json:"avg_latency_ms"`
}

type HourlyStat struct {
	Hour     string `json:"hour"`
	Requests int64  `json:"requests"`
}

type ModelStat struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

type OverviewStats struct {
	TotalAccounts  int64 `json:"total_accounts"`
	ActiveAccounts int64 `json:"active_accounts"`
	TotalKeys      int64 `json:"total_keys"`
	TotalProxies   int64 `json:"total_proxies"`
	TodayRequests  int64 `json:"today_requests"`
	TodayErrors    int64 `json:"today_errors"`
	AvgLatencyMs   int64 `json:"avg_latency_ms"`
}

// ============================================================================
// Schema
// ============================================================================

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	token TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'active',
	rpm INTEGER NOT NULL DEFAULT 60,
	max_concur INTEGER NOT NULL DEFAULT 5,
	proxy_id INTEGER,
	total_reqs INTEGER NOT NULL DEFAULT 0,
	token_expiry INTEGER NOT NULL DEFAULT 0,
	fingerprint TEXT NOT NULL DEFAULT '',
	refresh_token TEXT NOT NULL DEFAULT '',
	created_at DATETIME DEFAULT (datetime('now')),
	updated_at DATETIME DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS api_keys (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	key TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL DEFAULT '',
	daily_limit INTEGER NOT NULL DEFAULT 1000,
	enabled INTEGER NOT NULL DEFAULT 1,
	total_reqs INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS proxies (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	url TEXT NOT NULL UNIQUE,
	type TEXT NOT NULL DEFAULT 'socks5',
	account_id INTEGER,
	status TEXT NOT NULL DEFAULT 'idle',
	created_at DATETIME DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS request_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	api_key_id INTEGER NOT NULL,
	account_id INTEGER NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_logs_created ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_logs_key ON request_logs(api_key_id);
CREATE INDEX IF NOT EXISTS idx_logs_account ON request_logs(account_id);
CREATE INDEX IF NOT EXISTS idx_logs_status ON request_logs(status);
CREATE INDEX IF NOT EXISTS idx_apikeys_key ON api_keys(key);
`

// ============================================================================
// Store
// ============================================================================

type Store struct {
	db      *sql.DB
	logChan chan LogEntry
	cfg     *Config
	wg      sync.WaitGroup
}

func NewStore(cfg *Config) (*Store, error) {
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db %q: %w", cfg.DBPath, err)
	}

	// SQLite tuning for concurrent access
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=-64000", // 64MB
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %q: %w", p, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	// Migration: add refresh_token column if missing (for existing databases)
	db.Exec("ALTER TABLE accounts ADD COLUMN refresh_token TEXT NOT NULL DEFAULT ''")


	s := &Store{
		db:      db,
		logChan: make(chan LogEntry, cfg.LogChannelSize),
		cfg:     cfg,
	}

	s.wg.Add(1)
	go s.asyncLogWriter()

	logInfo("database initialized: %s (WAL mode, %d log buffer)", cfg.DBPath, cfg.LogChannelSize)
	return s, nil
}

func (s *Store) Close() {
	close(s.logChan)
	s.wg.Wait()
	s.db.Close()
	logInfo("database closed")
}

// ============================================================================
// Async Batch Log Writer
// ============================================================================

func (s *Store) PushLog(entry LogEntry) {
	select {
	case s.logChan <- entry:
	default:
		logWarn("log channel full (%d), dropping entry", s.cfg.LogChannelSize)
	}
}

func (s *Store) asyncLogWriter() {
	defer s.wg.Done()

	batch := make([]LogEntry, 0, s.cfg.LogFlushSize)
	ticker := time.NewTicker(s.cfg.LogFlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.flushLogs(batch)
		batch = batch[:0]
	}

	for {
		select {
		case entry, ok := <-s.logChan:
			if !ok {
				flush()
				return
			}
			batch = append(batch, entry)
			if len(batch) >= s.cfg.LogFlushSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *Store) flushLogs(entries []LogEntry) {
	tx, err := s.db.Begin()
	if err != nil {
		logError("log flush begin tx: %v", err)
		return
	}

	stmt, err := tx.Prepare(
		"INSERT INTO request_logs(api_key_id, account_id, model, path, status, latency_ms, created_at) VALUES(?,?,?,?,?,?,?)",
	)
	if err != nil {
		tx.Rollback()
		logError("log flush prepare: %v", err)
		return
	}
	defer stmt.Close()

	errs := 0
	for _, e := range entries {
		if _, err := stmt.Exec(e.APIKeyID, e.AccountID, e.Model, e.Path, e.Status, e.Latency, e.CreatedAt); err != nil {
			errs++
		}
	}

	if err := tx.Commit(); err != nil {
		logError("log flush commit: %v", err)
		return
	}

	if errs > 0 {
		logWarn("log flush: %d/%d entries had errors", errs, len(entries))
	}
	logDebug("flushed %d log entries", len(entries))
}

// ============================================================================
// Account CRUD
// ============================================================================

func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`
		SELECT id, name, token, status, rpm, max_concur, proxy_id,
		       total_reqs, token_expiry, fingerprint, refresh_token, created_at, updated_at
		FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Token, &a.Status, &a.RPM, &a.MaxConcur,
			&a.ProxyID, &a.TotalReqs, &a.TokenExpiry, &a.Fingerprint, &a.RefreshToken, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAccount(id int64) (*Account, error) {
	var a Account
	err := s.db.QueryRow(`
		SELECT id, name, token, status, rpm, max_concur, proxy_id,
		       total_reqs, token_expiry, fingerprint, refresh_token, created_at, updated_at
		FROM accounts WHERE id=?`, id).Scan(&a.ID, &a.Name, &a.Token, &a.Status, &a.RPM, &a.MaxConcur,
		&a.ProxyID, &a.TotalReqs, &a.TokenExpiry, &a.Fingerprint, &a.RefreshToken, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) CreateAccount(name, token, fingerprint, refreshToken string, rpm, maxConcur int, tokenExpiry int64) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO accounts(name, token, fingerprint, refresh_token, rpm, max_concur, token_expiry)
		 VALUES(?,?,?,?,?,?,?)`,
		name, token, fingerprint, refreshToken, rpm, maxConcur, tokenExpiry,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateAccount(id int64, name, fingerprint string, rpm, maxConcur int) error {
	_, err := s.db.Exec(
		`UPDATE accounts SET name=?, fingerprint=?, rpm=?, max_concur=?, updated_at=datetime('now')
		 WHERE id=?`, name, fingerprint, rpm, maxConcur, id)
	return err
}

func (s *Store) UpdateAccountStatus(id int64, status string) error {
	_, err := s.db.Exec(
		"UPDATE accounts SET status=?, updated_at=datetime('now') WHERE id=?", status, id)
	return err
}

func (s *Store) UpdateAccountToken(id int64, token string, expiry int64) error {
	_, err := s.db.Exec(
		`UPDATE accounts SET token=?, token_expiry=?, status='active', updated_at=datetime('now')
		 WHERE id=?`, token, expiry, id)
	return err
}

func (s *Store) UpdateAccountRefreshToken(id int64, refreshToken string) error {
	_, err := s.db.Exec(
		"UPDATE accounts SET refresh_token=?, updated_at=datetime('now') WHERE id=?",
		refreshToken, id)
	return err
}

func (s *Store) IncrementAccountReqs(id int64) {
	s.db.Exec("UPDATE accounts SET total_reqs=total_reqs+1, updated_at=datetime('now') WHERE id=?", id)
}

func (s *Store) BindProxy(accountID, proxyID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE accounts SET proxy_id=?, updated_at=datetime('now') WHERE id=?", proxyID, accountID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec("UPDATE proxies SET account_id=?, status='bound' WHERE id=?", accountID, proxyID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) UnbindProxy(accountID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE proxies SET account_id=NULL, status='idle' WHERE account_id=?", accountID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec("UPDATE accounts SET proxy_id=NULL, updated_at=datetime('now') WHERE id=?", accountID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteAccount(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	// Unbind proxy first
	tx.Exec("UPDATE proxies SET account_id=NULL, status='idle' WHERE account_id=?", id)
	if _, err := tx.Exec("DELETE FROM accounts WHERE id=?", id); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ============================================================================
// API Key CRUD
// ============================================================================

func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.db.Query(
		"SELECT id, key, name, daily_limit, enabled, total_reqs, created_at FROM api_keys ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Key, &k.Name, &k.DailyLimit, &k.Enabled, &k.TotalReqs, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) GetAPIKeyByKey(key string) (*APIKey, error) {
	var k APIKey
	err := s.db.QueryRow(
		"SELECT id, key, name, daily_limit, enabled, total_reqs, created_at FROM api_keys WHERE key=?", key,
	).Scan(&k.ID, &k.Key, &k.Name, &k.DailyLimit, &k.Enabled, &k.TotalReqs, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (s *Store) CreateAPIKey(key, name string, dailyLimit int) (int64, error) {
	res, err := s.db.Exec("INSERT INTO api_keys(key, name, daily_limit) VALUES(?,?,?)", key, name, dailyLimit)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateAPIKey(id int64, enabled int, dailyLimit int, name string) error {
	_, err := s.db.Exec("UPDATE api_keys SET enabled=?, daily_limit=?, name=? WHERE id=?",
		enabled, dailyLimit, name, id)
	return err
}

func (s *Store) IncrementKeyReqs(id int64) {
	s.db.Exec("UPDATE api_keys SET total_reqs=total_reqs+1 WHERE id=?", id)
}

func (s *Store) GetKeyDailyUsage(keyID int64) (int64, error) {
	var count int64
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM request_logs WHERE api_key_id=? AND created_at >= date('now')", keyID,
	).Scan(&count)
	return count, err
}

func (s *Store) DeleteAPIKey(id int64) error {
	_, err := s.db.Exec("DELETE FROM api_keys WHERE id=?", id)
	return err
}

// ============================================================================
// Proxy CRUD
// ============================================================================

func (s *Store) ListProxies() ([]Proxy, error) {
	rows, err := s.db.Query(
		"SELECT id, url, type, account_id, status, created_at FROM proxies ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Proxy
	for rows.Next() {
		var p Proxy
		if err := rows.Scan(&p.ID, &p.URL, &p.Type, &p.AccountID, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProxyByAccountID(accountID int64) (*Proxy, error) {
	var p Proxy
	err := s.db.QueryRow(
		"SELECT id, url, type, account_id, status, created_at FROM proxies WHERE account_id=?", accountID,
	).Scan(&p.ID, &p.URL, &p.Type, &p.AccountID, &p.Status, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) GetIdleProxy() (*Proxy, error) {
	var p Proxy
	err := s.db.QueryRow(
		"SELECT id, url, type, account_id, status, created_at FROM proxies WHERE status='idle' ORDER BY id LIMIT 1",
	).Scan(&p.ID, &p.URL, &p.Type, &p.AccountID, &p.Status, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) CreateProxy(proxyURL, ptype string) (int64, error) {
	res, err := s.db.Exec("INSERT INTO proxies(url, type) VALUES(?,?)", proxyURL, ptype)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) CreateProxiesBatch(urls []string, ptype string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare("INSERT OR IGNORE INTO proxies(url, type) VALUES(?,?)")
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	count := 0
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		res, err := stmt.Exec(u, ptype)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			count++
		}
	}
	return count, tx.Commit()
}

func (s *Store) DeleteProxy(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	// Unbind from account
	tx.Exec("UPDATE accounts SET proxy_id=NULL WHERE proxy_id=?", id)
	if _, err := tx.Exec("DELETE FROM proxies WHERE id=?", id); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ============================================================================
// Statistics Queries
// ============================================================================

func (s *Store) GetOverviewStats() (*OverviewStats, error) {
	var o OverviewStats
	s.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&o.TotalAccounts)
	s.db.QueryRow("SELECT COUNT(*) FROM accounts WHERE status='active'").Scan(&o.ActiveAccounts)
	s.db.QueryRow("SELECT COUNT(*) FROM api_keys WHERE enabled=1").Scan(&o.TotalKeys)
	s.db.QueryRow("SELECT COUNT(*) FROM proxies").Scan(&o.TotalProxies)
	s.db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE created_at >= date('now')").Scan(&o.TodayRequests)
	s.db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE created_at >= date('now') AND status >= 400").Scan(&o.TodayErrors)
	s.db.QueryRow("SELECT COALESCE(AVG(latency_ms),0) FROM request_logs WHERE created_at >= date('now')").Scan(&o.AvgLatencyMs)
	return &o, nil
}

func (s *Store) GetDailyStats(days int) ([]DailyStat, error) {
	rows, err := s.db.Query(`
		SELECT date(created_at) AS d,
		       COUNT(*) AS cnt,
		       COALESCE(AVG(latency_ms), 0) AS avg_lat,
		       SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END) AS errs
		FROM request_logs
		WHERE created_at >= date('now', printf('-%d days', ?))
		GROUP BY d ORDER BY d`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DailyStat
	for rows.Next() {
		var s DailyStat
		if err := rows.Scan(&s.Date, &s.Requests, &s.AvgLatMs, &s.ErrCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) GetHourlyStats(hours int) ([]HourlyStat, error) {
	rows, err := s.db.Query(`
		SELECT strftime('%Y-%m-%d %H:00', created_at) AS h, COUNT(*) AS cnt
		FROM request_logs
		WHERE created_at >= datetime('now', printf('-%d hours', ?))
		GROUP BY h ORDER BY h`, hours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HourlyStat
	for rows.Next() {
		var s HourlyStat
		if err := rows.Scan(&s.Hour, &s.Requests); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) GetKeyStats(days int) ([]KeyStat, error) {
	rows, err := s.db.Query(`
		SELECT k.id, COALESCE(k.name, 'unknown'), COUNT(*), COALESCE(AVG(l.latency_ms), 0)
		FROM request_logs l
		LEFT JOIN api_keys k ON k.id = l.api_key_id
		WHERE l.created_at >= date('now', printf('-%d days', ?))
		GROUP BY l.api_key_id ORDER BY COUNT(*) DESC`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []KeyStat
	for rows.Next() {
		var s KeyStat
		if err := rows.Scan(&s.KeyID, &s.KeyName, &s.Requests, &s.AvgLatMs); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) GetModelStats(days int) ([]ModelStat, error) {
	rows, err := s.db.Query(`
		SELECT COALESCE(NULLIF(model,''), 'unknown'), COUNT(*)
		FROM request_logs
		WHERE created_at >= date('now', printf('-%d days', ?))
		GROUP BY model ORDER BY COUNT(*) DESC`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelStat
	for rows.Next() {
		var s ModelStat
		if err := rows.Scan(&s.Model, &s.Requests); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) GetRecentLogs(limit int) ([]map[string]any, error) {
	rows, err := s.db.Query(`
		SELECT l.id, COALESCE(k.name,'?') AS key_name, COALESCE(a.name,'?') AS acc_name,
		       l.model, l.path, l.status, l.latency_ms, l.created_at
		FROM request_logs l
		LEFT JOIN api_keys k ON k.id = l.api_key_id
		LEFT JOIN accounts a ON a.id = l.account_id
		ORDER BY l.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id int64
		var keyName, accName, model, path, createdAt string
		var status int
		var latency int64
		if err := rows.Scan(&id, &keyName, &accName, &model, &path, &status, &latency, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id":         id,
			"key_name":   keyName,
			"account":    accName,
			"model":      model,
			"path":       path,
			"status":     status,
			"latency_ms": latency,
			"created_at": createdAt,
		})
	}
	return out, rows.Err()
}

// PurgeOldLogs removes logs older than the given number of days.
func (s *Store) PurgeOldLogs(days int) (int64, error) {
	res, err := s.db.Exec(
		"DELETE FROM request_logs WHERE created_at < date('now', printf('-%d days', ?))", days)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
