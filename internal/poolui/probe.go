package poolui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"generalcompute2api/internal/auth"
	"generalcompute2api/internal/config"
	glclient "generalcompute2api/internal/gl/client"
	"generalcompute2api/internal/poolaccounthealth"
	"generalcompute2api/internal/pooldb"
)

const glProbeModel = "minimax-m2.7"

type accountTestResult struct {
	Identifier    string `json:"identifier"`
	OK            bool   `json:"ok"`
	Message       string `json:"message,omitempty"`
	TokenUpdated  bool   `json:"token_updated"`
	TokenPreview  string `json:"token_preview,omitempty"`
	Skipped       bool   `json:"skipped,omitempty"`
	PoolStatus    string `json:"pool_status,omitempty"`
	DiscardReason string `json:"discard_reason,omitempty"`
	AutoDiscarded bool   `json:"auto_discarded,omitempty"`
}

type accountTestResponse struct {
	Total     int                 `json:"total"`
	OK        int                 `json:"ok"`
	Failed    int                 `json:"failed"`
	Skipped   int                 `json:"skipped"`
	Cancelled bool                `json:"cancelled,omitempty"`
	Results   []accountTestResult `json:"results"`
}

func (s *Server) runAccountTests(ctx context.Context, apiKey string, creds []pooldb.PoolAccountCredential) accountTestResponse {
	res, _ := s.runAccountTestsWithProgress(ctx, apiKey, creds, nil)
	return res
}

func (s *Server) probeOneAccount(ctx context.Context, apiKey string, cred pooldb.PoolAccountCredential) accountTestResult {
	row := accountTestResult{Identifier: cred.Identifier}
	if strings.TrimSpace(cred.Password) == "" {
		row.Skipped = true
		row.Message = "缺少凭证"
		return row
	}

	var acc config.Account
	var extra struct {
		Password       string `json:"password"`
		Cookie         string `json:"cookie"`
		SessionID      string `json:"session_id"`
		OrganizationID string `json:"organization_id"`
	}
	if err := json.Unmarshal([]byte(cred.Password), &extra); err != nil {
		row.Skipped = true
		row.Message = "解析凭证 JSON 失败，可能非有效 Clerk 凭证格式"
		return row
	}
	acc.Email = cred.Identifier
	acc.Cookie = extra.Cookie
	acc.SessionID = extra.SessionID
	acc.OrganizationID = extra.OrganizationID

	if strings.TrimSpace(acc.Cookie) == "" || strings.TrimSpace(acc.SessionID) == "" || strings.TrimSpace(acc.OrganizationID) == "" {
		row.Skipped = true
		row.Message = "缺少 Clerk 会话要素 (cookie/session_id/organization_id)"
		return row
	}

	var jwt string
	var err error

	// 纯 GL Clerk 测号逻辑，使用 glclient
	resolver := auth.NewResolver(s.Store, nil)
	resolver.PoolDB = s.DB
	gl := glclient.NewClient(s.Store, resolver)
	useExistingToken := strings.TrimSpace(cred.Token) != ""
	jwt = cred.Token

	var probeErr error
	if useExistingToken {
		probeErr = probeGLChat(ctx, gl, cred.Identifier, jwt)
	}

	if !useExistingToken || probeErr != nil {
		jwt, err = gl.Login(glclient.WithMagicLinkAutoLogin(ctx), acc)
		if err == nil {
			probeErr = probeGLChat(ctx, gl, cred.Identifier, jwt)
			if probeErr != nil {
				err = probeErr
			}
		} else {
			err = fmt.Errorf("GL Clerk login failed: %w", err)
		}
	}

	if err != nil {
		row.OK = false
		row.Message = err.Error()
		reason := discardReasonFromGLProbeError(err.Error())
		if reason != "" {
			row.PoolStatus = reason
			row.DiscardReason = reason
			// 自动在 SQLite 号池中作废该账号
			if sErr := s.DB.SetAccountPoolState(ctx, apiKey, cred.Identifier, true, reason); sErr == nil {
				row.AutoDiscarded = true
			}
		} else {
			row.PoolStatus = poolaccounthealth.PoolStatusTransport
		}
		return row
	}

	row.OK = true
	row.Message = "可用"
	row.PoolStatus = "ok"

	// 将最新捕获的动态 Token 更新写入持久化数据库
	if uErr := s.DB.UpdateAccountToken(ctx, cred.Identifier, jwt); uErr == nil {
		row.TokenUpdated = true
		row.TokenPreview = maskTokenPreview(jwt)
	}
	_ = s.DB.SetAccountPoolState(ctx, apiKey, cred.Identifier, false, pooldb.DiscardReasonNone)

	return row
}

func probeGLChat(ctx context.Context, gl *glclient.Client, identifier, jwt string) error {
	a := &auth.RequestAuth{
		AccountID:      strings.TrimSpace(identifier),
		DeepSeekToken:  strings.TrimSpace(jwt),
		UseConfigToken: false,
		TriedAccounts:  map[string]bool{},
	}
	resp, err := gl.CallCompletion(ctx, a, map[string]any{
		"model": glProbeModel,
		"messages": []any{
			map[string]any{"role": "user", "content": "ping"},
		},
	}, "", 1)
	if err != nil {
		return err
	}
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("GL %s probe returned empty response", glProbeModel)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return readErr
	}
	if reason, msg := poolaccounthealth.ClassifyResponseBytes(raw); reason != "" {
		if strings.TrimSpace(msg) != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("%s", reason)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("GL %s probe failed: status %d: %s", glProbeModel, resp.StatusCode, msg)
	}
	return nil
}

func discardReasonFromGLProbeError(msg string) string {
	if reason := poolaccounthealth.ClassifyLoginError(msg); reason != "" {
		return reason
	}
	lower := strings.ToLower(strings.TrimSpace(msg))
	if strings.Contains(lower, "status 401") ||
		strings.Contains(lower, "status 403") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden") {
		return pooldb.DiscardReasonBanned
	}
	return ""
}

func (s *Server) runAccountTestsWithProgress(
	ctx context.Context,
	apiKey string,
	creds []pooldb.PoolAccountCredential,
	onProgress func(done int, row accountTestResult, ok, failed, skipped int),
) (accountTestResponse, bool) {
	results := make([]accountTestResult, 0, len(creds))
	var okCount, failedCount, skippedCount int

	for _, cred := range creds {
		if err := ctx.Err(); err != nil {
			break
		}
		if s.isTestJobCancelled(apiKey) {
			break
		}
		row := s.probeOneAccount(ctx, apiKey, cred)
		results = append(results, row)
		switch {
		case row.Skipped:
			skippedCount++
		case row.OK:
			okCount++
		default:
			failedCount++
		}
		if onProgress != nil {
			onProgress(len(results), row, okCount, failedCount, skippedCount)
		}
	}

	cancelled := ctx.Err() != nil || s.isTestJobCancelled(apiKey)
	return accountTestResponse{
		Total:     len(creds),
		OK:        okCount,
		Failed:    failedCount,
		Skipped:   skippedCount,
		Cancelled: cancelled,
		Results:   results,
	}, cancelled
}

func maskTokenPreview(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "…" + token[len(token)-4:]
}
