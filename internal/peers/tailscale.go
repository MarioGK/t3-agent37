package peers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"tailscale.com/client/tailscale/v2"
)

const agent37Tag = "tag:agent37"

type Enrollment struct {
	keys *tailscale.KeysResource
}

// VerifyEnrollment exchanges the workstation credentials for a scoped API token.
// It contacts Tailscale without creating a device or an enrollment key.
func VerifyEnrollment(ctx context.Context, clientID, clientSecret string) (*Enrollment, error) {
	client := newHTTPClient(15 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("unexpected Tailscale API redirect")
	}
	return verifyEnrollment(ctx, client, &url.URL{Scheme: "https", Host: "api.tailscale.com"}, clientID, clientSecret)
}

func verifyEnrollment(ctx context.Context, client *http.Client, base *url.URL, clientID, clientSecret string) (*Enrollment, error) {
	config := clientcredentials.Config{
		ClientID:       clientID,
		ClientSecret:   clientSecret,
		TokenURL:       base.JoinPath("api/v2/oauth/token").String(),
		Scopes:         []string{"auth_keys"},
		EndpointParams: url.Values{"tags": {agent37Tag}},
		AuthStyle:      oauth2.AuthStyleInParams,
	}
	token, err := config.Token(context.WithValue(ctx, oauth2.HTTPClient, client))
	if err != nil {
		var reply *oauth2.RetrieveError
		if errors.As(err, &reply) {
			var status int
			if reply.Response != nil {
				status = reply.Response.StatusCode
			}
			switch {
			case reply.ErrorCode == "invalid_client" || status == http.StatusUnauthorized:
				return nil, errors.New("Tailscale rejected the OAuth client ID or secret. Check the credentials and rerun setup --verify")
			case reply.ErrorCode == "invalid_scope" || reply.ErrorCode == "unauthorized_client" ||
				reply.ErrorCode == "invalid_grant" || status == http.StatusForbidden:
				return nil, errors.New("Tailscale denied enrollment. Grant the OAuth client Auth Keys Write for tag:agent37")
			case status != 0:
				return nil, fmt.Errorf("Tailscale OAuth returned HTTP %d. Retry setup --verify", status)
			}
		}
		return nil, errors.New("could not verify the Tailscale OAuth client. Check network access to api.tailscale.com and retry")
	}
	// Use the verified token for the rest of this command. Request contexts on
	// SDK calls also allow revocation after the original join was canceled.
	apiHTTP := *client
	apiHTTP.Transport = &oauth2.Transport{Source: oauth2.StaticTokenSource(token), Base: client.Transport}
	api := &tailscale.Client{BaseURL: base, HTTP: &apiHTTP, UserAgent: "t3-agent37"}
	return &Enrollment{keys: api.Keys()}, nil
}

// UseKey mints one ephemeral key for a synchronous enrollment attempt.
// A failed attempt revokes the key even if the caller canceled the join.
func (enrollment *Enrollment) UseKey(ctx context.Context, use func(string) error) error {
	request := tailscale.CreateKeyRequest{
		ExpirySeconds: 600,
		Description:   "Agent37 disposable enrollment",
	}
	device := &request.Capabilities.Devices.Create
	device.Ephemeral = true
	device.Preauthorized = true
	device.Tags = []string{agent37Tag}
	key, err := enrollment.keys.CreateAuthKey(ctx, request)
	if err != nil {
		return enrollmentAPIError("create an ephemeral enrollment key", err)
	}
	if key.ID == "" {
		return errors.New("Tailscale returned an enrollment key without a revocation ID")
	}
	if !strings.HasPrefix(key.Key, "tskey-auth-") {
		err = errors.New("Tailscale did not issue a device enrollment key")
	} else {
		err = use(key.Key)
	}
	if err == nil {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if revokeErr := enrollment.keys.Delete(cleanup, key.ID); revokeErr != nil && !tailscale.IsNotFound(revokeErr) {
		return errors.Join(err, enrollmentAPIError("revoke the unused enrollment key", revokeErr))
	}
	return err
}

func enrollmentAPIError(action string, err error) error {
	// The SDK's full error can include the vendor response body.
	var response tailscale.APIError
	if errors.As(err, &response) {
		return fmt.Errorf("could not %s (Tailscale HTTP %d)", action, response.Status)
	}
	return fmt.Errorf("could not %s through the Tailscale API", action)
}
