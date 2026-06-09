package account

import (
	"sort"
	"sync"

	"generalcompute2api/internal/config"
)

type Pool struct {
	lookup                 Lookup
	runtime                *config.Store // optional; limits from env when nil
	mu                     sync.Mutex
	queue                  []string
	inUse                  map[string]int
	waiters                []chan struct{}
	maxInflightPerAccount  int
	recommendedConcurrency int
	maxQueueSize           int
	globalMaxInflight      int
}

// NewPoolWithRuntime uses lookup for account discovery and runtime (may be nil) for limit knobs.
// AccountCount returns how many accounts participate in pooling (for auth loop bounds).
func (p *Pool) AccountCount() int {
	if p == nil || p.lookup == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.lookup.Accounts())
}

func NewPoolWithRuntime(lookup Lookup, runtime *config.Store) *Pool {
	maxPer := 2
	if runtime != nil {
		maxPer = runtime.RuntimeAccountMaxInflight()
	}
	p := &Pool{
		lookup:                lookup,
		runtime:               runtime,
		inUse:                 map[string]int{},
		maxInflightPerAccount: maxPer,
	}
	p.Reset()
	return p
}

func (p *Pool) Reset() {
	if p.lookup == nil {
		return
	}
	accounts := sortedAccountsForQueue(p.lookup.Accounts())
	ids := accountIDs(accounts)
	if p.runtime != nil {
		p.maxInflightPerAccount = p.runtime.RuntimeAccountMaxInflight()
	} else {
		p.maxInflightPerAccount = maxInflightFromEnv()
	}
	recommended := defaultRecommendedConcurrency(len(ids), p.maxInflightPerAccount)
	queueLimit := maxQueueFromEnv(recommended)
	globalLimit := recommended
	if p.runtime != nil {
		queueLimit = p.runtime.RuntimeAccountMaxQueue(recommended)
		globalLimit = p.runtime.RuntimeGlobalMaxInflight(recommended)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drainWaitersLocked()
	p.queue = ids
	p.inUse = map[string]int{}
	p.recommendedConcurrency = recommended
	p.maxQueueSize = queueLimit
	p.globalMaxInflight = globalLimit
	config.Logger.Info(
		"[init_account_queue] initialized",
		"total", len(ids),
		"max_inflight_per_account", p.maxInflightPerAccount,
		"global_max_inflight", p.globalMaxInflight,
		"recommended_concurrency", p.recommendedConcurrency,
		"max_queue_size", p.maxQueueSize,
	)
}

// SyncAccounts 刷新账号快照，同时保留仍在池内账号的租约计数。
// 这样每次从数据库同步状态时，不会丢失高并发下的占用统计。
func (p *Pool) SyncAccounts(accounts []config.Account) {
	if p == nil {
		return
	}
	accounts = sortedAccountsForQueue(accounts)
	newLookup := NewMemoryLookup(accounts)
	newIDs := accountIDs(accounts)
	allowed := make(map[string]struct{}, len(newIDs))
	for _, id := range newIDs {
		allowed[id] = struct{}{}
	}

	if p.runtime != nil {
		p.maxInflightPerAccount = p.runtime.RuntimeAccountMaxInflight()
	} else {
		p.maxInflightPerAccount = maxInflightFromEnv()
	}
	recommended := defaultRecommendedConcurrency(len(newIDs), p.maxInflightPerAccount)
	queueLimit := maxQueueFromEnv(recommended)
	globalLimit := recommended
	if p.runtime != nil {
		queueLimit = p.runtime.RuntimeAccountMaxQueue(recommended)
		globalLimit = p.runtime.RuntimeGlobalMaxInflight(recommended)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	oldQueue := p.queue
	nextQueue := make([]string, 0, len(newIDs))
	seen := make(map[string]struct{}, len(newIDs))
	for _, id := range oldQueue {
		if _, ok := allowed[id]; !ok {
			continue
		}
		nextQueue = append(nextQueue, id)
		seen[id] = struct{}{}
	}
	for _, id := range newIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		nextQueue = append(nextQueue, id)
		seen[id] = struct{}{}
	}
	for id := range p.inUse {
		if _, ok := allowed[id]; !ok {
			delete(p.inUse, id)
		}
	}
	p.lookup = newLookup
	p.queue = nextQueue
	p.recommendedConcurrency = recommended
	p.maxQueueSize = queueLimit
	p.globalMaxInflight = globalLimit
	// 账号列表变化可能释放容量，唤醒等待者重新竞争共享池。
	p.drainWaitersLocked()
}

func (p *Pool) RemoveAccount(accountID string) {
	if p == nil || accountID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, id := range p.queue {
		if id != accountID {
			continue
		}
		p.queue = append(p.queue[:i], p.queue[i+1:]...)
		break
	}
	delete(p.inUse, accountID)
	p.drainWaitersLocked()
}

func (p *Pool) Release(accountID string) {
	if accountID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	count := p.inUse[accountID]
	if count <= 0 {
		return
	}
	if count == 1 {
		delete(p.inUse, accountID)
		p.notifyWaiterLocked()
		return
	}
	p.inUse[accountID] = count - 1
	p.notifyWaiterLocked()
}

func (p *Pool) Status() map[string]any {
	if p.lookup == nil {
		return map[string]any{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	available := make([]string, 0, len(p.queue))
	inUseAccounts := make([]string, 0, len(p.inUse))
	inUseSlots := 0
	for _, id := range p.queue {
		if p.inUse[id] < p.maxInflightPerAccount {
			available = append(available, id)
		}
	}
	for id, count := range p.inUse {
		if count > 0 {
			inUseAccounts = append(inUseAccounts, id)
			inUseSlots += count
		}
	}
	sort.Strings(inUseAccounts)
	return map[string]any{
		"available":                len(available),
		"in_use":                   inUseSlots,
		"total":                    len(p.lookup.Accounts()),
		"available_accounts":       available,
		"in_use_accounts":          inUseAccounts,
		"max_inflight_per_account": p.maxInflightPerAccount,
		"global_max_inflight":      p.globalMaxInflight,
		"recommended_concurrency":  p.recommendedConcurrency,
		"waiting":                  len(p.waiters),
		"max_queue_size":           p.maxQueueSize,
	}
}

func sortedAccountsForQueue(accounts []config.Account) []config.Account {
	out := make([]config.Account, len(accounts))
	copy(out, accounts)
	sort.SliceStable(out, func(i, j int) bool {
		iHas := out[i].Token != ""
		jHas := out[j].Token != ""
		if iHas == jHas {
			return i < j
		}
		return iHas
	})
	return out
}

func accountIDs(accounts []config.Account) []string {
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		id := a.Identifier()
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
