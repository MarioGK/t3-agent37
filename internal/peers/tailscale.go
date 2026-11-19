package peers

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"tailscale.com/client/tailscale/v2"
)

const DefaultEnrollmentTag = "tag:agent37"

var tagPattern = regexp.MustCompile(`^tag:[a-z][a-z0-9-]*$`)

var ErrEnrollmentAuthorization = errors.New("Tailscale OAuth authorization failed")

type Enrollment struct {
	keys *tailscale.KeysResource
	tags []string
}

func ParseEnrollmentTags(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return []string{DefaultEnrollmentTag}, nil
	}
	var tags []string
	for part := range strings.SplitSeq(value, ",") {
		tag := strings.ToLower(strings.TrimSpace(part))
		if !tagPattern.MatchString(tag) {
			return nil, errors.New("use comma-separated Tailscale tags such as tag:agent37,tag:dev")
		}
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	return tags, nil
}

// VerifyEnrollment exchanges the workstation credentials for a scoped API token.
// It contacts Tailscale without creating a device or an enrollment key.
func VerifyEnrollment(ctx context.Context, clientID, clientSecret string, tags []string) (*Enrollment, error) {
	client := newHTTPClient(15 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("unexpected Tailscale API redirect")
	}
	return verifyEnrollment(ctx, client, &url.URL{Scheme: "https", Host: "api.tailscale.com"}, clientID, clientSecret, tags)
}

func verifyEnrollment(ctx context.Context, client *http.Client, base *url.URL, clientID, clientSecret string, tags []string) (*Enrollment, error) {
	config := clientcredentials.Config{
		ClientID:       clientID,
		ClientSecret:   clientSecret,
		TokenURL:       base.JoinPath("api/v2/oauth/token").String(),
		Scopes:         []string{"auth_keys"},
		EndpointParams: url.Values{"tags": {strings.Join(tags, " ")}},
		AuthStyle:      oauth2.AuthStyleInParams,
	}
	token, err := config.Token(context.WithValue(ctx, oauth2.HTTPClient, client))
	if err != nil {
		if reply, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
			var status int
			if reply.Response != nil {
				status = reply.Response.StatusCode
			}
			switch {
			case reply.ErrorCode == "invalid_client" || status == http.StatusUnauthorized:
				return nil, fmt.Errorf("%w. Check the OAuth client ID and secret in t3-agent37 setup", ErrEnrollmentAuthorization)
			case status == http.StatusBadRequest && oauthRejectedTags(reply):
				return nil, fmt.Errorf("%w. Tailscale denied the requested tags %s. In t3-agent37 setup, enter the tags granted to the OAuth client or grant it Auth Keys Write with these tags in Trust credentials", ErrEnrollmentAuthorization, strings.Join(tags, ", "))
			case reply.ErrorCode == "invalid_scope" || reply.ErrorCode == "unauthorized_client" ||
				reply.ErrorCode == "invalid_grant" || status == http.StatusForbidden:
				return nil, fmt.Errorf("%w. Grant the OAuth client Auth Keys Write for %s", ErrEnrollmentAuthorization, strings.Join(tags, ", "))
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
	return &Enrollment{keys: api.Keys(), tags: slices.Clone(tags)}, nil
}

func oauthRejectedTags(reply *oauth2.RetrieveError) bool {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(reply.Body, &body)
	message := strings.ToLower(body.Message + " " + reply.ErrorDescription)
	// Match the provider's diagnostic without printing its response body.
	return strings.Contains(message, "requested tags") && strings.Contains(message, "invalid or not permitted")
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
	device.Tags = enrollment.tags
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
	if response, ok := errors.AsType[tailscale.APIError](err); ok {
		return fmt.Errorf("could not %s (Tailscale HTTP %d)", action, response.Status)
	}
	return fmt.Errorf("could not %s through the Tailscale API", action)
}
