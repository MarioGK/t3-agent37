package peers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const tailscaleAPI = "https://api.tailscale.com/api/v2"

// EnrollmentKey mints a one-use, preauthorized ephemeral key restricted to the Agent37 tag.
func EnrollmentKey(ctx context.Context, clientID, clientSecret string) (string, error) {
	client := newHTTPClient(15 * time.Second)
	defer client.CloseIdleConnections()
	return enrollmentKey(ctx, client, tailscaleAPI, clientID, clientSecret)
}

func enrollmentKey(ctx context.Context, client *http.Client, base, clientID, clientSecret string) (string, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"grant_type":    {"client_credentials"},
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := tailscalePost(ctx, client, base, "/oauth/token", "application/x-www-form-urlencoded", "", []byte(form.Encode()), &token); err != nil {
		return "", err
	}
	if token.AccessToken == "" {
		return "", errors.New("Tailscale did not issue an OAuth access token")
	}
	const policy = `{
  "capabilities": {
    "devices": {
      "create": {"reusable": false, "ephemeral": true, "preauthorized": true, "tags": ["tag:agent37"]}
    }
  },
  "expirySeconds": 600,
  "description": "Agent37 disposable enrollment"
}`
	var key struct {
		Key string `json:"key"`
	}
	if err := tailscalePost(ctx, client, base, "/tailnet/-/keys", "application/json", token.AccessToken, []byte(policy), &key); err != nil {
		return "", err
	}
	if !strings.HasPrefix(key.Key, "tskey-auth-") {
		return "", errors.New("Tailscale did not issue a device enrollment key")
	}
	return key.Key, nil
}

func tailscalePost(ctx context.Context, client *http.Client, base, path, contentType, token string, body []byte, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", contentType)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		var response *http.Response
		response, err = client.Do(req)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return fmt.Errorf("Tailscale %s returned HTTP %d", path, response.StatusCode)
			}
			err = readJSON(response.Body, result)
		}
	}
	if err != nil {
		return fmt.Errorf("Tailscale %s could not be reached or returned invalid JSON", path)
	}
	return nil
}
