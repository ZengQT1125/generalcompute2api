package poolui

import (
	"context"
	"encoding/json"
	"strings"

	"generalcompute2api/internal/config"
	dsclient "generalcompute2api/internal/deepseek/client"
	glclient "generalcompute2api/internal/gl/client"
	"generalcompute2api/internal/pooldb"
)

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
	isGL := false
	if strings.HasPrefix(strings.TrimSpace(cred.Password), "{") {
		var extra struct {
			Password       string `json:"password"`
			Cookie         string `json:"cookie"`
			SessionID      string `json:"session_id"`
			OrganizationID string `json:"organization_id"`
		}
		if err := json.Unmarshal([]byte(cred.Password), &extra); err == nil {
			acc.Cookie = extra.Cookie
			acc.SessionID = extra.SessionID
			acc.OrganizationID = extra.OrganizationID
			
			if acc.Cookie != "" || acc.SessionID != "" || acc.OrganizationID != "" {
				isGL = true
			}
		}
	}

	var jwt string
	var err error
	
	if isGL {
		// 动态检测：属于 GL Clerk 会话账号，直接使用 glclient 刷新 Token 进行测号
		gl := glclient.NewClient(nil, nil)
		jwt, err = gl.Login(ctx, acc)
	} else {
		// 属于普通 DeepSeek 官方账号，走 dsclient 的账号密码登陆测号
		acc.Email = cred.Identifier
		acc.Password = cred.Password
		ds := dsclient.NewClient(nil, nil)
		jwt, err = ds.Login(ctx, acc)
	}

	if err != nil {
		row.OK = false
		row.Message = err.Error()
		row.PoolStatus = "banned"
		row.DiscardReason = "banned"
		// 自动在 SQLite 号池中作废该账号
		if sErr := s.DB.SetAccountPoolState(ctx, apiKey, cred.Identifier, true, "banned"); sErr == nil {
			row.AutoDiscarded = true
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
