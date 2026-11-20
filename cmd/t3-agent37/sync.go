package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

func unlinkSandbox(ctx context.Context, name, id string, diagnostic io.Writer) error {
	configured, err := peers.Configured()
	if err != nil {
		return err
	}
	removed, err := peers.PruneSandboxLinks(ctx, configured, false, func(_ context.Context, link peers.SandboxLink, remove func() error) (bool, error) {
		if link.Name != name || link.SandboxID != id {
			return false, nil
		}
		err := remove()
		return err == nil, err
	})
	for _, link := range removed {
		fmt.Fprintf(diagnostic, "Removed %s from T3 on %s.\n", name, link.Peer)
	}
	return err
}

func syncSandboxLinks(ctx context.Context, client *agent37.Client, dryRun bool, output io.Writer) error {
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
	removed, err := peers.PruneSandboxLinks(ctx, configured, dryRun, func(ctx context.Context, link peers.SandboxLink, remove func() error) (pruned bool, err error) {
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
	})
	if removed == nil {
		removed = []peers.SandboxLink{}
	}
	return errors.Join(jsonOutput(output, removed, true), err)
}

func staleSandboxLink(ctx context.Context, client *agent37.Client, link peers.SandboxLink) (bool, error) {
	instance, err := client.Get(ctx, link.SandboxID)
	if apiError, ok := errors.AsType[*agent37.Error](err); ok && apiError.Status == http.StatusNotFound {
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
