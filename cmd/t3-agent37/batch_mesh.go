package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

// A fresh inventory prevents reuse of setup proof for a sandbox that has
// since been stopped or deleted. All other nodes still use normal discovery.
func batchMeshNodes(ctx context.Context, client *agent37.Client, inventory []agent37.Instance, results []batchSandbox) ([]peers.MeshNode, error) {
	batch := make(map[string]batchSandbox, len(results))
	for _, item := range results {
		if item.instance.ID != "" {
			batch[item.instance.ID] = item
		}
	}
	var nodes []peers.MeshNode
	var others []agent37.Instance
	var failures []error
	for _, instance := range inventory {
		item, fresh := batch[instance.ID]
		if !fresh || instance.Status != "running" {
			others = append(others, instance)
			continue
		}
		delete(batch, instance.ID)
		if item.proof.TailnetURL == "" {
			continue
		}
		if !isManaged(instance) || instance.Metadata["environment"] != item.name ||
			instance.Metadata["tailnet"] != "tailscale" || agent37.ValidateID(instance.ID) != nil ||
			!namePattern.MatchString(item.name) {
			failures = append(failures, fmt.Errorf("%s: invalid managed sandbox identity", item.name))
			continue
		}
		pending := requiresDesktopConfirmation(instance) && !item.proof.DesktopConfirmed
		node, err := meshNode(client, instance.ID, item.name, sandboxPeer{
			EnvironmentID: item.proof.EnvironmentID, URL: item.proof.TailnetURL, PeerProxy: item.proof.PeerProxy,
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", item.name, err))
			continue
		}
		if !pending && (!item.proof.PeerProxy || item.proof.EnvironmentID == "") {
			failures = append(failures, fmt.Errorf("%s: sandbox needs a fresh boot for T3 peer routing", item.name))
		}
		node.CleanupOnly = pending || item.err != nil || !item.proof.PeerProxy || item.proof.EnvironmentID == ""
		nodes = append(nodes, node)
	}
	for _, item := range batch {
		if item.err == nil {
			failures = append(failures, fmt.Errorf("%s: sandbox is no longer running", item.name))
		}
	}
	discovered, err := discoverCloudNodes(ctx, client, others, "", true, true)
	return append(nodes, discovered...), errors.Join(errors.Join(failures...), err)
}
