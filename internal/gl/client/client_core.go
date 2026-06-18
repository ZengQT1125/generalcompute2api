package client

import (
	"bufio"
	"context"
	"encoding/base64"
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

	utls "github.com/refraction-networking/utls"
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

func forceHTTP11ALPN(uConn *utls.UConn) error {
	if err := uConn.BuildHandshakeState(); err != nil {
		return err
	}
	for _, ext := range uConn.Extensions {
		alpnExt, ok := ext.(*utls.ALPNExtension)
		if !ok {
			continue
		}
		alpnExt.AlpnProtocols = []string{"http/1.1"}
		return nil
	}
	return nil
}

func makeUtlsDialer(proxyFunc func(*http.Request) (*url.URL, error), dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var plainConn net.Conn
		var err error
		var proxyURL *url.URL

		if proxyFunc != nil {
			dummyReq, _ := http.NewRequestWithContext(ctx, "CONNECT", "https://"+addr, nil)
			if u, err := proxyFunc(dummyReq); err == nil {
				proxyURL = u
			}
		}

		if proxyURL != nil && (proxyURL.Scheme == "http" || proxyURL.Scheme == "https") {
			config.Logger.Info("[glclient] [uTLS] 建立连接: 正在通过 HTTP 代理发起 CONNECT 隧道", "proxy", proxyURL.Host, "target", addr)
			proxyHost := proxyURL.Host
			if !strings.Contains(proxyHost, ":") {
				proxyHost = proxyHost + ":80"
			}
			plainConn, err = dialContext(ctx, "tcp", proxyHost)
			if err != nil {
				return nil, fmt.Errorf("dial proxy %s failed: %w", proxyURL.Host, err)
			}

			connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
			if proxyURL.User != nil {
				password, _ := proxyURL.User.Password()
				auth := proxyURL.User.Username() + ":" + password
				basicAuth := base64.StdEncoding.EncodeToString([]byte(auth))
				connectReq += "Proxy-Authorization: Basic " + basicAuth + "\r\n"
			}
			connectReq += "\r\n"

			_, err = plainConn.Write([]byte(connectReq))
			if err != nil {
				plainConn.Close()
				return nil, fmt.Errorf("write CONNECT to proxy failed: %w", err)
			}

			respReader := bufio.NewReader(plainConn)
			resp, err := http.ReadResponse(respReader, &http.Request{Method: "CONNECT"})
			if err != nil {
				plainConn.Close()
				return nil, fmt.Errorf("read CONNECT response from proxy failed: %w", err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				plainConn.Close()
				return nil, fmt.Errorf("proxy CONNECT returned status %d: %s", resp.StatusCode, resp.Status)
			}
			config.Logger.Info("[glclient] [uTLS] HTTP 代理 CONNECT 隧道建立成功", "target", addr)
		} else {
			config.Logger.Info("[glclient] [uTLS] 建立连接: 正在发起直连或 SOCKS 代理连接", "target", addr)
			plainConn, err = dialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
		}

		config.Logger.Info("[glclient] [uTLS] TCP 连接已建立，正在使用 Chrome Auto 指纹执行 uTLS 握手...", "target", addr)
		host, _, _ := net.SplitHostPort(addr)
		uCfg := &utls.Config{ServerName: host}
		uConn := utls.UClient(plainConn, uCfg, utls.HelloChrome_Auto)
		if err := forceHTTP11ALPN(uConn); err != nil {
			_ = plainConn.Close()
			return nil, err
		}

		err = uConn.HandshakeContext(ctx)
		if err != nil {
			_ = plainConn.Close()
			return nil, err
		}

		config.Logger.Info("[glclient] [uTLS] uTLS 握手成功！已成功建立安全通道", "target", addr)
		return uConn, nil
	}
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

	dialContext := (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext

	baseTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: dialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	baseTransport.DialTLSContext = makeUtlsDialer(http.ProxyFromEnvironment, dialContext)

	return &Client{
		Store:        store,
		Auth:         resolver,
		HttpClient:   &http.Client{Timeout: 30 * time.Second, Transport: baseTransport},
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
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	pType := strings.ToLower(strings.TrimSpace(foundProxy.Type))
	var proxyFunc func(*http.Request) (*url.URL, error)
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
			proxyFunc = http.ProxyURL(u)
			transport.Proxy = proxyFunc
		} else {
			config.Logger.Error("[glclient] Failed to parse HTTP proxy URL, using direct connection", "error", err, "proxy_id", proxyID)
		}
	}

	transport.DialTLSContext = makeUtlsDialer(proxyFunc, transport.DialContext)

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
