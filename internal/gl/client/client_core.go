package client

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"generalcompute2api/internal/auth"
	"generalcompute2api/internal/config"
	dsclient "generalcompute2api/internal/deepseek/client"
)

type Client struct {
	Store      *config.Store
	Auth       *auth.Resolver
	HttpClient *http.Client
	maxRetries int
}

func NewClient(store *config.Store, resolver *auth.Resolver) *Client {
	return &Client{
		Store:      store,
		Auth:       resolver,
		HttpClient: &http.Client{Timeout: 30 * time.Second},
		maxRetries: 3,
	}
}

// PreloadPow 兼容接口。
func (c *Client) PreloadPow(_ context.Context) error {
	return nil
}

// CreateSession 兼容定义，返回虚拟会话 ID 绕过前置握手
func (c *Client) CreateSession(ctx context.Context, a *auth.RequestAuth, maxAttempts int) (string, error) {
	return "dummy-session-id", nil
}

// GetPow 兼容定义，返回空 Pow 绕过验证挑战
func (c *Client) GetPow(ctx context.Context, a *auth.RequestAuth, maxAttempts int) (string, error) {
	return "dummy-pow-answer", nil
}

// UploadFile 兼容定义，直接返回不支持错误
func (c *Client) UploadFile(ctx context.Context, a *auth.RequestAuth, req dsclient.UploadFileRequest, maxAttempts int) (*dsclient.UploadFileResult, error) {
	return nil, fmt.Errorf("file upload is not supported in GL mode")
}

// DeleteSessionForToken 兼容定义
func (c *Client) DeleteSessionForToken(ctx context.Context, token string, sessionID string) (*dsclient.DeleteSessionResult, error) {
	return nil, nil
}

// DeleteAllSessionsForToken 兼容定义
func (c *Client) DeleteAllSessionsForToken(ctx context.Context, token string) error {
	return nil
}
