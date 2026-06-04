package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"generalcompute2api/internal/config"
	"generalcompute2api/internal/server"
)

func main() {
	if err := config.LoadDotEnv(); err != nil {
		config.Logger.Warn("[dotenv] load failed", "error", err)
	}
	config.RefreshLogger()
	app, err := server.NewApp()
	if err != nil {
		config.Logger.Error("server initialization failed", "error", err)
		os.Exit(1)
	}
	defer app.Close()

	// 验证并打印当前的并发限制及数据库配置，便于用户确认配置是否成功加载
	maxInflight := app.Store.RuntimeAccountMaxInflight()
	globalMaxInflight := app.Store.RuntimeGlobalMaxInflight(-1)
	dbPath := os.Getenv("GENERALCOMPUTE2API_DATABASE_PATH")
	if dbPath == "" {
		dbPath = "docker-data/generalcompute2api/generalcompute2api.db"
	}
	maxAccountsPerKey := os.Getenv("GENERALCOMPUTE2API_POOL_MAX_ACCOUNTS_PER_KEY")
	if maxAccountsPerKey == "" {
		maxAccountsPerKey = "0"
	}

	config.Logger.Info("[config] startup environment check",
		"database_path", dbPath,
		"account_max_inflight", maxInflight,
		"global_max_inflight", globalMaxInflight,
		"pool_max_accounts_per_key", maxAccountsPerKey,
	)
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8000"
	}

	srv := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           app.Router,
		ReadHeaderTimeout: 5 * time.Second,
	}
	localURL := fmt.Sprintf("http://127.0.0.1:%s", port)
	lanIP := detectLANIPv4()
	lanURL := ""
	if lanIP != "" {
		lanURL = fmt.Sprintf("http://%s:%s", lanIP, port)
	}

	// Start server in a goroutine so we can listen for shutdown signals.
	go func() {
		if lanURL != "" {
			config.Logger.Info("starting generalcompute2api", "bind", srv.Addr, "port", port, "local_url", localURL, "lan_url", lanURL, "lan_ip", lanIP)
		} else {
			config.Logger.Info("starting generalcompute2api", "bind", srv.Addr, "port", port, "local_url", localURL)
			config.Logger.Warn("lan ip not detected; check active network interfaces")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			config.Logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal (Ctrl+C / SIGTERM).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	sig := <-quit
	config.Logger.Info("shutdown signal received", "signal", sig.String())

	// Graceful shutdown: allow up to 10 seconds for in-flight requests to complete.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		config.Logger.Error("graceful shutdown failed, forcing exit", "error", err)
		os.Exit(1)
	}
	config.Logger.Info("server gracefully stopped")
}

func detectLANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			ip = ip.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}
