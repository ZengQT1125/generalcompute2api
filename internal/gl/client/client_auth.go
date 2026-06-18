package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"generalcompute2api/internal/config"
	"generalcompute2api/internal/pooldb"
)

func getClerkAPIVersion() string {
	if v := strings.TrimSpace(os.Getenv("GENERALCOMPUTE2API_CLERK_API_VERSION")); v != "" {
		return v
	}
	return "2026-05-12"
}

func getClerkJSVersion() string {
	if v := strings.TrimSpace(os.Getenv("GENERALCOMPUTE2API_CLERK_JS_VERSION")); v != "" {
		return v
	}
	return "6.17.0"
}

// extractClientCookie 提取 Cookie 中 __client 的字段值，并拼装为 "__client=xxx" 格式。
func extractClientCookie(cookieStr string) string {
	cookieStr = strings.TrimSpace(cookieStr)
	if cookieStr == "" {
		return ""
	}

	// 1. 如果包含 __client=
	if strings.Contains(cookieStr, "__client=") {
		parts := strings.Split(cookieStr, ";")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "__client=") {
				return part
			}
		}
	}

	// 2. 如果是 JWT 字符串（以 eyJ 开头）
	if strings.HasPrefix(cookieStr, "eyJ") {
		return "__client=" + cookieStr
	}

	// 3. 用正则兜底匹配 __client
	re := regexp.MustCompile(`__client\s*=\s*([a-zA-Z0-9_\.-]+)`)
	matches := re.FindStringSubmatch(cookieStr)
	if len(matches) > 1 {
		return "__client=" + matches[1]
	}

	return cookieStr
}

// Login 完美实现了 auth.Resolver 所需的 LoginFunc 接口签名，实现 Clerk JWT 动态刷新，并带有 Magic Link 自动登录自愈能力
func (c *Client) Login(ctx context.Context, acc config.Account) (string, error) {
	cookie := extractClientCookie(acc.Cookie)
	sessionID := strings.TrimSpace(acc.SessionID)
	orgID := strings.TrimSpace(acc.OrganizationID)

	if cookie == "" || sessionID == "" || orgID == "" {
		return "", fmt.Errorf("account %q is missing Clerk credentials (cookie/session_id/organization_id)", acc.Identifier())
	}

	clerkURL := fmt.Sprintf("https://clerk.generalcompute.com/v1/client/sessions/%s/tokens", sessionID)
	q := url.Values{}
	q.Set("__clerk_api_version", getClerkAPIVersion())
	q.Set("_clerk_js_version", getClerkJSVersion())
	clerkURL += "?" + q.Encode()

	formData := url.Values{}
	formData.Set("organization_id", orgID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, clerkURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", fmt.Errorf("create clerk token request: %w", err)
	}

	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://app.generalcompute.com")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", "https://app.generalcompute.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")

	httpClient := c.getClientForAccount(acc)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send clerk token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		errStr := string(body)

		// 检查是否是已登出 (signed_out) 或无授权，执行 Magic Link 自动登录自愈
		if strings.Contains(errStr, "signed_out") || strings.Contains(errStr, "Signed out") || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			config.Logger.Warn("[glclient] Clerk session is signed out or invalid. Attempting Auto-Login via Email Magic Link...", "email", acc.Email)

			newCookie, newSessionID, newOrgID, autoErr := c.AutoLoginMagicLink(ctx, acc)
			if autoErr == nil {
				config.Logger.Info("[glclient] Auto-Login via Magic Link succeeded! Retrying Clerk Token fetch...", "email", acc.Email)

				// 回写新获取的会话凭证到 SQLite 数据库中
				if c.Auth != nil && c.Auth.PoolDB != nil {
					if dbConn, ok := c.Auth.PoolDB.(*pooldb.DB); ok {
						_ = dbConn.UpdateAccountClerkCredentials(ctx, acc.Email, newCookie, newSessionID, newOrgID)
					}
				}

				// 用最新凭证重新发起登录
				acc.Cookie = newCookie
				acc.SessionID = newSessionID
				acc.OrganizationID = newOrgID
				return c.Login(ctx, acc)
			} else {
				config.Logger.Error("[glclient] Auto-Login via Magic Link failed", "email", acc.Email, "error", autoErr)
			}
		}

		return "", fmt.Errorf("clerk server returned status %d: %s", resp.StatusCode, errStr)
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

// AutoLoginMagicLink 通过 Workers 临时邮箱接口自动获取 Magic Link 并激活登录状态
func (c *Client) AutoLoginMagicLink(ctx context.Context, acc config.Account) (newCookie, newSessionID, newOrgID string, err error) {
	email := strings.TrimSpace(acc.Email)
	if email == "" {
		return "", "", "", fmt.Errorf("empty email address")
	}

	workerBase := strings.TrimSpace(os.Getenv("GENERALCOMPUTE2API_EMAIL_WORKER_BASE"))
	if workerBase == "" {
		workerBase = "https://temp-mail.1583615885.workers.dev"
	}
	workerAuth := strings.TrimSpace(os.Getenv("GENERALCOMPUTE2API_EMAIL_WORKER_AUTH"))
	if workerAuth == "" {
		workerAuth = "1125"
	}

	httpClient := c.getClientForAccount(acc)

	config.Logger.Info("[glclient] 步骤1/4: 正在向 Clerk 触发发送登录 Magic Link 邮件...", "email", email)

	// 1. 发起登录指令，让 Clerk 向临时邮箱发送 Magic Link 登录邮件
	clerkURL := fmt.Sprintf("https://clerk.generalcompute.com/v1/client/sign_ins?__clerk_api_version=%s&_clerk_js_version=%s", getClerkAPIVersion(), getClerkJSVersion())
	formData := url.Values{}
	formData.Set("locale", "zh-CN")
	formData.Set("identifier", email)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, clerkURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://app.generalcompute.com")
	req.Header.Set("Referer", "https://app.generalcompute.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")
	oldCookie := extractClientCookie(acc.Cookie)
	if oldCookie != "" {
		req.Header.Set("Cookie", oldCookie)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("trigger clerk login mail failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", "", "", fmt.Errorf("trigger clerk login mail returned status %d: %s", resp.StatusCode, string(body))
	}

	// 提取初始 __client Cookie 值（若有）
	var initialClientCookie string
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "__client" {
			initialClientCookie = cookie.Value
		}
	}

	var signInResp struct {
		Response struct {
			ID                    string `json:"id"`
			SupportedFirstFactors []struct {
				Strategy       string `json:"strategy"`
				EmailAddressID string `json:"email_address_id"`
			} `json:"supported_first_factors"`
		} `json:"response"`
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", fmt.Errorf("read sign_in response body failed: %w", err)
	}

	if err := json.Unmarshal(bodyBytes, &signInResp); err != nil {
		return "", "", "", fmt.Errorf("parse sign_in response JSON failed: %w, body: %s", err, string(bodyBytes))
	}

	signInID := signInResp.Response.ID
	var emailAddressID string
	for _, factor := range signInResp.Response.SupportedFirstFactors {
		if factor.Strategy == "email_link" {
			emailAddressID = factor.EmailAddressID
			break
		}
	}

	if signInID == "" || emailAddressID == "" {
		return "", "", "", fmt.Errorf("could not extract signInID or emailAddressID from sign_ins response: %s", string(bodyBytes))
	}

	config.Logger.Info("[glclient] 步骤1.5: 正在请求 prepare_first_factor 触发邮件发送...", "email", email, "sign_in_id", signInID, "email_address_id", emailAddressID)

	// 请求 prepare_first_factor，Clerk 才会真正把邮件发送出去
	prepareURL := fmt.Sprintf("https://clerk.generalcompute.com/v1/client/sign_ins/%s/prepare_first_factor?__clerk_api_version=%s&_clerk_js_version=%s",
		signInID, getClerkAPIVersion(), getClerkJSVersion())

	prepareData := url.Values{}
	prepareData.Set("email_address_id", emailAddressID)
	prepareData.Set("redirect_url", "https://app.generalcompute.com/sign-in/verify")
	prepareData.Set("strategy", "email_link")

	prepareReq, err := http.NewRequestWithContext(ctx, http.MethodPost, prepareURL, strings.NewReader(prepareData.Encode()))
	if err != nil {
		return "", "", "", err
	}
	prepareReq.Header.Set("Accept", "*/*")
	prepareReq.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	prepareReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	prepareReq.Header.Set("Origin", "https://app.generalcompute.com")
	prepareReq.Header.Set("Referer", "https://app.generalcompute.com/")
	prepareReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")

	activeOldCookie := oldCookie
	if initialClientCookie != "" {
		activeOldCookie = "__client=" + initialClientCookie
	}
	if activeOldCookie != "" {
		prepareReq.Header.Set("Cookie", activeOldCookie)
	}

	prepareResp, err := httpClient.Do(prepareReq)
	if err != nil {
		return "", "", "", fmt.Errorf("prepare first factor failed: %w", err)
	}
	defer prepareResp.Body.Close()

	if prepareResp.StatusCode != http.StatusOK && prepareResp.StatusCode != http.StatusCreated {
		pBody, _ := io.ReadAll(io.LimitReader(prepareResp.Body, 1024))
		return "", "", "", fmt.Errorf("prepare first factor returned status %d: %s", prepareResp.StatusCode, string(pBody))
	}

	// 再次提取最新的 __client Cookie 值（若有）
	for _, cookie := range prepareResp.Cookies() {
		if cookie.Name == "__client" {
			initialClientCookie = cookie.Value
		}
	}

	config.Logger.Info("[glclient] 步骤2/4: Clerk 邮件已发送，开始轮询 Workers 临时邮箱提取 Magic Link...", "email", email)

	// 2. 轮询邮箱，提取激活链接 Magic Link
	var magicLink string
	pollStart := time.Now()
	timeout := 120 * time.Second
	interval := 4 * time.Second
	mailURL := fmt.Sprintf("%s/admin/mails?limit=20&offset=0&address=%s", workerBase, email)
	linkRegex := regexp.MustCompile(`https?://[a-zA-Z0-9][-a-zA-Z0-9.]{0,62}\/v1\/client\/sign_ins\/[^\s"<>]*?token=[A-Za-z0-9%_.-]+`)

	pollCount := 0
	for time.Since(pollStart) < timeout {
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		default:
		}

		pollCount++
		config.Logger.Info(fmt.Sprintf("[glclient] 正在轮询临时邮箱邮件列表 (第 %d 次)...", pollCount), "email", email, "elapsed", time.Since(pollStart).Round(time.Second).String())

		mailReq, err := http.NewRequestWithContext(ctx, http.MethodGet, mailURL, nil)
		if err == nil {
			mailReq.Header.Set("x-admin-auth", workerAuth)
			mailResp, err := httpClient.Do(mailReq)
			if err != nil {
				config.Logger.Warn("[glclient] 轮询临时邮箱网络请求出错", "error", err.Error())
			} else {
				if mailResp.StatusCode == http.StatusOK {
					var mailData struct {
						Results []struct {
							Raw string `json:"raw"`
						} `json:"results"`
						Mails []struct {
							Raw string `json:"raw"`
						} `json:"mails"`
					}
					if json.NewDecoder(mailResp.Body).Decode(&mailData) == nil {
						mails := mailData.Results
						if len(mails) == 0 {
							mails = mailData.Mails
						}
						for _, m := range mails {
							rawStr := m.Raw
							if rawStr != "" {
								decodedBody := parseAndDecodeRawEmail(rawStr)
								match := linkRegex.FindString(decodedBody)
								if match != "" {
									magicLink = match
									break
								}
								// 兜底正则
								backupRegex := regexp.MustCompile(`https?://[^\s"'<>]*?token=[A-Za-z0-9%_.-]+`)
								matches := backupRegex.FindAllString(decodedBody, -1)
								for _, link := range matches {
									if strings.Contains(link, "accept") || strings.Contains(link, "sign_in") || strings.Contains(link, "clerk") {
										magicLink = link
										break
									}
								}
							}
						}
					}
				} else {
					body, _ := io.ReadAll(io.LimitReader(mailResp.Body, 1024))
					config.Logger.Warn("[glclient] 轮询临时邮箱返回异常状态码", "status", mailResp.StatusCode, "body", string(body))
				}
				mailResp.Body.Close()
			}
		}

		if magicLink != "" {
			break
		}
		time.Sleep(interval)
	}

	if magicLink == "" {
		return "", "", "", fmt.Errorf("fetching verification email or extracting magic link timed out")
	}

	config.Logger.Info("[glclient] 步骤3/4: 成功提取到 Magic Link, 正在激活并拦截重定向捕获最新的 __client Cookie...", "magic_link", magicLink)

	// 3. 点击激活 Magic Link。这里我们必须拦截重定向过程，从而在中间抓取已登录的最新 Cookie
	var capturedClientCookie string
	proxyTransport := httpClient.Transport
	redirectInterceptor := &http.Client{
		Timeout:   20 * time.Second,
		Transport: proxyTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && req.Response != nil {
				for _, cookie := range req.Response.Cookies() {
					if cookie.Name == "__client" {
						capturedClientCookie = cookie.Value
					}
				}
			}
			if capturedClientCookie != "" {
				req.Header.Set("Cookie", "__client="+capturedClientCookie)
			}
			return nil
		},
	}

	activateReq, err := http.NewRequestWithContext(ctx, http.MethodGet, magicLink, nil)
	if err != nil {
		return "", "", "", err
	}
	activateReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8")
	activateReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")
	if initialClientCookie != "" {
		activateReq.Header.Set("Cookie", "__client="+initialClientCookie)
	}

	activateResp, err := redirectInterceptor.Do(activateReq)
	if err != nil {
		return "", "", "", fmt.Errorf("activate magic link request failed: %w", err)
	}
	defer activateResp.Body.Close()

	var loggedInClientCookie string
	for _, cookie := range activateResp.Cookies() {
		if cookie.Name == "__client" {
			loggedInClientCookie = cookie.Value
		}
	}

	activeCookie := loggedInClientCookie
	if activeCookie == "" {
		activeCookie = capturedClientCookie
	}
	if activeCookie == "" {
		activeCookie = initialClientCookie
	}
	if activeCookie == "" {
		return "", "", "", fmt.Errorf("failed to capture logged-in __client cookie")
	}

	config.Logger.Info("[glclient] 步骤4/4: 成功捕获 __client Cookie，正在请求 clerk/client 端点获取 Sessions 及 Org ID...")

	// 4. 调用 clerk/client 端点获取被激活后的会话详情 JSON
	clientURL := "https://clerk.generalcompute.com/v1/client"
	infoReq, err := http.NewRequestWithContext(ctx, http.MethodGet, clientURL, nil)
	if err != nil {
		return "", "", "", err
	}
	infoReq.Header.Set("Accept", "*/*")
	infoReq.Header.Set("Cookie", "__client="+activeCookie)
	infoReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")

	infoResp, err := httpClient.Do(infoReq)
	if err != nil {
		return "", "", "", fmt.Errorf("fetch clerk client info failed: %w", err)
	}
	defer infoResp.Body.Close()

	if infoResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(infoResp.Body, 1024))
		return "", "", "", fmt.Errorf("fetch clerk client info returned status %d: %s", infoResp.StatusCode, string(body))
	}

	infoBody, err := io.ReadAll(infoResp.Body)
	if err != nil {
		return "", "", "", err
	}

	sessRegex := regexp.MustCompile(`sess_[A-Za-z0-9]+`)
	orgRegex := regexp.MustCompile(`org_[A-Za-z0-9]+`)

	sessIDMatch := sessRegex.FindString(string(infoBody))
	orgIDMatch := orgRegex.FindString(string(infoBody))

	var clerkData struct {
		Response struct {
			Sessions []struct {
				ID                       string `json:"id"`
				LastActiveOrganizationID string `json:"last_active_organization_id"`
			} `json:"sessions"`
		} `json:"response"`
	}
	if json.Unmarshal(infoBody, &clerkData) == nil && len(clerkData.Response.Sessions) > 0 {
		sessionVal := clerkData.Response.Sessions[0].ID
		orgVal := clerkData.Response.Sessions[0].LastActiveOrganizationID
		if strings.HasPrefix(sessionVal, "sess_") {
			sessIDMatch = sessionVal
		}
		if strings.HasPrefix(orgVal, "org_") {
			orgIDMatch = orgVal
		}
	}

	if sessIDMatch == "" || orgIDMatch == "" {
		return "", "", "", fmt.Errorf("clerk sessions not found in response (body: %s)", string(infoBody))
	}

	config.Logger.Info("[glclient] 成功抓取全部新凭证！", "session_id", sessIDMatch, "org_id", orgIDMatch)

	return "__client=" + activeCookie, sessIDMatch, orgIDMatch, nil
}

// parseAndDecodeRawEmail 解析 SMTP 原始邮件格式并对 quoted-printable 编码进行解码还原为纯文本正文
func parseAndDecodeRawEmail(rawMail string) string {
	parts := strings.SplitN(rawMail, "\r\n\r\n", 2)
	body := rawMail
	if len(parts) == 2 {
		body = parts[1]
	} else {
		parts = strings.SplitN(rawMail, "\n\n", 2)
		if len(parts) == 2 {
			body = parts[1]
		}
	}

	dec := quotedprintable.NewReader(strings.NewReader(body))
	decodedBytes, err := io.ReadAll(dec)
	if err == nil {
		body = string(decodedBytes)
	}

	return body
}
