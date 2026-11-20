package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/gofrs/flock"
)

// A bare down remembers its exact target through lost responses and retries.
// Keep only identity and display fields from the original sandbox.
type downReceipt struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Created        int64  `json:"created"`
	Provider       string `json:"provider"`
	Template       string `json:"template"`
	Status         string `json:"status"`
	CleanupPending bool   `json:"cleanupPending,omitempty"`
}

func (r downReceipt) instance() agent37.Instance {
	return agent37.Instance{ID: r.ID, Name: r.Name, Created: r.Created, Template: r.Template, Status: r.Status,
		Metadata: map[string]string{"provider": r.Provider}}
}

func runLatestDown(ctx context.Context, client *agent37.Client, opts options, output, diagnostic io.Writer) (err error) {
	state, err := statePath()
	if err != nil {
		return err
	}
	// A workstation can use more than one workspace key. Keep their receipts
	// independent without persisting the workspace credential.
	scope := sha256.Sum256([]byte(client.Key))
	path := filepath.Join(state, "last-down-"+hex.EncodeToString(scope[:16])+".json")
	if !opts.dryRun {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return err
		}
		lock := flock.New(path + ".selection.lock")
		locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("could not lock the latest down operation")
		}
		defer func() { err = errors.Join(err, lock.Unlock()) }()
	}

	original, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var receipt downReceipt
	if original != nil {
		if json.Unmarshal(original, &receipt) != nil || agent37.ValidateID(receipt.ID) != nil ||
			!namePattern.MatchString(receipt.Name) || receipt.Created <= 0 {
			return errors.New("invalid saved down target. Use delete <name> to choose a sandbox")
		}
	}
	if receipt.ID == "" || receipt.Status == "deleted" {
		instances, err := client.List(ctx)
		if err != nil {
			return err
		}
		// Once deletion succeeds, an older sandbox cannot become the target of
		// a replay. A sandbox created later starts a new bare down operation.
		if receipt.ID != "" {
			instances = slices.DeleteFunc(instances, func(instance agent37.Instance) bool {
				return instance.Created > 0 && instance.Created <= receipt.Created
			})
		}
		latest, err := latestManaged(instances)
		if err != nil {
			return err
		}
		if latest.ID != "" {
			receipt = downReceipt{ID: latest.ID, Name: latest.Metadata["environment"], Created: latest.Created,
				Provider: displayProvider(latest), Template: latest.Template, Status: latest.Status,
				CleanupPending: latest.Metadata["tailnet"] == "tailscale"}
			if !opts.dryRun {
				if err := saveConfigJSON(ctx, path, original, receipt); err != nil {
					return fmt.Errorf("record the down target: %w", err)
				}
				original, err = os.ReadFile(path)
				if err != nil {
					return err
				}
			}
		}
	}
	if receipt.ID == "" {
		notice(output, info, "No managed Agent37 sandboxes.")
		return nil
	}
	if opts.dryRun {
		return previewSandboxDown(output, receipt.instance(), true)
	}
	opts.name, opts.targetID = receipt.Name, receipt.ID
	lock, err := lockSandbox(ctx, client, state, &opts)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	if receipt.Status == "deleted" {
		if receipt.CleanupPending && cleanupSandboxLinks(ctx, client, receipt.Name, receipt.ID, diagnostic) {
			receipt.CleanupPending = false
			if err := saveConfigJSON(ctx, path, original, receipt); err != nil {
				return err
			}
		}
		return showLifecycleResult(output, "Sandbox already deleted", receipt.instance(), "")
	}
	instance, err := client.Get(ctx, receipt.ID)
	if err != nil && !sandboxNotFound(err) {
		return err
	}
	if err == nil && (instance.ID != receipt.ID || instance.Status != "deleted" && (!isManaged(instance) || instance.Metadata["environment"] != receipt.Name)) {
		return errors.New("the selected sandbox changed. Use delete <name> to choose a sandbox")
	}
	title := "Sandbox already deleted"
	if instance.ID != "" && instance.Status != "deleted" {
		instance, err = applySandboxDown(ctx, client, instance, receipt.Name, state, true)
		if err != nil {
			return err
		}
		if receipt.CleanupPending {
			showDesktopCleanup(ctx, receipt.Name, diagnostic)
		}
		title = "Sandbox deleted"
	}
	receipt.Status = "deleted"
	if receipt.CleanupPending {
		receipt.CleanupPending = !cleanupSandboxLinks(ctx, client, receipt.Name, receipt.ID, diagnostic)
	}
	return errors.Join(showLifecycleResult(output, title, receipt.instance(), ""), saveConfigJSON(ctx, path, original, receipt))
}

func sandboxNotFound(err error) bool {
	apiError, ok := errors.AsType[*agent37.Error](err)
	return ok && apiError.Status == http.StatusNotFound
}

func applySandboxDown(ctx context.Context, client *agent37.Client, instance agent37.Instance, name, state string, deleteSandbox bool) (agent37.Instance, error) {
	instance, err := stopSandbox(ctx, client, instance, name)
	if err != nil || !deleteSandbox {
		return instance, err
	}
	if err := client.Delete(ctx, instance.ID); err != nil {
		return instance, err
	}
	_ = os.Remove(filepath.Join(state, name+".pending"))
	instance.Status = "deleted"
	return instance, nil
}

func finishSandboxDown(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string, output, diagnostic io.Writer) error {
	title := "Sandbox stopped"
	if instance.Status == "deleted" {
		title = "Sandbox deleted"
	}
	if instance.Metadata["tailnet"] == "tailscale" {
		if instance.Status == "deleted" {
			showDesktopCleanup(ctx, name, diagnostic)
		}
		cleanupSandboxLinks(ctx, client, name, instance.ID, diagnostic)
	}
	return showLifecycleResult(output, title, instance, "")
}

func showDesktopCleanup(ctx context.Context, name string, diagnostic io.Writer) {
	if _, err := desktopLauncher(ctx); err == nil {
		notice(diagnostic, info, "Remove any saved entry for %s under T3 Code → Settings → Connections.", name)
	}
}

func cleanupSandboxLinks(ctx context.Context, client *agent37.Client, name, id string, diagnostic io.Writer) bool {
	if err := unlinkSandbox(ctx, client, name, id, diagnostic); err != nil {
		notice(diagnostic, warning, "T3 cleanup pending for %s. Run t3-agent37 sync to retry. %v", name, err)
		return false
	}
	return true
}

func previewSandboxDown(output io.Writer, instance agent37.Instance, deleteSandbox bool) error {
	if instance.Status == "deleted" {
		if _, terminal := ttyOutput(output); terminal {
			return printSandboxResult(output, "Preview · sandbox already deleted", instance, "")
		}
		target := summary(instance)
		target["action"] = "none"
		return jsonOutput(output, target, true)
	}
	title, action := "Preview · stop sandbox", "stop"
	if deleteSandbox {
		title, action = "Preview · delete sandbox and disk", "delete"
	}
	if _, terminal := ttyOutput(output); terminal {
		return printSandboxResult(output, title, instance, "")
	}
	target := summary(instance)
	target["action"] = action
	return jsonOutput(output, target, true)
}

func showAbsentSandbox(output io.Writer, name string, dryRun bool) error {
	if _, terminal := ttyOutput(output); terminal {
		notice(output, info, "No managed Agent37 sandbox named %s.", name)
		return nil
	}
	result := map[string]string{"name": name, "status": "absent"}
	if dryRun {
		result["action"] = "none"
	}
	return jsonOutput(output, result, dryRun)
}
