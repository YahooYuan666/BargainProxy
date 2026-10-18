package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/model"
)

// Postgres is the PostgreSQL-backed store. Credentials are sealed with the
// crypto envelope before touching the database. All statements use
// parameter binding.
type Postgres struct {
	pool *pgxpool.Pool
	box  *cryptox.Box
}

// DDL is the schema for a fresh database. Applied idempotently at Open.
const DDL = `
CREATE TABLE IF NOT EXISTS accounts (
    id            TEXT PRIMARY KEY,
    channel       TEXT NOT NULL,
    uid           TEXT NOT NULL DEFAULT '',
    nickname      TEXT NOT NULL DEFAULT '',
    realm         TEXT NOT NULL DEFAULT '',
    project_id    TEXT NOT NULL DEFAULT '',
    creds_enc     TEXT NOT NULL DEFAULT '',
    disabled      BOOLEAN NOT NULL DEFAULT FALSE,
    state         JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS accounts_channel_idx ON accounts (channel);

CREATE TABLE IF NOT EXISTS request_logs (
    id            BIGSERIAL PRIMARY KEY,
    ts            TIMESTAMPTZ NOT NULL,
    request_id    TEXT NOT NULL DEFAULT '',
    protocol      TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    channel       TEXT NOT NULL DEFAULT '',
    account_id    TEXT NOT NULL DEFAULT '',
    account_label TEXT NOT NULL DEFAULT '',
    status        INT  NOT NULL DEFAULT 0,
    error_class   TEXT NOT NULL DEFAULT '',
    ttfb_ms       INT  NOT NULL DEFAULT 0,
    duration_ms   INT  NOT NULL DEFAULT 0,
    input_tokens  INT  NOT NULL DEFAULT 0,
    output_tokens INT  NOT NULL DEFAULT 0,
    stream        BOOLEAN NOT NULL DEFAULT FALSE,
    client_ip     TEXT NOT NULL DEFAULT '',
    user_agent    TEXT NOT NULL DEFAULT '',
    degraded      TEXT NOT NULL DEFAULT '',
    attempts        INT NOT NULL DEFAULT 0,
    attempt_channels TEXT NOT NULL DEFAULT '',
    credit          NUMERIC(14,4) NOT NULL DEFAULT 0,
    credit_known    BOOLEAN NOT NULL DEFAULT FALSE,
    cached_tokens   INT NOT NULL DEFAULT 0,
    cache_known     BOOLEAN NOT NULL DEFAULT FALSE
);
-- Idempotent column additions for databases created before BP-38/BP-40/BP-53.
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS attempt_channels TEXT NOT NULL DEFAULT '';
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS credit NUMERIC(14,4) NOT NULL DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS credit_known BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS cached_tokens INT NOT NULL DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS cache_known BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS request_logs_ts_idx ON request_logs (ts);

CREATE TABLE IF NOT EXISTS task_runs (
    id          BIGSERIAL PRIMARY KEY,
    account_id  TEXT NOT NULL,
    task_type   TEXT NOT NULL,
    run_date    TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    credits     BIGINT NOT NULL DEFAULT 0,
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL,
    UNIQUE (account_id, task_type, run_date)
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);
`

// OpenPostgres connects, applies the schema, and returns the store.
func OpenPostgres(ctx context.Context, dsn string, box *cryptox.Box) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if _, err := pool.Exec(ctx, DDL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	return &Postgres{pool: pool, box: box}, nil
}

// stateBlob is the JSONB column shape: everything except creds + identity.
type stateBlob struct {
	UpdatedAt            time.Time                        `json:"updated_at,omitempty"`
	Paused               bool                             `json:"paused,omitempty"`
	AccountCooldownUntil time.Time                        `json:"account_cooldown_until,omitempty"`
	ModelCooldowns       map[string]model.ModelCooldown   `json:"model_cooldowns,omitempty"`
	Windows              map[string]model.Window          `json:"windows,omitempty"`
	Credits              model.Credits                    `json:"credits,omitempty"`
	CostTiers            map[string]model.CostObservation `json:"cost_tiers,omitempty"`
	ConsecutiveFails     int                              `json:"consecutive_fails,omitempty"`
	BreakerFails         int                              `json:"breaker_fails,omitempty"`
	LastUsedAt           time.Time                        `json:"last_used_at,omitempty"`
}

func (p *Postgres) LoadAccounts() ([]*model.Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := p.pool.Query(ctx,
		`SELECT id, channel, uid, nickname, realm, project_id, creds_enc, disabled, state FROM accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Account
	for rows.Next() {
		var (
			a     model.Account
			creds string
			state []byte
		)
		if err := rows.Scan(&a.ID, &a.Channel, &a.UID, &a.Nickname, &a.Realm, &a.ProjectID, &creds, &a.Disabled, &state); err != nil {
			return nil, err
		}
		plain, err := p.box.Open(creds)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt account %s: %w", a.ID, err)
		}
		a.CredsJSON = plain
		if len(state) > 0 {
			var sb stateBlob
			if err := json.Unmarshal(state, &sb); err != nil {
				return nil, fmt.Errorf("store: account %s state: %w", a.ID, err)
			}
			a.Paused = sb.Paused
			a.AccountCooldownUntil = sb.AccountCooldownUntil
			a.ModelCooldowns = sb.ModelCooldowns
			a.Windows = sb.Windows
			a.Credits = sb.Credits
			a.CostTiers = sb.CostTiers
			a.ConsecutiveFails = sb.ConsecutiveFails
			a.BreakerFails = sb.BreakerFails
			a.LastUsedAt = sb.LastUsedAt
			a.UpdatedAt = sb.UpdatedAt
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (p *Postgres) SaveAccount(acc *model.Account) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sealed, err := p.box.Seal(acc.Credentials())
	if err != nil {
		return fmt.Errorf("store: encrypt account %s: %w", acc.ID, err)
	}
	sb := stateBlob{
		UpdatedAt:            acc.UpdatedAt,
		Paused:               acc.Paused,
		AccountCooldownUntil: acc.AccountCooldownUntil,
		ModelCooldowns:       acc.ModelCooldowns,
		Windows:              acc.Windows,
		Credits:              acc.Credits,
		CostTiers:            acc.CostTiers,
		ConsecutiveFails:     acc.ConsecutiveFails,
		BreakerFails:         acc.BreakerFails,
		LastUsedAt:           acc.LastUsedAt,
	}
	state, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
        INSERT INTO accounts (id, channel, uid, nickname, realm, project_id, creds_enc, disabled, state, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
        ON CONFLICT (id) DO UPDATE SET
            channel = EXCLUDED.channel,
            uid = EXCLUDED.uid,
            nickname = EXCLUDED.nickname,
            realm = EXCLUDED.realm,
            project_id = EXCLUDED.project_id,
            creds_enc = EXCLUDED.creds_enc,
            disabled = EXCLUDED.disabled,
            state = EXCLUDED.state,
            updated_at = now()`,
		acc.ID, acc.Channel, acc.UID, acc.Nickname, acc.Realm, acc.ProjectID, sealed, acc.Disabled, state)
	return err
}

func (p *Postgres) DeleteAccount(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id)
	return err
}

func (p *Postgres) AppendRequestLog(l *model.RequestLog) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.pool.Exec(ctx, `
        INSERT INTO request_logs (ts, request_id, protocol, model, channel, account_id, account_label,
            status, error_class, ttfb_ms, duration_ms, input_tokens, output_tokens, stream, client_ip, user_agent, degraded,
            attempts, attempt_channels, credit, credit_known, cached_tokens, cache_known)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		l.Time, l.RequestID, l.Protocol, l.Model, l.Channel, l.AccountID, l.AccountLabel,
		l.Status, l.ErrorClass, l.TTFBMs, l.DurationMs, l.InputTokens, l.OutputTokens, l.Stream, l.ClientIP, l.UserAgent, l.Degraded,
		l.Attempts, l.AttemptChannels, l.Credit, l.CreditKnown, l.CachedTokens, l.CacheKnown)
	return err
}

func (p *Postgres) RecentRequestLogs(limit int) ([]model.RequestLog, error) {
	// The usage page scans up to limit=20000 rows; a silent 200-row clamp
	// here made "全部时间" aggregate only the newest 200 requests (ticket 29).
	if limit <= 0 {
		limit = 200
	}
	if limit > 20000 {
		limit = 20000
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := p.pool.Query(ctx, `
        SELECT id, ts, request_id, protocol, model, channel, account_id, account_label, status,
               error_class, ttfb_ms, duration_ms, input_tokens, output_tokens, stream, client_ip, user_agent, degraded,
               attempts, attempt_channels, credit, credit_known, cached_tokens, cache_known
        FROM request_logs ORDER BY ts DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RequestLog
	for rows.Next() {
		var l model.RequestLog
		if err := rows.Scan(&l.ID, &l.Time, &l.RequestID, &l.Protocol, &l.Model, &l.Channel, &l.AccountID, &l.AccountLabel,
			&l.Status, &l.ErrorClass, &l.TTFBMs, &l.DurationMs, &l.InputTokens, &l.OutputTokens, &l.Stream, &l.ClientIP, &l.UserAgent, &l.Degraded,
			&l.Attempts, &l.AttemptChannels, &l.Credit, &l.CreditKnown, &l.CachedTokens, &l.CacheKnown); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (p *Postgres) PurgeRequestLogs(before time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tag, err := p.pool.Exec(ctx, `DELETE FROM request_logs WHERE ts < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (p *Postgres) UpsertTaskRun(t *model.TaskRun) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.pool.Exec(ctx, `
        INSERT INTO task_runs (account_id, task_type, run_date, status, detail, credits, started_at, finished_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
        ON CONFLICT (account_id, task_type, run_date) DO UPDATE SET
            status = EXCLUDED.status, detail = EXCLUDED.detail, credits = EXCLUDED.credits,
            finished_at = EXCLUDED.finished_at`,
		t.AccountID, t.TaskType, t.RunDate, t.Status, t.Detail, t.Credits, t.StartedAt, t.FinishedAt)
	return err
}

func (p *Postgres) TaskRunFor(accountID, taskType, runDate string) (*model.TaskRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var t model.TaskRun
	err := p.pool.QueryRow(ctx, `
        SELECT id, account_id, task_type, run_date, status, detail, credits, started_at, finished_at
        FROM task_runs WHERE account_id=$1 AND task_type=$2 AND run_date=$3`,
		accountID, taskType, runDate).
		Scan(&t.ID, &t.AccountID, &t.TaskType, &t.RunDate, &t.Status, &t.Detail, &t.Credits, &t.StartedAt, &t.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (p *Postgres) RecentTaskRuns(limit int) ([]model.TaskRun, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := p.pool.Query(ctx, `
        SELECT id, account_id, task_type, run_date, status, detail, credits, started_at, finished_at
        FROM task_runs ORDER BY started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TaskRun
	for rows.Next() {
		var t model.TaskRun
		if err := rows.Scan(&t.ID, &t.AccountID, &t.TaskType, &t.RunDate, &t.Status, &t.Detail, &t.Credits, &t.StartedAt, &t.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PurgeTaskRuns drops ledger rows that started before the cutoff (the same
// retention window as request logs; keeps the ledger from growing forever).
func (p *Postgres) PurgeTaskRuns(before time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tag, err := p.pool.Exec(ctx, `DELETE FROM task_runs WHERE started_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (p *Postgres) GetSetting(key string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var v string
	err := p.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (p *Postgres) SetSetting(key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.pool.Exec(ctx, `
        INSERT INTO settings (key, value) VALUES ($1,$2)
        ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value)
	return err
}

// ListSettings returns every row whose key starts with prefix (BP-51: the
// panel reloads persisted pending login flows at startup). LIKE wildcards in
// the prefix are not escaped — callers pass fixed ASCII prefixes.
func (p *Postgres) ListSettings(prefix string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT key, value FROM settings WHERE key LIKE $1`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}
