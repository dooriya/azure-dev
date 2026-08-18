// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAzdAccessTokenRawJWT(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	payload := base64.RawURLEncoding.EncodeToString(
		fmt.Appendf(nil, `{"exp":%d}`, expiresAt.Unix()),
	)
	token := "header." + payload + ".signature"

	accessToken, err := parseAzdAccessToken(token)
	require.NoError(t, err)
	assert.Equal(t, token, accessToken.Token)
	assert.Equal(t, expiresAt, accessToken.ExpiresOn)
}

func TestParseAzdAccessTokenJSON(t *testing.T) {
	accessToken, err := parseAzdAccessToken(
		`{"token":"token","expiresOn":"2026-08-17T23:00:00Z"}`,
	)
	require.NoError(t, err)
	assert.Equal(t, "token", accessToken.Token)
	assert.Equal(t, time.Date(2026, 8, 17, 23, 0, 0, 0, time.UTC), accessToken.ExpiresOn)
}

func TestStandaloneAzdEnvironmentRemovesParentExtensionContext(t *testing.T) {
	environment := standaloneAzdEnvironment([]string{
		"AZD_SERVER=localhost:1234",
		"AZD_ACCESS_TOKEN=secret",
		"TRACEPARENT=trace",
		"KEEP=value",
	})
	assert.Equal(t, []string{"KEEP=value"}, environment)
}
