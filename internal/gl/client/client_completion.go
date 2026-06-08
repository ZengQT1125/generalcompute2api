package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"generalcompute2api/internal/auth"
	"generalcompute2api/internal/config"
)

// CallCompletion 完美实现了原 dsclient.CallCompletion 接口，执行向 GL 后端的 OpenAI 对话转发
func (c *Client) CallCompletion(ctx context.Context, a *auth.RequestAuth, payload map[string]any, _ string, maxAttempts int) (*http.Response, error) {
	if maxAttempts <= 0 {
		maxAttempts = c.maxRetries
	}

	// 1. 模型校验与映射
	modelRaw, _ := payload["model"].(string)
	model := strings.TrimSpace(modelRaw)
	if model == "" {
		model = "deepseek-v3.2" // 默认模型
	}

	// 支持且仅支持这三个模型
	switch model {
	case "deepseek-v3.2", "deepseek-v3.1", "minimax-m2.7":
		// 合法，正常传递
		payload["model"] = model
	default:
		return nil, fmt.Errorf("unsupported model %q; only deepseek-v3.2, deepseek-v3.1, and minimax-m2.7 are supported", model)
	}

	// 重组为 GL 官网 API (api.generalcompute.com) 所需的标准 OpenAI 格式
	messages, _ := payload["messages"].([]any)

	glPayload := map[string]any{
		"model":             model,
		"messages":          messages,
		"temperature":       payloadValueOrZero(payload, "temperature"),
		"top_p":             payloadValueOrZero(payload, "top_p"),
		"presence_penalty":  payloadValueOrZero(payload, "presence_penalty"),
		"frequency_penalty": payloadValueOrZero(payload, "frequency_penalty"),
		"stream":            true, // 永远强制以流式向官网请求，避开官网针对非流式的 429 限流保护
	}

	glPayload["stream_options"] = map[string]any{
		"include_usage": true,
	}

	b, err := json.Marshal(glPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal request payload: %w", err)
	}

	glURL := "https://api.generalcompute.com/dashboard/playground/chat/completions"

	var lastErr error
	attemptBudget := maxAttempts
	if a != nil && a.UseConfigToken {
		if n := a.PoolAccountCount(); n > attemptBudget {
			attemptBudget = n
		}
	}
	if attemptBudget <= 0 {
		attemptBudget = 1
	}
	transportErrors := 0
	for attempt := 0; attempt < attemptBudget; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, glURL, bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("create downstream request: %w", err)
		}

		req.Header.Set("Accept", "*/*")
		req.Header.Set("Authorization", "Bearer "+a.DeepSeekToken) // 携带 Clerk JWT 令牌
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://app.generalcompute.com")
		req.Header.Set("Referer", "https://app.generalcompute.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")

		resp, err := c.HttpClient.Do(req)
		if err != nil {
			lastErr = err
			transportErrors++
			if transportErrors >= maxAttempts {
				break
			}
			continue
		}

		// 如果未授权 (401)，自动触发 Token 刷新，并重试轮询
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			if a.UseConfigToken && c.Auth != nil {
				c.Auth.MarkTokenInvalid(a)
				if c.Auth.RefreshToken(ctx, a) {
					continue
				}
				if c.Auth.SwitchAccount(ctx, a) {
					continue
				}
			}
			return nil, fmt.Errorf("GL authentication failed (401/403)")
		}

		if shouldFailoverGLStatus(resp.StatusCode) {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if c.Auth != nil && a != nil && a.UseConfigToken {
				discarded := c.Auth.TryAutoDiscardFromHTTPBody(ctx, a, body)
				if !discarded {
					c.Auth.MarkAccountCooldown(a.AccountID, 2*time.Minute)
				}
				config.Logger.Warn("[gl] upstream 5xx; switching pooled account",
					"status", resp.StatusCode,
					"account", a.AccountID,
					"auto_discarded", discarded,
					"attempt", attempt+1,
					"attempt_budget", attemptBudget,
				)
				if c.Auth.SwitchAccount(ctx, a) {
					continue
				}
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			return resp, nil
		}

		return resp, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("GL API completions request failed after retries: %w", lastErr)
	}
	return nil, errors.New("GL API completions failed")
}

func shouldFailoverGLStatus(status int) bool {
	return status >= http.StatusInternalServerError && status <= 599
}

func payloadValueOrZero(payload map[string]any, key string) any {
	if payload == nil {
		return 0
	}
	if v, ok := payload[key]; ok {
		return v
	}
	return 0
}
