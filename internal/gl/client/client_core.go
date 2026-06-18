package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"generalcompute2api/internal/auth"
	"generalcompute2api/internal/config"
	dsclient "generalcompute2api/internal/deepseek/client"

	"golang.org/x/net/proxy"
)

type Client struct {
	Store      *config.Store
	Auth       *auth.Resolver
	HttpClient *http.Client
	maxRetries int

	proxyClientsMu sync.RWMutex
	proxyClients   map[string]*http.Client
}

func getWindowsSystemProxy() string {
	cmd := exec.Command("reg", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "/v", "ProxyEnable")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	if !strings.Contains(string(out), "0x1") {
		return ""
	}

	cmd = exec.Command("reg", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "/v", "ProxyServer")
	out, err = cmd.Output()
	if err != nil {
		return ""
	}

	re := regexp.MustCompile(`ProxyServer\s+REG_SZ\s+(.*)`)
	matches := re.FindStringSubmatch(string(out))
	if len(matches) > 1 {
		proxyStr := strings.TrimSpace(matches[1])
		if strings.Contains(proxyStr, ";") {
			parts := strings.Split(proxyStr, ";")
			for _, part := range parts {
				if strings.HasPrefix(part, "http=") {
					return strings.TrimPrefix(part, "http=")
				}
			}
			if len(parts) > 0 {
				return parts[0]
			}
		}
		return proxyStr
	}
	return ""
}

func NewClient(store *config.Store, resolver *auth.Resolver) *Client {
	// 自动加载 Windows 系统代理以支持未显式配置账号代理时的自适应翻墙
	if os.Getenv("HTTP_PROXY") == "" && os.Getenv("HTTPS_PROXY") == "" {
		winProxy := getWindowsSystemProxy()
		if winProxy != "" {
			if !strings.Contains(winProxy, "://") {
				winProxy = "http://" + winProxy
			}
			_ = os.Setenv("HTTP_PROXY", winProxy)
			_ = os.Setenv("HTTPS_PROXY", winProxy)
			config.Logger.Info("[glclient] Automatically detected and applied Windows system proxy", "proxy", winProxy)
		}
	}

	return &Client{
		Store:        store,
		Auth:         resolver,
		HttpClient:   &http.Client{Timeout: 30 * time.Second},
		maxRetries:   3,
		proxyClients: make(map[string]*http.Client),
	}
}

func (c *Client) getClientForAccount(acc config.Account) *http.Client {
	c.proxyClientsMu.RLock()
	if c.proxyClients == nil {
		c.proxyClientsMu.RUnlock()
		c.proxyClientsMu.Lock()
		if c.proxyClients == nil {
			c.proxyClients = make(map[string]*http.Client)
		}
		c.proxyClientsMu.Unlock()
		c.proxyClientsMu.RLock()
	}

	proxyID := strings.TrimSpace(acc.ProxyID)
	if proxyID == "" {
		c.proxyClientsMu.RUnlock()
		return c.HttpClient
	}

	snap := c.Store.Snapshot()
	var foundProxy *config.Proxy
	for _, p := range snap.Proxies {
		if p.ID == proxyID {
			pNormalized := config.NormalizeProxy(p)
			foundProxy = &pNormalized
			break
		}
	}

	if foundProxy == nil {
		c.proxyClientsMu.RUnlock()
		config.Logger.Warn("[glclient] Proxy ID configured for account but not found in runtime settings, falling back to direct connection", "proxy_id", proxyID, "email", acc.Email)
		return c.HttpClient
	}

	key := fmt.Sprintf("%s|%s|%s|%d|%s|%s", foundProxy.ID, foundProxy.Type, foundProxy.Host, foundProxy.Port, foundProxy.Username, foundProxy.Password)
	if cli, ok := c.proxyClients[key]; ok {
		c.proxyClientsMu.RUnlock()
		return cli
	}
	c.proxyClientsMu.RUnlock()

	c.proxyClientsMu.Lock()
	defer c.proxyClientsMu.Unlock()
	if cli, ok := c.proxyClients[key]; ok {
		return cli
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	pType := strings.ToLower(strings.TrimSpace(foundProxy.Type))
	if pType == "socks5" {
		var authCfg *proxy.Auth
		if foundProxy.Username != "" || foundProxy.Password != "" {
			authCfg = &proxy.Auth{User: foundProxy.Username, Password: foundProxy.Password}
		}
		addr := net.JoinHostPort(foundProxy.Host, strconv.Itoa(foundProxy.Port))
		dialer, err := proxy.SOCKS5("tcp", addr, authCfg, &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second})
		if err == nil {
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, splitErr := net.SplitHostPort(address)
				if splitErr == nil && net.ParseIP(host) == nil {
					addrs, lookupErr := net.DefaultResolver.LookupHost(ctx, host)
					if lookupErr == nil && len(addrs) > 0 {
						address = net.JoinHostPort(addrs[0], port)
					}
				}
				if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
					return ctxDialer.DialContext(ctx, network, address)
				}
				return dialer.Dial(network, address)
			}
		} else {
			config.Logger.Error("[glclient] Failed to build socks5 dialer, using direct connection", "error", err, "proxy_id", proxyID)
		}
	} else if pType == "http" || pType == "https" {
		proxyURLStr := fmt.Sprintf("%s://%s:%d", pType, foundProxy.Host, foundProxy.Port)
		if foundProxy.Username != "" || foundProxy.Password != "" {
			proxyURLStr = fmt.Sprintf("%s://%s:%s@%s:%d", pType, url.QueryEscape(foundProxy.Username), url.QueryEscape(foundProxy.Password), foundProxy.Host, foundProxy.Port)
		}
		if u, err := url.Parse(proxyURLStr); err == nil {
			transport.Proxy = http.ProxyURL(u)
		} else {
			config.Logger.Error("[glclient] Failed to parse HTTP proxy URL, using direct connection", "error", err, "proxy_id", proxyID)
		}
	}

	cli := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
	c.proxyClients[key] = cli
	config.Logger.Info("[glclient] Successfully created proxy http client", "proxy_id", proxyID, "type", pType, "host", foundProxy.Host)
	return cli
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
