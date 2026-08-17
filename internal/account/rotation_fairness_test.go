package account

import (
	"fmt"
	"testing"

	"generalcompute2api/internal/config"
)

// 回归测试：验证号池轮转公平性——低流量下也要保证所有账号都会被用到，
// 避免尾部账号 session 因长期空闲而过期
func TestRotationFairnessTemp(t *testing.T) {
	var accounts []config.Account
	// 前 5 个带 token（模拟已用过缓存），后 195 个不带
	for i := 0; i < 200; i++ {
		acc := config.Account{Email: fmt.Sprintf("acc-%03d@test.com", i)}
		if i < 5 {
			acc.Token = fmt.Sprintf("cached-token-%d", i)
		}
		accounts = append(accounts, acc)
	}
	p := NewPoolWithRuntime(NewMemoryLookup(accounts), nil)

	used := map[string]int{}
	var order []string
	// 模拟 400 次串行请求（Acquire -> 立即 Release）
	for i := 0; i < 400; i++ {
		acc, ok := p.Acquire("", nil)
		if !ok {
			t.Fatalf("第 %d 次 acquire 失败", i)
		}
		used[acc.Identifier()]++
		order = append(order, acc.Identifier())
		p.Release(acc.Identifier())
	}

	distinct := len(used)
	fmt.Printf("400 次请求共用到 %d 个不同账号\n", distinct)
	if distinct < 200 {
		t.Fatalf("轮转不公平: 只用到 %d/200 个账号", distinct)
	}
	// 打印前 20 次请求用了谁
	fmt.Println("前 20 次请求顺序:", order[:20])
	// 统计每个账号被用次数分布
	minU, maxU := 1<<30, 0
	for _, c := range used {
		if c < minU { minU = c }
		if c > maxU { maxU = c }
	}
	fmt.Printf("每账号被用次数: min=%d max=%d\n", minU, maxU)
}
