package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

// desktopLauncher uses the OS's registered handler for the official pairing URI.
func desktopLauncher(ctx context.Context) ([]string, error) {
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var command []string
	switch runtime.GOOS {
	case "darwin":
		command = []string{"open"}
	case "windows":
		if err := exec.CommandContext(probe, "reg.exe", "query", `HKCR\t3code\shell\open\command`, "/ve").Run(); err != nil {
			return nil, errors.New("install and launch T3 Code to register its desktop link handler")
		}
		command = []string{"rundll32.exe", "url.dll,FileProtocolHandler"}
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return nil, errors.New("open a terminal on the PC running T3 Code")
		}
		handler, err := exec.CommandContext(probe, "xdg-mime", "query", "default", "x-scheme-handler/t3code").Output()
		if err != nil || strings.TrimSpace(string(handler)) == "" {
			return nil, errors.New("install and launch T3 Code to register its desktop link handler")
		}
		command = []string{"xdg-open"}
	}
	if _, err := exec.LookPath(command[0]); err != nil {
		return nil, errors.New("this workstation has no desktop link launcher")
	}
	return command, nil
}

func desktopPairingURL(pairing string) string {
	return "t3code://pair?" + url.Values{"pairingUrl": {pairing}}.Encode()
}

func handOffPairing(ctx context.Context, client *agent37.Client, instance agent37.Instance, origin string, launcher []string) error {
	pairing, err := pairLink(ctx, client, instance.ID, origin)
	if err != nil {
		return err
	}
	args := append(slices.Clone(launcher[1:]), desktopPairingURL(pairing))
	openCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// Do not forward launcher errors or output that could contain the one-time link.
	if err := exec.CommandContext(openCtx, launcher[0], args...).Run(); err != nil {
		return errors.New("could not open the pairing request in T3 Code")
	}
	return nil
}

func linkSandboxes(ctx context.Context, client *agent37.Client, opts options, output, diagnostic io.Writer) error {
	instances, err := client.List(ctx)
	if err != nil {
		return err
	}
	targets, err := desktopTargets(instances, opts.name)
	if err != nil {
		return err
	}
	if opts.dryRun {
		rows := make([]map[string]any, 0, len(targets))
		for _, instance := range targets {
			rows = append(rows, summary(instance))
		}
		return jsonOutput(output, rows, true)
	}
	if len(targets) == 0 {
		fmt.Fprintln(output, "No managed Agent37 sandboxes.")
		return nil
	}
	launcher, err := desktopLauncher(ctx)
	if err != nil {
		return err
	}
	state, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}

	var failures []error
	for _, instance := range targets {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		name := instance.Metadata["environment"]
		err := pairExistingSandbox(ctx, client, instance, state, name, launcher, diagnostic)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			continue
		}
		fmt.Fprintf(output, "Sent %s to T3 Code. Confirm its private address in the app.\n", name)
	}
	return errors.Join(failures...)
}

func desktopTargets(instances []agent37.Instance, name string) ([]agent37.Instance, error) {
	if name != "" {
		instance, err := namedManaged(instances, name)
		if err != nil {
			return nil, err
		}
		if instance.ID == "" {
			return nil, fmt.Errorf("no managed Agent37 environment named %s", name)
		}
		return []agent37.Instance{instance}, nil
	}
	var targets []agent37.Instance
	for _, instance := range instances {
		if isManaged(instance) {
			targets = append(targets, instance)
		}
	}
	slices.SortFunc(targets, func(a, b agent37.Instance) int {
		return strings.Compare(a.Metadata["environment"], b.Metadata["environment"])
	})
	return targets, nil
}

func pairExistingSandbox(ctx context.Context, client *agent37.Client, target agent37.Instance, state, name string, launcher []string, diagnostic io.Writer) (err error) {
	if !namePattern.MatchString(name) || agent37.ValidateID(target.ID) != nil {
		return errors.New("Agent37 returned an invalid sandbox identity")
	}
	lock := sandboxLock(state, name)
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("could not acquire the sandbox lifecycle lock")
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()

	instance, err := client.Get(ctx, target.ID)
	if err != nil {
		return err
	}
	if instance.ID != target.ID || !isManaged(instance) || instance.Metadata["environment"] != name {
		return errors.New("the selected sandbox changed")
	}
	if instance.Metadata["tailnet"] != "tailscale" {
		return errors.New("desktop pairing requires a Tailscale sandbox")
	}
	switch instance.Status {
	case "running", "stopped", "sleeping":
	default:
		return errors.New("wait for the sandbox to become ready before retrying link")
	}
	_, proof, err := startSandbox(ctx, client, instance, name, false, false, diagnostic)
	if err != nil {
		return err
	}
	return handOffPairing(ctx, client, instance, proof.TailnetURL, launcher)
}
