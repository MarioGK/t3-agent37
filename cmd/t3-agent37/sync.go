package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

func unlinkSandbox(ctx context.Context, client *agent37.Client, name, id string, diagnostic io.Writer) error {
	return unlinkSandboxes(ctx, client, map[string]string{id: name}, diagnostic)
}

func unlinkSandboxes(ctx context.Context, client *agent37.Client, targets map[string]string, diagnostic io.Writer) error {
	configured, configErr := peers.Configured()
	visit := func(_ context.Context, link peers.SandboxLink, remove func() error) (bool, error) {
		if targets[link.SandboxID] != link.Name {
			return false, nil
		}
		err := remove()
		return err == nil, err
	}
	fleetRemoved, fleetErr := peers.PruneSandboxLinks(ctx, configured, false, visit)
	instances, inventoryErr := client.List(ctx)
	instances = slices.DeleteFunc(instances, func(instance agent37.Instance) bool { return targets[instance.ID] != "" })
	nodes, discoverErr := discoverCloudNodes(ctx, client, instances, "", false, true)
	cloudRemoved, cloudErr := peers.PruneMeshSandboxLinks(ctx, nodes, false, visit)
	for _, link := range append(fleetRemoved, cloudRemoved...) {
		notice(diagnostic, success, "Removed %s from T3 on %s.", link.Name, link.Peer)
	}
	return errors.Join(configErr, fleetErr, inventoryErr, discoverErr, cloudErr)
}

func syncSandboxLinks(ctx context.Context, client *agent37.Client, dryRun bool, output, diagnostic io.Writer) error {
	configured, configErr := peers.Configured()
	return errors.Join(configErr, syncSandboxLinksWithDiscovery(ctx, client, configured, dryRun, output, diagnostic,
		func(ctx context.Context, instances []agent37.Instance) ([]peers.MeshNode, error) {
			return discoverCloudNodes(ctx, client, instances, "", true, true)
		}))
}

func syncSandboxLinksWithDiscovery(ctx context.Context, client *agent37.Client, configured []peers.Peer, dryRun bool, output, diagnostic io.Writer, discover func(context.Context, []agent37.Instance) ([]peers.MeshNode, error)) error {
	state, err := statePath()
	if err != nil {
		return err
	}
	if !dryRun {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	active := make(map[string]string)
	visit := func(ctx context.Context, link peers.SandboxLink, remove func() error) (pruned bool, err error) {
		if active[link.SandboxID] == link.Name {
			return false, nil
		}
		if !dryRun {
			lock := sandboxLock(state, link.Name)
			locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
			if err != nil {
				return false, err
			}
			if !locked {
				return false, fmt.Errorf("could not acquire the lifecycle lock for %s", link.Name)
			}
			defer func() { err = errors.Join(err, lock.Unlock()) }()
		}
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		stale, err := staleSandboxLink(checkCtx, client, link, state)
		if err != nil || !stale {
			return false, err
		}
		err = remove()
		return err == nil, err
	}
	var removed []peers.SandboxLink
	var syncErr error
	if dryRun {
		removed, syncErr = peers.PruneSandboxLinks(ctx, configured, true, visit)
	} else {
		instances, inventoryErr := client.List(ctx)
		if inventoryErr == nil {
			// The fresh inventory is enough to keep active links. A stale
			// candidate still needs a locked, exact Agent37 lookup.
			for _, instance := range instances {
				if isManaged(instance) && instance.Metadata["tailnet"] == "tailscale" &&
					(instance.Status == "running" || instance.Status == "sleeping") {
					active[instance.ID] = instance.Metadata["environment"]
				}
			}
		}
		nodes, discoveryErr := discover(ctx, instances)
		count, pruned, meshErr := peers.SyncMesh(ctx, configured, nodes, visit)
		reportMesh(count, diagnostic)
		removed = pruned
		syncErr = errors.Join(inventoryErr, discoveryErr, meshErr, ctx.Err())
	}
	if _, terminal := ttyOutput(output); terminal {
		return errors.Join(printSyncResult(output, removed, dryRun, syncErr != nil), syncErr)
	}
	if removed == nil {
		removed = []peers.SandboxLink{}
	}
	return errors.Join(jsonOutput(output, removed, true), syncErr)
}

func printSyncResult(output io.Writer, links []peers.SandboxLink, preview, incomplete bool) error {
	title, label := "T3 sync", "Removed"
	if preview {
		title, label = "Preview · T3 sync", "Would remove"
	}
	fields := []detail{{label, fmt.Sprintf("%d stale links", len(links))}}
	if incomplete {
		fields = append(fields, detail{"Status", "Some environments could not be checked"})
	}
	for _, link := range links {
		fields = append(fields, detail{link.Name, link.Peer})
	}
	return printDetails(output, title, fields...)
}

func staleSandboxLink(ctx context.Context, client *agent37.Client, link peers.SandboxLink, state string) (bool, error) {
	instance, err := client.Get(ctx, link.SandboxID)
	if sandboxNotFound(err) {
		// An absent ID can also belong to a different workspace. Only an
		// exact local deletion record can disambiguate that response.
		for _, path := range []string{deletedSandboxPath(state, link.SandboxID), filepath.Join(state, "last-down-"+keyScope(client)+".json")} {
			receipt, _, err := readDownReceipt(path)
			if err != nil {
				return false, err
			}
			if receipt.ID == link.SandboxID && receipt.Name == link.Name && receipt.Status == "deleted" {
				return true, nil
			}
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("could not check sandbox %s for T3 on %s: %w", link.Name, link.Peer, err)
	}
	if instance.ID != link.SandboxID || instance.Metadata["managed_by"] != owner ||
		instance.Metadata["environment"] != link.Name || instance.Metadata["tailnet"] != "tailscale" {
		return false, nil
	}
	return instance.Status == "stopped" || instance.Status == "deleted", nil
}
