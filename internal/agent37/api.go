// Package agent37 provides Agent37 Cloud API operations.
package agent37

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

const (
	apiBase             = "https://api.agent37.com/v1/instances"
	templatesBase       = "https://api.agent37.com/v1/templates"
	execBase            = "https://sandbox.agent37.app/v1/instances"
	ManagedCreditMicros = 1_000_000
)

var (
	idPattern         = regexp.MustCompile("^[a-z0-9]{10}$")
	errorCodePattern  = regexp.MustCompile("^[a-z_]{1,80}$")
	enrollmentPattern = regexp.MustCompile("^tailscale-enrollment-[a-f0-9]{32}$")
	errRequest        = errors.New("Agent37 request failed or returned an invalid response")
	errT3Ready        = errors.New("protected T3 endpoint did not become ready. Check instance status and retry")
)

type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("Agent37 returned HTTP %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("Agent37 returned HTTP %d", e.Status)
}

type Instance struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Template  string            `json:"template"`
	Status    string            `json:"status"`
	Created   int64             `json:"created"`
	Metadata  map[string]string `json:"metadata"`
	extraMeta map[string]json.RawMessage
}

// Keep non-string metadata on managed instances without letting metadata on an
// unrelated instance prevent listing the workspace.
func (i *Instance) UnmarshalJSON(data []byte) error {
	type fields Instance
	row := struct {
		*fields
		Metadata map[string]json.RawMessage `json:"metadata"`
		Created  json.RawMessage            `json:"created"`
	}{fields: (*fields)(i)}
	if err := json.Unmarshal(data, &row); err != nil {
		return err
	}
	i.Created = 0
	_ = json.Unmarshal(row.Created, &i.Created)
	i.Metadata = make(map[string]string, len(row.Metadata))
	i.extraMeta = nil
	for key, raw := range row.Metadata {
		var value *string
		if json.Unmarshal(raw, &value) == nil && value != nil {
			i.Metadata[key] = *value
			continue
		}
		if i.extraMeta == nil {
			i.extraMeta = make(map[string]json.RawMessage)
		}
		i.extraMeta[key] = raw
	}
	return nil
}

type Descriptor struct {
	EnvironmentID string         `json:"environmentId"`
	ServerVersion string         `json:"serverVersion"`
	Capabilities  map[string]any `json:"capabilities"`
}

type Client struct {
	Key  string
	http *http.Client
}

func New(key string) *Client {
	return &Client{Key: key, http: &http.Client{
		Timeout: 240 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return errors.New("invalid Agent37 instance ID")
	}
	return nil
}

func ProtectedOrigin(id string, port int) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	if port != 3773 && port != 4373 {
		return "", errors.New("unexpected T3 service port")
	}
	return fmt.Sprintf("https://%s-%d.agent37.app", id, port), nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, payload any, result any, sandbox bool) error {
	var body io.Reader
	var mediaType string
	switch value := payload.(type) {
	case nil:
	case []byte:
		body, mediaType = bytes.NewReader(value), "application/octet-stream"
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return errRequest
		}
		body, mediaType = bytes.NewReader(data), "application/json"
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errRequest
	}
	if sandbox {
		request.Header.Set("X-Agent37-Key", c.Key)
	} else {
		request.Header.Set("Authorization", "Bearer "+c.Key)
	}
	if mediaType != "" {
		request.Header.Set("Content-Type", mediaType)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return cmp.Or(ctx.Err(), errRequest)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Only the API's machine-readable code is safe to show. Details can
		// include secrets provided to exec or the environment API.
		var issue struct {
			Error json.RawMessage `json:"error"`
		}
		var code string
		if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&issue) == nil {
			if json.Unmarshal(issue.Error, &code) != nil {
				var detail struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(issue.Error, &detail) == nil {
					code = detail.Code
				}
			}
		}
		if !errorCodePattern.MatchString(code) {
			code = ""
		}
		return &Error{Status: response.StatusCode, Code: code}
	}
	if response.StatusCode == http.StatusNoContent || result == nil {
		return nil
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(result) != nil {
		return errRequest
	}
	return nil
}

func (c *Client) List(ctx context.Context) ([]Instance, error) {
	result := struct {
		Data []Instance `json:"data"`
	}{Data: []Instance{}}
	err := c.request(ctx, http.MethodGet, apiBase, nil, &result, false)
	return result.Data, err
}

type Template struct {
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	Revision  int    `json:"revision"`
	Revisions []struct {
		Number int `json:"revision"`
	} `json:"revisions"`
}

func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	var result struct {
		Data []Template `json:"data"`
	}
	err := c.request(ctx, http.MethodGet, templatesBase, nil, &result, false)
	return result.Data, err
}

func (c *Client) GetTemplate(ctx context.Context, name string) (Template, error) {
	var result Template
	err := c.request(ctx, http.MethodGet, templatesBase+"/"+url.PathEscape(name), nil, &result, false)
	return result, err
}

func (c *Client) Create(ctx context.Context, payload any) (Instance, error) {
	var result Instance
	err := c.request(ctx, http.MethodPost, apiBase, payload, &result, false)
	if err == nil {
		err = ValidateID(result.ID)
	}
	return result, err
}

func (c *Client) Get(ctx context.Context, id string) (Instance, error) {
	if err := ValidateID(id); err != nil {
		return Instance{}, err
	}
	var result Instance
	err := c.request(ctx, http.MethodGet, apiBase+"/"+id, nil, &result, false)
	return result, err
}

func (c *Client) Delete(ctx context.Context, id string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	err := c.request(ctx, http.MethodDelete, apiBase+"/"+id, nil, nil, false)
	if apiError, ok := errors.AsType[*Error](err); ok && apiError.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *Client) Lifecycle(ctx context.Context, id, action string) (Instance, error) {
	if err := ValidateID(id); err != nil {
		return Instance{}, err
	}
	switch action {
	case "start", "stop", "restart":
	default:
		return Instance{}, errors.New("unknown Agent37 lifecycle action")
	}
	var result Instance
	err := c.request(ctx, http.MethodPost, apiBase+"/"+id+"/"+action, nil, &result, false)
	return result, err
}

func (c *Client) Exec(ctx context.Context, id, command string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	var result struct {
		ExitCode  *int   `json:"exit_code"`
		Stdout    string `json:"stdout"`
		Truncated bool   `json:"truncated"`
	}
	err := c.request(ctx, http.MethodPost, execBase+"/"+id+"/exec", map[string]string{"command": command}, &result, true)
	if err != nil {
		return "", err
	}
	if result.ExitCode == nil {
		return "", errRequest
	}
	if *result.ExitCode != 0 || result.Truncated {
		return "", fmt.Errorf("sandbox setup command failed (exit %d)", *result.ExitCode)
	}
	return result.Stdout, nil
}

// ExecJSON decodes the JSON output of a successful sandbox command without
// exposing its stdout in error messages.
func (c *Client) ExecJSON[T any](ctx context.Context, id, command string) (T, error) {
	var result T
	output, err := c.Exec(ctx, id, command)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return result, errors.New("sandbox returned invalid setup data")
	}
	return result, nil
}

func (c *Client) UploadEnrollmentKey(ctx context.Context, id, filename, key string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	if !enrollmentPattern.MatchString(filename) {
		return errors.New("invalid enrollment file name")
	}
	query := url.Values{"path": {"~/.local/share/t3-agent37/" + filename}, "overwrite": {"false"}}
	endpoint := "https://" + id + ".agent37.app/v1/files/content?" + query.Encode()
	return c.request(ctx, http.MethodPut, endpoint, []byte(key), nil, true)
}

func (c *Client) RequireManagedBudget(ctx context.Context, id string, initialize bool) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	endpoint := apiBase + "/" + id + "/budget"
	var budget struct {
		Monthly int64 `json:"monthly_remaining_micros"`
		Credit  int64 `json:"credit_remaining_micros"`
	}
	if err := c.request(ctx, http.MethodGet, endpoint, nil, &budget, false); err != nil {
		return err
	}
	if initialize && budget.Monthly <= 0 && budget.Credit <= 0 {
		payload := map[string]any{"amount_micros": ManagedCreditMicros, "idempotency_key": "t3-agent37-ready-v1"}
		if err := c.request(ctx, http.MethodPost, endpoint+"/top-up", payload, &budget, false); err != nil {
			return err
		}
	}
	if budget.Monthly <= 0 && budget.Credit <= 0 {
		return errors.New("Agent37 model budget is exhausted. Add headroom in Agent37 before starting a turn")
	}
	return nil
}

func (c *Client) SetProvider(ctx context.Context, instance Instance, provider string) error {
	if err := ValidateID(instance.ID); err != nil {
		return err
	}
	metadata := make(map[string]any, len(instance.Metadata)+len(instance.extraMeta)+1)
	for key, value := range instance.extraMeta {
		metadata[key] = value
	}
	for key, value := range instance.Metadata {
		metadata[key] = value
	}
	metadata["provider"] = provider
	return c.request(ctx, http.MethodPatch, apiBase+"/"+instance.ID, map[string]any{"metadata": metadata}, nil, false)
}

func (c *Client) Terminal(ctx context.Context, id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	var result struct {
		URL string `json:"url"`
	}
	err := c.request(ctx, http.MethodPost, apiBase+"/"+id+"/signed-url", map[string]int{"port": 7681}, &result, false)
	if err == nil && result.URL == "" {
		return "", errRequest
	}
	return result.URL, err
}

func (c *Client) WaitForT3(ctx context.Context, id, expected string, remotePort int) (Descriptor, int, error) {
	if err := ValidateID(id); err != nil {
		return Descriptor{}, 0, err
	}
	ports := []int{3773, 4373}
	if remotePort != 0 {
		ports = []int{remotePort}
	}
	// The previous CLI waits up to 45 seconds. A caller can shorten the
	// deadline by canceling the enclosing command context.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for {
		for _, port := range ports {
			origin, err := ProtectedOrigin(id, port)
			if err != nil {
				return Descriptor{}, 0, err
			}
			var visible Descriptor
			attempt, stop := context.WithTimeout(ctx, 5*time.Second)
			err = c.request(attempt, http.MethodGet, origin+"/.well-known/t3/environment", nil, &visible, true)
			stop()
			if err == nil {
				if visible.EnvironmentID == "" || (expected != "" && visible.EnvironmentID != expected) {
					return Descriptor{}, 0, errors.New("protected T3 endpoint returned a different environment")
				}
				return visible, port, nil
			}
			if apiError, ok := errors.AsType[*Error](err); ok {
				switch apiError.Status {
				case 404, 409, 425, 429, 502, 503, 504:
				default:
					return Descriptor{}, 0, fmt.Errorf("protected T3 endpoint returned HTTP %d", apiError.Status)
				}
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return Descriptor{}, 0, ctx.Err()
			}
			return Descriptor{}, 0, errT3Ready
		case <-time.After(time.Second):
		}
	}
}
