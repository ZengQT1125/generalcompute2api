package auth

import (
	"context"

	"generalcompute2api/internal/config"
)

// KeepAliveFunc performs a lightweight upstream activity for an acquired account
// (e.g. a minimal playground completion) so the upstream session stays fresh.
type KeepAliveFunc func(ctx context.Context, a *RequestAuth) error

// adminKeyValue returns the single admin token used as the gateway key.
func adminKeyValue() string {
	if t := config.AdminTokenFromEnv(); t != "" {
		return t
	}
	return "change-me-pool-ui"
}

// KeepAliveOnce touches every pooled account exactly once:
//  1. load all non-discarded accounts,
//  2. targeted acquire (non-blocking, respects per-account inflight limits),
//  3. ensure a valid upstream token (login/refresh when needed, which revives
//     stale sessions through the magic-link auto-login path),
//  4. run keepAlive (real upstream activity) and release.
//
// Accounts in cooldown or currently busy are skipped. Returns the number of
// accounts successfully touched. The purpose is session 保鲜: keeping every
// account active so upstream sessions never expire due to low traffic.
func (r *Resolver) KeepAliveOnce(ctx context.Context, keepAlive KeepAliveFunc) int {
	if r == nil || r.PoolDB == nil {
		return 0
	}
	accounts, err := r.PoolDB.LoadAccountsForAPIKey(ctx, adminKeyValue())
	if err != nil {
		config.Logger.Error("[keepalive] load accounts failed", "error", err)
		return 0
	}
	if len(accounts) == 0 {
		config.Logger.Info("[keepalive] no accounts to keep alive")
		return 0
	}
	accounts = r.filterCooldownedAccounts(accounts)
	pool := r.sharedPool(accounts)

	touched := 0
	for _, acc := range accounts {
		if ctx.Err() != nil {
			break
		}
		ident := acc.Identifier()
		if ident == "" {
			continue
		}
		// 定向获取：不进入等待队列，当前正在被真实请求占用的账号直接跳过，
		// 避免保鲜任务与正常流量互相阻塞。
		got, ok := pool.Acquire(ident, nil)
		if !ok {
			config.Logger.Debug("[keepalive] skip busy/limited account", "account", ident)
			continue
		}
		a := &RequestAuth{
			UseConfigToken: true,
			CallerID:       "keepalive",
			AccountID:      ident,
			Account:        got,
			TriedAccounts:  map[string]bool{},
			resolver:       r,
			activePool:     pool,
			PoolManaged:    true,
		}
		if err := r.ensureManagedToken(ctx, a); err != nil {
			r.Release(a)
			config.Logger.Warn("[keepalive] token ensure failed", "account", ident, "error", err)
			continue
		}
		if keepAlive != nil {
			if err := keepAlive(ctx, a); err != nil {
				r.Release(a)
				config.Logger.Warn("[keepalive] activity failed", "account", ident, "error", err)
				continue
			}
		}
		r.Release(a)
		touched++
		config.Logger.Debug("[keepalive] account touched", "account", ident)
	}
	return touched
}
