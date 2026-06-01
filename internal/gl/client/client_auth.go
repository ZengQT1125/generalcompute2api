package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"generalcompute2api/internal/config"
)

// Login 完美实现了 auth.Resolver 所需的 LoginFunc 接口签名，实现 Clerk JWT 动态刷新
func (c *Client) Login(ctx context.Context, acc config.Account) (string, error) {
	cookie := strings.TrimSpace(acc.Cookie)
	sessionID := strings.TrimSpace(acc.SessionID)
	orgID := strings.TrimSpace(acc.OrganizationID)

	if cookie == "" || sessionID == "" || orgID == "" {
		return "", fmt.Errorf("account %q is missing Clerk credentials (cookie/session_id/organization_id)", acc.Identifier())
	}

	clerkURL := fmt.Sprintf("https://clerk.generalcompute.com/v1/client/sessions/%s/tokens", sessionID)

	formData := url.Values{}
	formData.Set("organization_id", orgID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, clerkURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", fmt.Errorf("create clerk token request: %w", err)
	}

	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://app.generalcompute.com")
	req.Header.Set("Referer", "https://app.generalcompute.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")

	resp, err := c.HttpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send clerk token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("clerk server returned status %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("decode clerk token response: %w", err)
	}

	jwt := strings.TrimSpace(res.JWT)
	if jwt == "" {
		return "", fmt.Errorf("clerk server returned empty JWT token")
	}

	return jwt, nil
}
