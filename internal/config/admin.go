package config

import (
	"os"
	"strings"
)

const (
	AdminTokenEnv       = "GENERALCOMPUTE2API_ADMIN_TOKEN"
	LegacyAdminTokenEnv = "POOL_UI_ADMIN_TOKEN"
)

func AdminTokenFromEnv() string {
	if token := strings.TrimSpace(os.Getenv(AdminTokenEnv)); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv(LegacyAdminTokenEnv))
}
