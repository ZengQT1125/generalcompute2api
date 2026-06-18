package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"generalcompute2api/internal/config"
)

func TestLoginDoesNotAutoMagicLinkByDefault(t *testing.T) {
	calls := 0
	c := &Client{
		HttpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if strings.Contains(req.URL.Path, "/client/sessions/") {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Status:     "403 Forbidden",
					Body:       io.NopCloser(strings.NewReader(`{"errors":[{"message":"signed_out"}]}`)),
					Header:     make(http.Header),
				}, nil
			}
			t.Fatalf("unexpected request to %s", req.URL.String())
			return nil, nil
		})},
	}

	_, err := c.Login(context.Background(), config.Account{
		Email:          "acc@example.com",
		Cookie:         "__client=old",
		SessionID:      "sess_old",
		OrganizationID: "org_old",
	})
	if err == nil {
		t.Fatal("expected login error")
	}
	if calls != 1 {
		t.Fatalf("expected only token refresh request, got %d calls", calls)
	}
	if strings.Contains(err.Error(), "Magic Link") {
		t.Fatalf("normal login should not attempt magic link, got %v", err)
	}
}

func TestWithMagicLinkAutoLoginMarksContext(t *testing.T) {
	if magicLinkAutoLoginAllowed(context.Background()) {
		t.Fatal("background context should not allow magic link auto-login")
	}
	if !magicLinkAutoLoginAllowed(WithMagicLinkAutoLogin(context.Background())) {
		t.Fatal("marked context should allow magic link auto-login")
	}
}
