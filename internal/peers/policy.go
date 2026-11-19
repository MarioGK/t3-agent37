package peers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/tailscale/hujson"
	"tailscale.com/client/tailscale/v2"
)

// EnsureEnrollmentTags defines missing tags using the OAuth client's policy
// grant. It preserves existing owners and uses the policy revision on save.
func EnsureEnrollmentTags(ctx context.Context, id, secret string, tags []string, dryRun bool) ([]string, error) {
	api, err := tailscaleOAuthClient(ctx, tailscaleHTTPClient(), &tailscaleAPI, id, secret, "policy_file", nil)
	if err != nil {
		return nil, errors.New("Tailscale tag repair needs the OAuth client's Policy File Write grant")
	}
	return ensureEnrollmentTags(ctx, api.PolicyFile(), tags, dryRun)
}

func ensureEnrollmentTags(ctx context.Context, policy *tailscale.PolicyFileResource, tags []string, dryRun bool) ([]string, error) {
	current, err := policy.Raw(ctx)
	if err != nil {
		return nil, enrollmentAPIError("read the tailnet policy", err)
	}
	updated, added, err := addEnrollmentTags(current.HuJSON, tags)
	if err != nil || len(added) == 0 {
		return nil, err
	}
	if current.ETag == "" {
		return nil, errors.New("Tailscale did not return a policy revision. Rerun setup --repair-tailscale")
	}
	if err := policy.Validate(ctx, updated); err != nil {
		return nil, enrollmentAPIError("validate the updated tailnet policy", err)
	}
	if !dryRun {
		if err := policy.Set(ctx, updated, current.ETag); err != nil {
			if reply, ok := errors.AsType[tailscale.APIError](err); ok && reply.Status == http.StatusPreconditionFailed {
				return nil, errors.New("the tailnet policy changed during repair. Rerun setup --repair-tailscale")
			}
			return nil, enrollmentAPIError("save the updated tailnet policy", err)
		}
	}
	return added, nil
}

func addEnrollmentTags(raw string, tags []string) (string, []string, error) {
	policy, err := hujson.Parse([]byte(raw))
	if err != nil {
		return "", nil, errors.New("could not parse the Tailscale policy")
	}
	object, ok := policy.Value.(*hujson.Object)
	if !ok {
		return "", nil, errors.New("the Tailscale policy must be an object")
	}
	var owners *hujson.Object
	for _, member := range object.Members {
		if member.Name.Value.(hujson.Literal).String() != "tagOwners" {
			continue
		}
		if owners != nil {
			return "", nil, errors.New("the Tailscale policy has multiple tagOwners fields")
		}
		owners, ok = member.Value.Value.(*hujson.Object)
		if !ok {
			return "", nil, errors.New("the Tailscale policy tagOwners must be an object")
		}
	}
	if owners == nil {
		owners = new(hujson.Object)
		object.Members = append(object.Members, hujson.ObjectMember{
			Name:  hujson.Value{Value: hujson.String("tagOwners")},
			Value: hujson.Value{Value: owners},
		})
	}

	existing := make(map[string]bool, len(owners.Members))
	for _, member := range owners.Members {
		existing[strings.ToLower(member.Name.Value.(hujson.Literal).String())] = true
	}
	var added []string
	for _, tag := range tags {
		if existing[tag] {
			continue
		}
		owners.Members = append(owners.Members, hujson.ObjectMember{
			Name:  hujson.Value{Value: hujson.String(tag)},
			Value: hujson.Value{Value: new(hujson.Array)},
		})
		existing[tag] = true
		added = append(added, tag)
	}
	if len(added) == 0 {
		return raw, nil, nil
	}
	policy.Format()
	return policy.String(), added, nil
}
