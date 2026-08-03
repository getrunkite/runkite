package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauth2ClientCredentials performs a standard OAuth2 client_credentials grant.
func oauth2ClientCredentials(ctx context.Context, cfg AuthConfig) (*CachedToken, error) {
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		return nil, fmt.Errorf("token_url required for oauth2_client_credentials")
	}

	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
	}
	if len(cfg.Scopes) > 0 {
		data.Set("scope", strings.Join(cfg.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		InstanceURL  string `json:"instance_url"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned empty access_token")
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600 // default 1h
	}

	return &CachedToken{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
		InstanceURL:  tokenResp.InstanceURL,
	}, nil
}

// oauth2TokenExchange performs RFC 8693 token exchange — exchanges an SSO token
// for a service-specific access token. The exact request shape is provider-specific;
// this implements the standard grant_type=urn:ietf:params:oauth:grant-type:token-exchange
// flow that works with compliant identity providers.
func oauth2TokenExchange(ctx context.Context, cfg AuthConfig, ssoToken string) (*CachedToken, error) {
	tokenURL := cfg.TokenURL
	if tokenURL == "" && cfg.Issuer != "" {
		// Convention: issuer + /oauth2/token for token exchange
		tokenURL = strings.TrimSuffix(cfg.Issuer, "/") + "/oauth2/token"
	}
	if tokenURL == "" {
		return nil, fmt.Errorf("token_url or issuer required for oauth2_token_exchange")
	}

	data := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {ssoToken},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"client_id":            {cfg.ClientID},
		"client_secret":        {cfg.ClientSecret},
	}
	if len(cfg.Scopes) > 0 {
		data.Set("scope", strings.Join(cfg.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IssuedToken  string `json:"issued_token_type"`
		InstanceURL  string `json:"instance_url"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse token exchange response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("token exchange returned empty access_token")
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	return &CachedToken{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
		InstanceURL:  tokenResp.InstanceURL,
	}, nil
}
