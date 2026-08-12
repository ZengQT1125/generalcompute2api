package poolui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"generalcompute2api/internal/auth"
	"generalcompute2api/internal/config"
	glclient "generalcompute2api/internal/gl/client"
	"generalcompute2api/internal/poolaccounthealth"
	"generalcompute2api/internal/pooldb"
)

const glProbeModel = "deepseek-v3.2"

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
	debug := probeDebugEnabled()
	l := func(msg string, kvs ...any) {
		if !debug {
			return
		}
		kv := append([]any{"account", cred.Identifier, "api_key", apiKey}, kvs...)
		config.Logger.Info("[probe] "+msg, kv...)
	}

	row := accountTestResult{Identifier: cred.Identifier}
	if strings.TrimSpace(cred.Password) == "" {
		row.Skipped = true
		row.Message = "缺少凭证"
		l("缺少凭证")
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
		l("解析凭证JSON失败", "err", err)
		return row
	}
	acc.Email = cred.Identifier
	acc.Cookie = extra.Cookie
	acc.SessionID = extra.SessionID
	acc.OrganizationID = extra.OrganizationID

	if strings.TrimSpace(acc.Cookie) == "" || strings.TrimSpace(acc.SessionID) == "" || strings.TrimSpace(acc.OrganizationID) == "" {
		row.Skipped = true
		row.Message = "缺少 Clerk 会话要素 (cookie/session_id/organization_id)"
		l("缺少Clerk会话要素",
			"has_cookie", strings.TrimSpace(acc.Cookie) != "",
			"has_session", strings.TrimSpace(acc.SessionID) != "",
			"has_org", strings.TrimSpace(acc.OrganizationID) != "")
		return row
	}

	resolver := auth.NewResolver(s.Store, nil)
	resolver.PoolDB = s.DB
	gl := glclient.NewClient(s.Store, resolver)

	useExistingToken := strings.TrimSpace(cred.Token) != ""
	jwt := cred.Token
	var probeErr error
	var err error

	if useExistingToken {
		l("尝试用已存token探测", "model", glProbeModel)
		probeErr = probeGLChat(ctx, gl, cred.Identifier, jwt)
		if probeErr != nil {
			l("已存token探测失败，将触发重新登录授权", "err", probeErr)
		} else {
			l("已存token探测成功")
		}
	} else {
		l("无已存token，直接触发重新登录授权")
	}

	// token 缺失或探测失败 -> 重新登录授权（内含 Magic Link 自动登录自愈）
	if !useExistingToken || probeErr != nil {
		jwt, err = gl.Login(glclient.WithMagicLinkAutoLogin(ctx), acc)
		if err == nil {
			l("重新登录授权成功，token已刷新，再次探测", "model", glProbeModel)
			probeErr = probeGLChat(ctx, gl, cred.Identifier, jwt)
			if probeErr != nil {
				l("重新登录后探测失败", "err", probeErr)
				err = probeErr
			} else {
				l("重新登录后探测成功")
			}
		} else {
			err = fmt.Errorf("GL Clerk login failed: %w", err)
			l("重新登录授权失败", "err", err)
		}
	}

	if err != nil {
		row.OK = false
		row.Message = err.Error()
		reason := discardReasonFromGLProbeError(err.Error())
		if reason != "" {
			row.PoolStatus = reason
			row.DiscardReason = reason
			if sErr := s.DB.SetAccountPoolState(ctx, apiKey, cred.Identifier, true, reason); sErr == nil {
				row.AutoDiscarded = true
			}
			l("判定为禁言/封号，自动作废", "reason", reason)
		} else {
			row.PoolStatus = poolaccounthealth.PoolStatusTransport
			l("判定为非致命（transient），不自动作废", "status", poolaccounthealth.PoolStatusTransport)
		}
		return row
	}

	row.OK = true
	row.Message = "可用"
	row.PoolStatus = "ok"
	l("探测成功，判定可用")

	if uErr := s.DB.UpdateAccountToken(ctx, cred.Identifier, jwt); uErr == nil {
		row.TokenUpdated = true
		row.TokenPreview = maskTokenPreview(jwt)
		l("已写入新token")
	} else {
		l("写入token失败", "err", uErr)
	}
	_ = s.DB.SetAccountPoolState(ctx, apiKey, cred.Identifier, false, pooldb.DiscardReasonNone)
	l("已恢复账号为可用状态")
	return row
}

func probeGLChat(ctx context.Context, gl *glclient.Client, identifier, jwt string) error {
	debug := probeDebugEnabled()
	dbg := func(msg string, kvs ...any) {
		if !debug {
			return
		}
		kv := append([]any{"account", identifier, "model", glProbeModel}, kvs...)
		config.Logger.Info("[probe] "+msg, kv...)
	}
	a := &auth.RequestAuth{
		AccountID:      strings.TrimSpace(identifier),
		DeepSeekToken:  strings.TrimSpace(jwt),
		UseConfigToken: false,
		TriedAccounts:  map[string]bool{},
	}
	dbg("发送探测对话请求")
	resp, err := gl.CallCompletion(ctx, a, map[string]any{
		"model": glProbeModel,
		"messages": []any{
			map[string]any{"role": "user", "content": "ping"},
		},
	}, "", 1)
	if err != nil {
		dbg("探测对话请求出错", "err", err)
		return err
	}
	if resp == nil || resp.Body == nil {
		dbg("探测返回空响应体")
		return fmt.Errorf("GL %s probe returned empty response", glProbeModel)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		dbg("读取探测响应失败", "err", readErr)
		return readErr
	}
	if reason, msg := poolaccounthealth.ClassifyResponseBytes(raw); reason != "" {
		dbg("上游响应命中禁言/封号信号", "reason", reason, "status", resp.StatusCode, "body", truncateLog(string(raw), 512))
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
		dbg("探测返回非2xx状态码", "status", resp.StatusCode, "body", truncateLog(string(raw), 512))
		return fmt.Errorf("GL %s probe failed: status %d: %s", glProbeModel, resp.StatusCode, msg)
	}
	dbg("探测对话成功", "status", resp.StatusCode)
	return nil
}

// discardReasonFromGLProbeError returns a pool discard reason only on explicit
// mute/ban signals from the upstream. Generic 401/403 (expired session, missing
// model permission, forbidden-for-plan) do NOT auto-discard a healthy account --
// otherwise a temporarily-rejected probe would wrongly retire the account.
func discardReasonFromGLProbeError(msg string) string {
	return poolaccounthealth.ClassifyLoginError(msg)
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

// probeDebugEnabled reports whether detailed pool-probe diagnostics are enabled.
// Set GENERALCOMPUTE2API_POOL_PROBE_DEBUG=1|true|on|yes|debug to enable.
func probeDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GENERALCOMPUTE2API_POOL_PROBE_DEBUG"))) {
	case "1", "true", "yes", "on", "enabled", "debug":
		return true
	default:
		return false
	}
}

// truncateLog bounds a probe response body before writing it into the debug log.
func truncateLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
