package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"generalcompute2api/internal/auth"
	"generalcompute2api/internal/config"
	"generalcompute2api/internal/pooldb"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newGLTestResolver(accounts []config.Account) *auth.Resolver {
	mem := pooldb.NewMem()
	mem.RegisterKey("managed-key", accounts, true)
	r := auth.NewResolver(config.LoadStore(), func(_ context.Context, acc config.Account) (string, error) {
		return "refreshed-" + acc.Identifier(), nil
	})
	r.PoolDB = mem
	return r
}

func newManagedAuth(t *testing.T, r *auth.Resolver) *auth.RequestAuth {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer managed-key")
	a, err := r.Determine(req)
	if err != nil {
		t.Fatalf("determine failed: %v", err)
	}
	return a
}

func validGLPayload() map[string]any {
	return map[string]any{
		"model":    "deepseek-v3.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
}

func TestCallCompletionFailoversOnUpstream5xx(t *testing.T) {
	resolver := newGLTestResolver([]config.Account{
		{Email: "bad@example.com", Token: "bad-token"},
		{Email: "good@example.com", Token: "good-token"},
	})
	a := newManagedAuth(t, resolver)
	defer resolver.Release(a)

	var seenAuth []string
	c := &Client{
		Auth: resolver,
		HttpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			authHeader := req.Header.Get("Authorization")
			seenAuth = append(seenAuth, authHeader)
			if strings.Contains(authHeader, "bad-token") {
				return textResponse(http.StatusInternalServerError, `{"error":"account backend broken"}`), nil
			}
			return textResponse(http.StatusOK, "data: [DONE]\n\n"), nil
		})},
		maxRetries: 1,
	}

	resp, err := c.CallCompletion(context.Background(), a, validGLPayload(), "", 1)
	if err != nil {
		t.Fatalf("call completion failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected failover response 200, got %d", resp.StatusCode)
	}
	if a.AccountID != "good@example.com" {
		t.Fatalf("expected switched account, got %q", a.AccountID)
	}
	if len(seenAuth) != 2 {
		t.Fatalf("expected two upstream attempts, got %d", len(seenAuth))
	}
	if !strings.Contains(seenAuth[0], "bad-token") || !strings.Contains(seenAuth[1], "good-token") {
		t.Fatalf("unexpected authorization sequence: %#v", seenAuth)
	}
}

func TestCallCompletionMapsMinimaxModelForUpstream(t *testing.T) {
	resolver := newGLTestResolver([]config.Account{
		{Email: "good@example.com", Token: "good-token"},
	})
	a := newManagedAuth(t, resolver)
	defer resolver.Release(a)

	var upstreamModel string
	c := &Client{
		Auth: resolver,
		HttpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var payload map[string]any
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatalf("decode payload failed: %v", err)
			}
			upstreamModel, _ = payload["model"].(string)
			return textResponse(http.StatusOK, "data: [DONE]\n\n"), nil
		})},
		maxRetries: 1,
	}

	payload := validGLPayload()
	payload["model"] = "minimax-m2.7"
	resp, err := c.CallCompletion(context.Background(), a, payload, "", 1)
	if err != nil {
		t.Fatalf("call completion failed: %v", err)
	}
	defer resp.Body.Close()
	if upstreamModel != "minimax2.7" {
		t.Fatalf("expected upstream minimax2.7, got %q", upstreamModel)
	}
	if payload["model"] != "minimax-m2.7" {
		t.Fatalf("expected canonical payload model, got %q", payload["model"])
	}
}

func TestCallCompletionReturnsFinal5xxWhenNoFallbackAccount(t *testing.T) {
	resolver := newGLTestResolver([]config.Account{
		{Email: "bad@example.com", Token: "bad-token"},
	})
	a := newManagedAuth(t, resolver)
	defer resolver.Release(a)

	c := &Client{
		Auth: resolver,
		HttpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return textResponse(http.StatusInternalServerError, "still broken"), nil
		})},
		maxRetries: 1,
	}

	resp, err := c.CallCompletion(context.Background(), a, validGLPayload(), "", 1)
	if err != nil {
		t.Fatalf("expected final 5xx response, got error: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body failed: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError || string(body) != "still broken" {
		t.Fatalf("unexpected final response: status=%d body=%q", resp.StatusCode, string(body))
	}
}

func TestCallCompletionKeepsTransportErrorRetryBudget(t *testing.T) {
	resolver := newGLTestResolver([]config.Account{
		{Email: "acc1@example.com", Token: "token-1"},
		{Email: "acc2@example.com", Token: "token-2"},
		{Email: "acc3@example.com", Token: "token-3"},
	})
	a := newManagedAuth(t, resolver)
	defer resolver.Release(a)

	attempts := 0
	c := &Client{
		Auth: resolver,
		HttpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("temporary network failure")
		})},
		maxRetries: 2,
	}

	_, err := c.CallCompletion(context.Background(), a, validGLPayload(), "", 2)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if attempts != 2 {
		t.Fatalf("expected transport retry budget 2, got %d", attempts)
	}
	if a.AccountID != "acc1@example.com" {
		t.Fatalf("transport errors should not switch account, got %q", a.AccountID)
	}
}

func textResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Header:        make(http.Header),
	}
}
