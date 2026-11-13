package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuthEndpointError retains only safe classification from a rejected token
// request. Provider bodies, descriptions, headers and endpoint URLs are omitted.
type OAuthEndpointError struct {
	StatusCode int
	Code       string
}

func (e *OAuthEndpointError) Error() string {
	if code := safeOAuthErrorCode(e.Code); code != "" {
		return fmt.Sprintf("oauth token endpoint returned %d: %s", e.StatusCode, code)
	}
	return fmt.Sprintf("oauth token endpoint returned %d", e.StatusCode)
}

// Accept named protocol errors only. A length or character check would still
// allow an upstream service to return a credential in the error field.
func safeOAuthErrorCode(code string) string {
	switch code {
	case "invalid_request", "invalid_client", "invalid_grant", "unauthorized_client",
		"unsupported_grant_type", "invalid_scope", "invalid_target", "access_denied",
		"unsupported_response_type", "server_error", "temporarily_unavailable",
		"interaction_required", "login_required", "account_selection_required", "consent_required":
		return code
	default:
		return ""
	}
}

func oauthTokenRequest(ctx context.Context, tokenURL string, data url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, errors.New("build token request: invalid endpoint request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, safeConnectorRequestError(ctx, "token request failed", err)
	}
	defer resp.Body.Close()

	body, err := readLimitedBody(resp.Body, MaxOAuthResponseBytes)
	if err != nil {
		return nil, safeConnectorRequestError(ctx, "read token response failed", err)
	}
	if resp.StatusCode != http.StatusOK {
		var providerError struct {
			Code string `json:"error"`
		}
		// Unknown or malformed provider content contributes no error text.
		code := ""
		if json.Unmarshal(body, &providerError) == nil {
			code = safeOAuthErrorCode(providerError.Code)
		}
		return nil, &OAuthEndpointError{StatusCode: resp.StatusCode, Code: code}
	}
	return body, nil
}

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

	body, err := oauthTokenRequest(ctx, tokenURL, data)
	if err != nil {
		return nil, err
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		InstanceURL  string `json:"instance_url"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, errors.New("parse token response: invalid response")
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

// oauth2TokenExchange performs RFC 8693 token exchange: exchanges an SSO token
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

	body, err := oauthTokenRequest(ctx, tokenURL, data)
	if err != nil {
		return nil, err
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IssuedToken  string `json:"issued_token_type"`
		InstanceURL  string `json:"instance_url"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, errors.New("parse token exchange response: invalid response")
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
