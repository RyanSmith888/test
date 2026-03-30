-- 虚拟身份表：存储每个账号的持久化虚拟身份
CREATE TABLE IF NOT EXISTS virtual_identities (
    account_id BIGINT PRIMARY KEY,
    client_id VARCHAR(255) NOT NULL UNIQUE,
    device_id VARCHAR(255) NOT NULL,
    session_seed VARCHAR(255) NOT NULL,
    user_agent TEXT NOT NULL,
    os_type VARCHAR(50) NOT NULL,
    architecture VARCHAR(50) NOT NULL,
    runtime_ver VARCHAR(50) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 会话绑定表：为每个账号维护持久化的会话关联
CREATE TABLE IF NOT EXISTS account_session_bindings (
    account_id BIGINT PRIMARY KEY,
    fixed_session_id VARCHAR(255) NOT NULL,
    last_used_at TIMESTAMPTZ,
    request_count BIGINT DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 账号健康分表：跟踪每个账号的错误率和健康状态
CREATE TABLE IF NOT EXISTS account_health_scores (
    account_id BIGINT PRIMARY KEY,
    success_count BIGINT DEFAULT 0,
    failure_count BIGINT DEFAULT 0,
    error_401_count BIGINT DEFAULT 0,
    error_403_count BIGINT DEFAULT 0,
    health_score DECIMAL(3, 2) DEFAULT 0.50,
    allowed_concurrency INT DEFAULT 1,
    last_check_time TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 索引：加速按健康分排序查询
CREATE INDEX IF NOT EXISTS idx_account_health_scores_health
    ON account_health_scores (health_score DESC);

-- 索引：加速会话绑定查询
CREATE INDEX IF NOT EXISTS idx_account_session_bindings_session_id
    ON account_session_bindings (fixed_session_id);
