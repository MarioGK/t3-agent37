package peers

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var sandboxHostname = regexp.MustCompile(`^a37-([a-z0-9][a-z0-9-]{0,42})-([a-z0-9]{10})$`)

type SandboxLink struct {
	Peer          string `json:"peer"`
	Name          string `json:"name"`
	SandboxID     string `json:"sandbox_id"`
	EnvironmentID string `json:"environment_id"`
	URL           string `json:"url"`
	Status        string `json:"-"`
}

func decodeSandboxLink(peer Peer, entry link) (SandboxLink, bool) {
	// Leave links with alternate routes alone. The CLI only registers the sandbox's
	// unique Tailscale hostname and never adds alternate addresses.
	if len(entry.URLs) != 1 || !environmentID.MatchString(entry.EnvironmentID) {
		return SandboxLink{}, false
	}
	origin, err := privateOrigin(entry.URLs[0])
	if err != nil {
		return SandboxLink{}, false
	}
	address, _ := url.Parse(origin)
	peerAddress, err := url.Parse(peer.URL)
	if err != nil || address.Port() != "" {
		return SandboxLink{}, false
	}
	host, tailnet, _ := strings.Cut(address.Hostname(), ".")
	_, peerTailnet, _ := strings.Cut(peerAddress.Hostname(), ".")
	matches := sandboxHostname.FindStringSubmatch(host)
	if matches == nil || tailnet != peerTailnet {
		return SandboxLink{}, false
	}
	return SandboxLink{
		Peer: peer.Host, Name: matches[1], SandboxID: matches[2], EnvironmentID: entry.EnvironmentID,
		URL: origin, Status: entry.Status,
	}, true
}

// PruneSandboxLinks visits links on the configured servers. The caller selects
// which ones to remove and can keep its lifecycle lock held through the removal.
func PruneSandboxLinks(ctx context.Context, configured []Peer, dryRun bool,
	visit func(context.Context, SandboxLink, func() error) (bool, error),
) ([]SandboxLink, error) {
	results := make([][]SandboxLink, len(configured))
	failures := make([]error, len(configured))
	forMeshNode(ctx, len(configured), func(i int) {
		results[i], failures[i] = prunePeer(ctx, configured[i], dryRun, visit)
	})
	return slices.Concat(results...), errors.Join(errors.Join(failures...), ctx.Err())
}

func prunePeer(ctx context.Context, peer Peer, dryRun bool,
	visit func(context.Context, SandboxLink, func() error) (bool, error),
) ([]SandboxLink, error) {
	output, err := runOwnerCLI(ctx, peer, "environment", "list", "--json")
	var entries []link
	if err != nil {
		return nil, fmt.Errorf("could not list T3 environments on %s: %w", peer.Host, err)
	}
	if json.Unmarshal(output, &entries) != nil || entries == nil {
		return nil, fmt.Errorf("could not list T3 environments on %s over SSH", peer.Host)
	}
	return pruneEntries(ctx, peer, entries, dryRun, visit, func(sandbox SandboxLink) error {
		if _, err := runOwnerCLI(ctx, peer, "environment", "unlink", sandbox.EnvironmentID); err != nil {
			return fmt.Errorf("could not unlink %s from T3 on %s: %w", sandbox.Name, peer.Host, err)
		}
		return nil
	})
}

func pruneEntries(ctx context.Context, peer Peer, entries []link, dryRun bool,
	visit func(context.Context, SandboxLink, func() error) (bool, error), unlink func(SandboxLink) error,
) ([]SandboxLink, error) {
	var selected []SandboxLink
	var failures []error
	for _, entry := range entries {
		sandbox, ok := decodeSandboxLink(peer, entry)
		if !ok {
			continue
		}
		remove := func() error {
			if dryRun {
				return nil
			}
			return unlink(sandbox)
		}
		pruned, err := visit(ctx, sandbox, remove)
		if pruned {
			selected = append(selected, sandbox)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return selected, errors.Join(failures...)
}
