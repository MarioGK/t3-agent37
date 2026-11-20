package peers

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// MeshNode describes a managed sandbox with its verified private T3 address
// and a way to issue a short-lived owner session for this environment.
type MeshNode struct {
	Name          string
	URL           string
	EnvironmentID string
	IssueToken    func(context.Context) (string, error)
	// ListLinksJSON is optional and returns the output of the local T3
	// environment list command. Dry runs only use this read-only route.
	ListLinksJSON func(context.Context) ([]byte, error)
}

// ReconcileMesh verifies every available directed link between configured T3
// peers and managed sandboxes. The returned count includes existing links.
func ReconcileMesh(ctx context.Context, configured []Peer, sandboxes []MeshNode) (int, error) {
	nodes := make([]MeshNode, 0, len(configured)+len(sandboxes))
	for _, peer := range configured {
		nodes = append(nodes, MeshNode{
			Name: peer.Host, URL: peer.URL, EnvironmentID: peer.EnvironmentID,
			IssueToken: func(ctx context.Context) (string, error) { return issueOverSSH(ctx, peer) },
		})
	}
	nodes = append(nodes, sandboxes...)
	client := newHTTPClient(35 * time.Second)
	defer client.CloseIdleConnections()
	return reconcileMesh(ctx, client, nodes)
}

type meshEndpoint struct {
	api   peerAPI
	token string
}

func reconcileMesh(ctx context.Context, client *http.Client, candidates []MeshNode) (int, error) {
	nodes, validation := normalizeMeshNodes(candidates)
	if len(nodes) < 2 {
		return 0, validation
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	endpoints := make([]meshEndpoint, len(nodes))
	availability := make([]error, len(nodes))
	forMeshNode(ctx, len(nodes), func(i int) {
		node := nodes[i]
		api := newPeerAPI(client, node.Name, node.URL, node.EnvironmentID)
		if err := api.verify(ctx, 1, true); err != nil {
			availability[i] = err
			return
		}
		token, err := node.issue(ctx)
		if err != nil {
			availability[i] = err
			return
		}
		endpoints[i] = meshEndpoint{api: api, token: token}
	})
	available := slices.DeleteFunc(endpoints, func(node meshEndpoint) bool { return node.token == "" })
	validated := errors.Join(validation, errors.Join(availability...))
	if len(available) < 2 {
		return 0, errors.Join(validated, ctx.Err())
	}

	counts := make([]int, len(available))
	failures := make([]error, len(available))
	forMeshNode(ctx, len(available), func(i int) {
		counts[i], failures[i] = reconcileSource(ctx, available[i], available)
	})
	var count int
	for _, n := range counts {
		count += n
	}
	return count, errors.Join(validated, errors.Join(failures...), ctx.Err())
}

func reconcileSource(ctx context.Context, source meshEndpoint, targets []meshEndpoint) (int, error) {
	entries, err := listPeerLinks(ctx, source.api, source.token)
	if err != nil {
		return 0, err
	}
	previous := make(map[string]link, len(entries))
	for _, entry := range entries {
		previous[entry.EnvironmentID] = entry
	}
	var count int
	var failures []error
	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		if source.api.environmentID == target.api.environmentID {
			continue
		}
		err := ensureListedLink(ctx, source.api, source.token, target.api, target.token, previous[target.api.environmentID])
		if err != nil {
			failures = append(failures, fmt.Errorf("%s to %s: %w", source.api.name, target.api.name, err))
		} else {
			count++
		}
	}
	return count, errors.Join(failures...)
}

func normalizeMeshNodes(candidates []MeshNode) ([]MeshNode, error) {
	nodes := make([]MeshNode, 0, len(candidates))
	addresses := make(map[string]string, len(candidates))
	identities := make(map[string]string, len(candidates))
	var failures []error
	for _, node := range candidates {
		origin, err := privateOrigin(node.URL)
		if !sshAlias.MatchString(node.Name) || !environmentID.MatchString(node.EnvironmentID) || err != nil {
			failures = append(failures, fmt.Errorf("invalid private T3 environment %q", node.Name))
			continue
		}
		key := strings.ToLower(origin)
		if previous := identities[node.EnvironmentID]; previous != "" && previous != key {
			failures = append(failures, fmt.Errorf("%s duplicates another T3 environment's identity", node.Name))
			continue
		}
		if previous := addresses[key]; previous != "" {
			if previous != node.EnvironmentID {
				failures = append(failures, fmt.Errorf("%s duplicates another T3 environment's address", node.Name))
			}
			continue
		}
		identities[node.EnvironmentID], addresses[key] = key, node.EnvironmentID
		node.URL = origin
		nodes = append(nodes, node)
	}
	return nodes, errors.Join(failures...)
}

func (node MeshNode) issue(ctx context.Context) (string, error) {
	if node.IssueToken != nil {
		token, err := node.IssueToken(ctx)
		if err == nil && token != "" {
			return token, nil
		}
	}
	return "", fmt.Errorf("could not authorize T3 peer administration on %s", node.Name)
}

func forMeshNode(ctx context.Context, count int, run func(int)) {
	workers := min(count, 6)
	var group sync.WaitGroup
	for worker := range workers {
		group.Go(func() {
			for i := worker; i < count && ctx.Err() == nil; i += workers {
				run(i)
			}
		})
	}
	group.Wait()
}

// PruneMeshSandboxLinks visits canonical sandbox links on managed cloud nodes.
// Only ready nodes with a read-only inventory callback are inspected in dry runs.
func PruneMeshSandboxLinks(ctx context.Context, sandboxes []MeshNode, dryRun bool,
	visit func(context.Context, SandboxLink, func() error) (bool, error),
) ([]SandboxLink, error) {
	nodes, validation := normalizeMeshNodes(sandboxes)
	if len(nodes) == 0 {
		return nil, validation
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := newHTTPClient(35 * time.Second)
	defer client.CloseIdleConnections()
	results := make([][]SandboxLink, len(nodes))
	failures := make([]error, len(nodes))
	forMeshNode(ctx, len(nodes), func(i int) {
		results[i], failures[i] = pruneMeshNode(ctx, client, nodes[i], dryRun, visit)
	})
	return slices.Concat(results...), errors.Join(validation, errors.Join(failures...), ctx.Err())
}

func pruneMeshNode(ctx context.Context, client *http.Client, node MeshNode, dryRun bool,
	visit func(context.Context, SandboxLink, func() error) (bool, error),
) ([]SandboxLink, error) {
	if dryRun && node.ListLinksJSON == nil {
		return nil, nil
	}
	api := newPeerAPI(client, node.Name, node.URL, node.EnvironmentID)
	if err := api.verify(ctx, 1, true); err != nil {
		return nil, err
	}
	var entries []link
	var token string
	if node.ListLinksJSON != nil {
		output, err := node.ListLinksJSON(ctx)
		if err != nil || len(output) > jsonLimit || json.Unmarshal(output, &entries) != nil || entries == nil {
			return nil, fmt.Errorf("could not list T3 environments on %s", node.Name)
		}
	} else {
		var err error
		token, err = node.issue(ctx)
		if err != nil {
			return nil, err
		}
		entries, err = listPeerLinks(ctx, api, token)
		if err != nil {
			return nil, err
		}
	}
	return pruneEntries(ctx, Peer{Host: node.Name, URL: node.URL}, entries, dryRun, visit, func(sandbox SandboxLink) error {
		if token == "" {
			var err error
			token, err = node.issue(ctx)
			if err != nil {
				return err
			}
		}
		var result struct {
			Removed bool `json:"removed"`
		}
		return api.rpc(ctx, token, "peerLinks.unlink", map[string]string{"environmentId": sandbox.EnvironmentID}, &result)
	})
}
