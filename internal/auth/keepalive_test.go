package auth

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"generalcompute2api/internal/config"
	"generalcompute2api/internal/pooldb"
)

// TestKeepAliveOnceTouchesAllAccounts verifies the keep-alive round reaches every
// pooled account (session 保鲜的核心保证).
func TestKeepAliveOnceTouchesAllAccounts(t *testing.T) {
	store := config.LoadStore()
	mem := pooldb.NewMem()
	var accounts []config.Account
	for i := 0; i < 20; i++ {
		accounts = append(accounts, config.Account{Email: fmt.Sprintf("acc-%02d@test.com", i), Token: "cached-token"})
	}
	mem.RegisterKey("change-me-pool-ui", accounts, true)
	r := NewResolver(store, func(_ context.Context, _ config.Account) (string, error) {
		return "fresh-token", nil
	})
	r.PoolDB = mem

	var touched int32
	n := r.KeepAliveOnce(context.Background(), func(_ context.Context, _ *RequestAuth) error {
		atomic.AddInt32(&touched, 1)
		return nil
	})
	if n != 20 {
		t.Fatalf("expected 20 accounts touched, got %d", n)
	}
	if got := atomic.LoadInt32(&touched); got != 20 {
		t.Fatalf("keep-alive func called %d times, expected 20", got)
	}
}

// TestKeepAliveOnceSkipsBusyAccounts verifies accounts currently in use by real
// traffic are skipped instead of blocking the keep-alive round.
func TestKeepAliveOnceSkipsBusyAccounts(t *testing.T) {
	store := config.LoadStore()
	mem := pooldb.NewMem()
	accounts := []config.Account{
		{Email: "busy@test.com", Token: "t"},
		{Email: "free@test.com", Token: "t"},
	}
	mem.RegisterKey("change-me-pool-ui", accounts, true)
	r := NewResolver(store, func(_ context.Context, _ config.Account) (string, error) {
		return "fresh-token", nil
	})
	r.PoolDB = mem

	// 模拟真实请求占用 busy 账号
	pool := r.sharedPool(accounts)
	if _, ok := pool.Acquire("busy@test.com", nil); !ok {
		t.Fatal("failed to occupy busy account")
	}
	defer pool.Release("busy@test.com")

	var touched int32
	n := r.KeepAliveOnce(context.Background(), func(_ context.Context, _ *RequestAuth) error {
		atomic.AddInt32(&touched, 1)
		return nil
	})
	if n != 1 {
		t.Fatalf("expected 1 account touched (busy skipped), got %d", n)
	}
	if got := atomic.LoadInt32(&touched); got != 1 {
		t.Fatalf("keep-alive func called %d times, expected 1", got)
	}
}
