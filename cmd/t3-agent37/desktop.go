package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/MarioGK/t3-agent37/internal/peers"
)

const desktopConfirmationKey = "desktop_confirmation"

const (
	desktopPollInterval   = 500 * time.Millisecond
	desktopConnectGrace   = 10 * time.Second
	maxDesktopProbeOutage = 30 * time.Second
)

type desktopPairingStatus struct {
	Confirmed bool
	Redeemed  bool
	Changed   bool
}

func requiresDesktopConfirmation(instance agent37.Instance) bool {
	return instance.Metadata[desktopConfirmationKey] == "required"
}

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

func handOffPairing(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string, proof setupProof, launcher []string, diagnostic io.Writer) error {
	confirmation, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	open := func(ctx context.Context) (string, error) {
		var nonce [16]byte
		_, _ = rand.Read(nonce[:])
		label := "t3-agent37-desktop-" + hex.EncodeToString(nonce[:])
		pairing, err := pairLink(ctx, client, instance.ID, proof.TailnetURL, label)
		if err != nil {
			return "", err
		}
		if err := openDesktopPairing(ctx, launcher, pairing); err != nil {
			return "", err
		}
		notice(diagnostic, info, "Confirm %s in T3 Code. Waiting before linking environments…", name)
		return label, nil
	}
	check := func(ctx context.Context, label, previous string) (desktopPairingStatus, error) {
		command := remote + " pair-status " + shellQuote(label) + " " + shellQuote(proof.EnvironmentID)
		if previous != "" {
			command += " " + shellQuote(previous)
		}
		attempt, stop := context.WithTimeout(ctx, 8*time.Second)
		defer stop()
		return client.ExecJSON[desktopPairingStatus](attempt, instance.ID, command)
	}
	err := waitForDesktop(confirmation, open, check, func() {
		notice(diagnostic, warning, "T3 Code accepted the pairing for %s but did not connect. Opening a fresh request…", name)
	})
	if err == nil {
		notice(diagnostic, success, "T3 Code confirmed %s.", name)
	}
	return err
}

func openDesktopPairing(ctx context.Context, launcher []string, pairing string) error {
	args := append(slices.Clone(launcher[1:]), desktopPairingURL(pairing))
	for attempt := range 2 {
		openCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		// The error and output could contain the one-time link.
		err := exec.CommandContext(openCtx, launcher[0], args...).Run()
		cancel()
		if err == nil {
			return nil
		}
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(desktopPollInterval):
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("could not open the pairing request in T3 Code after retrying")
}

func waitForDesktop(ctx context.Context, open func(context.Context) (string, error), check func(context.Context, string, string) (desktopPairingStatus, error), onRetry func()) error {
	label, err := open(ctx)
	if err != nil {
		return err
	}
	var previous string
	var redeemedAt, outageSince time.Time
	delay := desktopPollInterval
	for ctx.Err() == nil {
		state, err := check(ctx, label, previous)
		if ctx.Err() != nil {
			break
		}
		switch {
		case err != nil:
			if outageSince.IsZero() {
				outageSince = time.Now()
			}
			if time.Since(outageSince) >= maxDesktopProbeOutage {
				return fmt.Errorf("could not check T3 desktop pairing after retrying: %w", err)
			}
			delay = min(delay*2, 4*time.Second)
		case state.Changed:
			return errors.New("T3 environment changed during desktop pairing")
		case state.Confirmed:
			return nil
		default:
			outageSince = time.Time{}
			delay = desktopPollInterval
			if state.Redeemed && redeemedAt.IsZero() {
				redeemedAt = time.Now()
			}
			if !redeemedAt.IsZero() && time.Since(redeemedAt) >= desktopConnectGrace {
				if previous != "" {
					return errors.New("T3 Code accepted the pairing but did not connect after retrying")
				}
				onRetry()
				previous = label
				label, err = open(ctx)
				if err != nil {
					return err
				}
				redeemedAt = time.Time{}
				continue
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("desktop confirmation timed out")
	}
	return ctx.Err()
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
		if width, terminal := ttyOutput(output); terminal {
			if len(targets) == 0 {
				return printDetails(output, "Preview · link sandboxes", detail{"Targets", "No managed sandboxes"})
			}
			return printSandboxTable(output, targets, width, "Preview · link sandboxes")
		}
		rows := make([]map[string]any, 0, len(targets))
		for _, instance := range targets {
			rows = append(rows, summary(instance))
		}
		return jsonOutput(output, rows, true)
	}
	if len(targets) == 0 {
		notice(output, info, "No managed Agent37 sandboxes.")
		return nil
	}
	launcher, err := desktopLauncher(ctx)
	if err != nil {
		return err
	}
	configured, err := peers.Configured()
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
		_, err := pairSandbox(ctx, client, instance, setupProof{}, state, name, launcher, diagnostic, func(paired agent37.Instance, proof setupProof) {
			linkStartedSandbox(ctx, client, paired, name, configured, proof, diagnostic)
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		}
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

func pairSandbox(ctx context.Context, client *agent37.Client, target agent37.Instance, proof setupProof, state, name string, launcher []string, diagnostic io.Writer, onConfirmed func(agent37.Instance, setupProof)) (_ setupProof, err error) {
	if !namePattern.MatchString(name) || agent37.ValidateID(target.ID) != nil {
		return proof, errors.New("Agent37 returned an invalid sandbox identity")
	}
	lock := sandboxLock(state, name)
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return proof, err
	}
	if !locked {
		return proof, errors.New("could not acquire the sandbox lifecycle lock")
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()

	instance, err := client.Get(ctx, target.ID)
	if err != nil {
		return proof, err
	}
	if instance.ID != target.ID || !isManaged(instance) || instance.Metadata["environment"] != name {
		return proof, errors.New("the selected sandbox changed")
	}
	if instance.Metadata["tailnet"] != "tailscale" {
		return proof, errors.New("desktop pairing requires a Tailscale sandbox")
	}
	switch instance.Status {
	case "running", "stopped", "sleeping":
	default:
		return proof, errors.New("wait for the sandbox to become ready before retrying link")
	}
	if proof.TailnetURL == "" || instance.Status != "running" {
		instance, proof, err = startSandbox(ctx, client, instance, name, true, false, diagnostic)
		if err != nil {
			return proof, err
		}
	}
	if err := handOffPairing(ctx, client, instance, name, proof, launcher, diagnostic); err != nil {
		return proof, err
	}
	proof.DesktopConfirmed = true
	if onConfirmed != nil {
		onConfirmed(instance, proof)
	}
	return proof, nil
}
