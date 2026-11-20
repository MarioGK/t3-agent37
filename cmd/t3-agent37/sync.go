package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

func unlinkSandbox(ctx context.Context, client *agent37.Client, name, id string, diagnostic io.Writer) error {
	configured, err := peers.Configured()
	if err != nil {
		return err
	}
	visit := func(_ context.Context, link peers.SandboxLink, remove func() error) (bool, error) {
		if link.Name != name || link.SandboxID != id {
			return false, nil
		}
		err := remove()
		return err == nil, err
	}
	fleetRemoved, fleetErr := peers.PruneSandboxLinks(ctx, configured, false, visit)
	instances, inventoryErr := client.List(ctx)
	nodes, discoverErr := cloudMeshNodes(ctx, client, instances, id, false)
	cloudRemoved, cloudErr := peers.PruneMeshSandboxLinks(ctx, nodes, false, visit)
	for _, link := range append(fleetRemoved, cloudRemoved...) {
		notice(diagnostic, success, "Removed %s from T3 on %s.", name, link.Peer)
	}
	return errors.Join(fleetErr, inventoryErr, discoverErr, cloudErr)
}

func syncSandboxLinks(ctx context.Context, client *agent37.Client, dryRun bool, output, diagnostic io.Writer) error {
	configured, err := peers.Configured()
	if err != nil {
		return err
	}
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
	instances, inventoryErr := client.List(ctx)
	nodes, discoveryErr := cloudMeshNodes(ctx, client, instances, "", !dryRun)
	visit := func(ctx context.Context, link peers.SandboxLink, remove func() error) (pruned bool, err error) {
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
		stale, err := staleSandboxLink(checkCtx, client, link)
		if err != nil || !stale {
			return false, err
		}
		err = remove()
		return err == nil, err
	}
	fleetRemoved, fleetErr := peers.PruneSandboxLinks(ctx, configured, dryRun, visit)
	cloudRemoved, cloudErr := peers.PruneMeshSandboxLinks(ctx, nodes, dryRun, visit)
	var meshErr error
	if !dryRun {
		count, err := peers.ReconcileMesh(ctx, configured, nodes)
		reportMesh(count, diagnostic)
		meshErr = err
	}
	removed := append(fleetRemoved, cloudRemoved...)
	syncErr := errors.Join(inventoryErr, discoveryErr, fleetErr, cloudErr, meshErr)
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

func staleSandboxLink(ctx context.Context, client *agent37.Client, link peers.SandboxLink) (bool, error) {
	instance, err := client.Get(ctx, link.SandboxID)
	if sandboxNotFound(err) {
		// Retain links to a running T3 environment that may be in another workspace.
		reachable := link.Reachable(ctx)
		return !reachable, ctx.Err()
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
