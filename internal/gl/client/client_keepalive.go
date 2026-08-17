package client

import (
	"context"
	"fmt"
	"io"

	"generalcompute2api/internal/auth"
)

// KeepAliveCompletion sends a minimal playground completion so the upstream
// session is marked active (滑动续期保鲜). It reuses the normal completion path,
// including automatic 401 token refresh and account failover, so a stale account
// is revived (even via magic-link auto-login) instead of being skipped.
func (c *Client) KeepAliveCompletion(ctx context.Context, a *auth.RequestAuth) error {
	if c == nil {
		return fmt.Errorf("gl client is nil")
	}
	payload := map[string]any{
		"model": "deepseek-v3.2",
		"messages": []any{
			map[string]any{"role": "user", "content": "ping"},
		},
		"max_tokens": 1,
		"stream":     true,
	}
	resp, err := c.CallCompletion(ctx, a, payload, "", 2)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
