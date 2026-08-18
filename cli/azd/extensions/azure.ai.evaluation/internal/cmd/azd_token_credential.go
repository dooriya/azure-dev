// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type azdTokenCredential struct {
	tenantID string
}

func newAzdTokenCredential(tenantID string) azcore.TokenCredential {
	return &azdTokenCredential{tenantID: tenantID}
}

func (c *azdTokenCredential) GetToken(
	ctx context.Context,
	options policy.TokenRequestOptions,
) (azcore.AccessToken, error) {
	if len(options.Scopes) == 0 {
		return azcore.AccessToken{}, fmt.Errorf("azd token credential requires at least one scope")
	}
	executable, err := exec.LookPath("azd")
	if err != nil {
		return azcore.AccessToken{}, fmt.Errorf("finding azd: %w", err)
	}

	args := []string{"auth", "token", "--no-prompt"}
	for _, scope := range options.Scopes {
		args = append(args, "--scope", scope)
	}
	tenantID := c.tenantID
	if options.TenantID != "" {
		tenantID = options.TenantID
	}
	if tenantID != "" {
		args = append(args, "--tenant-id", tenantID)
	}
	if options.Claims != "" {
		args = append(args, "--claims", base64.StdEncoding.EncodeToString([]byte(options.Claims)))
	}

	// Arguments are fixed except for SDK-provided scopes/claims and the selected tenant.
	command := exec.CommandContext(ctx, executable, args...) //nolint:gosec
	command.Env = standaloneAzdEnvironment(os.Environ())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return azcore.AccessToken{}, fmt.Errorf("requesting token from azd: %s", message)
	}
	return parseAzdAccessToken(stdout.String())
}

func standaloneAzdEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		switch strings.ToUpper(key) {
		case "AZD_ACCESS_TOKEN", "AZD_SERVER", "TRACEPARENT", "TRACESTATE":
			continue
		default:
			result = append(result, entry)
		}
	}
	return result
}

func parseAzdAccessToken(raw string) (azcore.AccessToken, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return azcore.AccessToken{}, fmt.Errorf("azd returned an empty access token")
	}

	if strings.HasPrefix(value, "{") {
		var response struct {
			Token     string `json:"token"`
			ExpiresOn string `json:"expiresOn"`
		}
		if err := json.Unmarshal([]byte(value), &response); err != nil {
			return azcore.AccessToken{}, fmt.Errorf("parsing azd token JSON: %w", err)
		}
		expiresOn, err := time.Parse("2006-01-02T15:04:05Z", response.ExpiresOn)
		if err != nil {
			return azcore.AccessToken{}, fmt.Errorf("parsing azd token expiration: %w", err)
		}
		return azcore.AccessToken{Token: response.Token, ExpiresOn: expiresOn}, nil
	}

	expiresOn, err := jwtExpiration(value)
	if err != nil {
		return azcore.AccessToken{}, err
	}
	return azcore.AccessToken{Token: value, ExpiresOn: expiresOn}, nil
}

func jwtExpiration(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("azd token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding azd token payload: %w", err)
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parsing azd token payload: %w", err)
	}
	if claims.ExpiresAt <= 0 {
		return time.Time{}, fmt.Errorf("azd token payload has no expiration")
	}
	return time.Unix(claims.ExpiresAt, 0).UTC(), nil
}

var _ azcore.TokenCredential = (*azdTokenCredential)(nil)
