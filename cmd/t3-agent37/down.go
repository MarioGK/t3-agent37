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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
	"github.com/gofrs/flock"
)

var (
	legacyReceiptName = regexp.MustCompile(`^last-down-[a-f0-9]{32}\.json$`)
	keyScopePattern   = regexp.MustCompile(`^[a-f0-9]{32}$`)
	errDownKeyChanged = errors.New("the Agent37 key changed since this target was saved. Use down --pick to confirm a target")
)

// A receipt binds a retry to an instance ID, even after its name is reused.
type downReceipt struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	APIName        *string `json:"apiName,omitempty"`
	Created        int64   `json:"created"`
	Provider       string  `json:"provider"`
	Template       string  `json:"template"`
	Status         string  `json:"status"`
	KeyScope       string  `json:"keyScope,omitempty"`
	CleanupPending bool    `json:"cleanupPending,omitzero"`
	TailnetPending bool    `json:"tailnetPending,omitzero"`
	ResultPending  bool    `json:"resultPending,omitzero"`
}

func newDownReceipt(instance agent37.Instance, scope string) downReceipt {
	return downReceipt{
		ID: instance.ID, Name: instance.Metadata["environment"], APIName: new(instance.Name),
		Created: instance.Created, Provider: displayProvider(instance), Template: instance.Template,
		Status: instance.Status, KeyScope: scope, ResultPending: true,
		CleanupPending: instance.Metadata["tailnet"] == "tailscale",
		TailnetPending: instance.Metadata["tailnet"] == "tailscale" && instance.Status != "stopped",
	}
}

func (r downReceipt) instance() agent37.Instance {
	name := r.Name // Legacy receipts stored only the environment name.
	if r.APIName != nil {
		name = *r.APIName
	}
	return agent37.Instance{ID: r.ID, Name: name, Created: r.Created, Template: r.Template, Status: r.Status,
		Metadata: map[string]string{"provider": r.Provider, "environment": r.Name}}
}

func (r downReceipt) complete() bool {
	return r.Status == "deleted" && !r.CleanupPending && !r.TailnetPending && !r.ResultPending
}

func keyScope(client *agent37.Client) string {
	scope := sha256.Sum256([]byte(client.Key))
	return hex.EncodeToString(scope[:16])
}

func readDownReceipt(path string) (downReceipt, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return downReceipt{}, nil, nil
	}
	if err != nil {
		return downReceipt{}, nil, err
	}
	var receipt downReceipt
	if json.Unmarshal(data, &receipt) != nil || agent37.ValidateID(receipt.ID) != nil ||
		!namePattern.MatchString(receipt.Name) || receipt.Created < 0 ||
		receipt.KeyScope != "" && !keyScopePattern.MatchString(receipt.KeyScope) {
		return downReceipt{}, nil, fmt.Errorf("invalid saved deletion target at %s", path)
	}
	return receipt, data, nil
}

func runRecordedDown(ctx context.Context, client *agent37.Client, opts options, output, diagnostic io.Writer) (err error) {
	state, err := statePath()
	if err != nil {
		return err
	}
	path := filepath.Join(state, "last-down.json")
	if !opts.latest {
		path = filepath.Join(state, "last-delete-"+opts.name+".json")
		if opts.targetID != "" {
			if err := agent37.ValidateID(opts.targetID); err != nil {
				return err
			}
			path = filepath.Join(state, "delete-id-"+opts.targetID+".json")
		}
	}
	if !opts.dryRun {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return err
		}
		lock := flock.New(filepath.Join(state, "deletion.lock"))
		locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("could not lock the deletion target")
		}
		defer func() { err = errors.Join(err, lock.Unlock()) }()
	}
	receipt, original, err := readDownReceipt(path)
	if err != nil {
		return err
	}
	scope := keyScope(client)
	if opts.latest && receipt.ID == "" {
		receipt, err = legacyDownReceipt(state, scope)
		if err != nil {
			return err
		}
	}
	if opts.latest && receipt.ID != "" && receipt.KeyScope != scope {
		// A previous picker confirmation may have moved the current key's
		// receipt aside. Its exact target is still valid for this key.
		archivedPath := filepath.Join(state, "last-down-"+scope+".json")
		archived, data, err := readDownReceipt(archivedPath)
		if err != nil {
			return err
		}
		if archived.ID != "" {
			archived.KeyScope = scope
			archived.ResultPending = archived.ResultPending || archived.Status != "deleted"
			path, receipt, original = archivedPath, archived, data
		}
	}
	if opts.targetID == "" && (receipt.ID == "" || receipt.complete()) {
		candidate, err := interruptedDeletion(state, path, scope, opts, receipt.KeyScope == scope)
		if err != nil {
			return err
		}
		if candidate != "" {
			path = candidate
			receipt, original, err = readDownReceipt(path)
			if err != nil {
				return err
			}
			if receipt.KeyScope == "" && filepath.Base(path) == "last-down-"+scope+".json" {
				receipt.KeyScope = scope
				receipt.ResultPending = true
			}
		}
	}
	if receipt.ID != "" && receipt.KeyScope != scope {
		if receipt.Status == "deleted" {
			return errDownKeyChanged
		}
		target, err := readDownTarget(ctx, client, receipt)
		if err != nil {
			return err
		}
		if target.Status == "deleted" {
			return errDownKeyChanged
		}
		receipt.KeyScope = scope
	}

	var selected agent37.Instance
	if receipt.ID == "" || receipt.complete() {
		selected, err = selectDownTarget(ctx, client, opts, receipt)
		if err != nil {
			return err
		}
		if selected.ID != "" {
			receipt = newDownReceipt(selected, scope)
		} else if receipt.ID == "" || !opts.latest {
			return showAbsentSandbox(output, opts.name, opts.dryRun)
		}
	}
	if opts.dryRun {
		if selected.ID == "" && receipt.Status != "deleted" {
			target, err := readDownTarget(ctx, client, receipt)
			if err != nil {
				return err
			}
			receipt.Status = target.Status
		}
		return previewSandboxLifecycle(output, receipt.instance(), "down")
	}

	opts.name, opts.targetID = receipt.Name, receipt.ID
	lock, err := lockSandbox(ctx, client, state, &opts)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	if err := completeRecordedDown(ctx, client, receipt, original, path, state, output, diagnostic); err != nil {
		return err
	}
	if opts.pickDelete {
		return rebindLatestDown(ctx, state, path, scope)
	}
	return nil
}

// A picker confirmation explicitly selects an ID in the new workspace. Keep the
// old receipt for its original key so an interrupted operation can still resume.
func rebindLatestDown(ctx context.Context, state, path, scope string) error {
	latestPath := filepath.Join(state, "last-down.json")
	previous, original, err := readDownReceipt(latestPath)
	if err != nil || previous.KeyScope == scope {
		return err
	}
	if previous.ID != "" {
		oldPath := filepath.Join(state, "last-down-"+previous.KeyScope+".json")
		_, old, err := readDownReceipt(oldPath)
		if err != nil {
			return err
		}
		if err := saveConfigJSON(ctx, oldPath, old, previous); err != nil {
			return err
		}
	}
	selected, _, err := readDownReceipt(path)
	if err != nil || selected.Created <= 0 {
		return err
	}
	return saveConfigJSON(ctx, latestPath, original, selected)
}

func interruptedDeletion(state, currentPath, scope string, opts options, knownScope bool) (string, error) {
	entries, err := os.ReadDir(state)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var match string
	for _, entry := range entries {
		name := entry.Name()
		legacy := !opts.latest && name == "last-down-"+scope+".json"
		if !legacy && name != "last-down.json" && !(opts.latest && strings.HasPrefix(name, "last-delete-") && strings.HasSuffix(name, ".json")) {
			continue
		}
		path := filepath.Join(state, name)
		if path == currentPath {
			continue
		}
		receipt, _, err := readDownReceipt(path)
		if err != nil {
			return "", err
		}
		pending := receipt.ResultPending || legacy && receipt.Status != "deleted"
		if !pending || (!opts.latest && receipt.Name != opts.name) || opts.latest && knownScope && receipt.KeyScope != scope {
			continue
		}
		if match != "" {
			return "", errors.New("more than one deletion needs a retry. Use down <name> to choose one")
		}
		match = path
	}
	return match, nil
}

func legacyDownReceipt(state, scope string) (downReceipt, error) {
	path := filepath.Join(state, "last-down-"+scope+".json")
	receipt, _, err := readDownReceipt(path)
	if err != nil {
		return downReceipt{}, err
	}
	if receipt.ID != "" {
		receipt.KeyScope = scope
		receipt.ResultPending = receipt.ResultPending || receipt.Status != "deleted"
		return receipt, nil
	}
	old, err := os.ReadDir(state)
	if errors.Is(err, os.ErrNotExist) {
		return downReceipt{}, nil
	}
	if err != nil {
		return downReceipt{}, err
	}
	for _, entry := range old {
		if legacyReceiptName.MatchString(entry.Name()) {
			return downReceipt{}, errDownKeyChanged
		}
	}
	return downReceipt{}, nil
}

func selectDownTarget(ctx context.Context, client *agent37.Client, opts options, receipt downReceipt) (agent37.Instance, error) {
	if opts.targetID != "" {
		if opts.targetID == receipt.ID {
			return agent37.Instance{}, nil
		}
		target := downReceipt{ID: opts.targetID, Name: opts.name}
		instance, err := readDownTarget(ctx, client, target)
		if instance.Status == "deleted" {
			return agent37.Instance{}, err
		}
		return instance, err
	}
	instances, err := client.List(ctx)
	if err != nil {
		return agent37.Instance{}, err
	}
	instances = slices.DeleteFunc(instances, func(instance agent37.Instance) bool {
		return receipt.ID != "" && (instance.ID == receipt.ID ||
			opts.latest && instance.Created > 0 && instance.Created < receipt.Created)
	})
	if !opts.latest {
		return namedManaged(instances, opts.name)
	}
	latest, err := latestManaged(instances)
	if err == nil && latest.ID != "" && receipt.ID != "" && latest.Created == receipt.Created {
		return agent37.Instance{}, errors.New("another sandbox has the same Agent37 creation time as the previous down target. Use down <name> to select it")
	}
	return latest, err
}

func readDownTarget(ctx context.Context, client *agent37.Client, receipt downReceipt) (agent37.Instance, error) {
	instance, err := client.Get(ctx, receipt.ID)
	if sandboxNotFound(err) {
		instance = receipt.instance()
		instance.Status = "deleted"
		return instance, nil
	}
	if err != nil {
		return instance, err
	}
	if instance.ID != receipt.ID || instance.Status != "deleted" && (!isManaged(instance) || instance.Metadata["environment"] != receipt.Name) {
		return instance, errors.New("the selected sandbox changed. Choose a target again")
	}
	return instance, nil
}

type recordedTarget struct {
	receipt        downReceipt
	original       []byte
	path           string
	alreadyDeleted bool
}

func (target *recordedTarget) save(ctx context.Context) error {
	if err := saveConfigJSON(ctx, target.path, target.original, target.receipt); err != nil {
		return err
	}
	data, err := os.ReadFile(target.path)
	if err == nil {
		target.original = data
	}
	return err
}

func (target *recordedTarget) deleteCloud(ctx context.Context, client *agent37.Client, state string) error {
	receipt := &target.receipt
	target.alreadyDeleted = true
	if receipt.Status != "deleted" {
		instance, err := readDownTarget(ctx, client, *receipt)
		if err != nil {
			return err
		}
		if instance.Status != "deleted" {
			receipt.CleanupPending = receipt.CleanupPending || instance.Metadata["tailnet"] == "tailscale"
			receipt.TailnetPending = receipt.TailnetPending || instance.Metadata["tailnet"] == "tailscale" && instance.Status != "stopped"
			if err := target.save(ctx); err != nil {
				return fmt.Errorf("record the deletion target: %w", err)
			}
			// The verified live sandbox has resolved any ambiguous create.
			if err := os.Remove(sandboxStatePath(state, receipt.Name, ".pending")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			receipt.TailnetPending, err = deleteSandbox(ctx, client, instance, receipt.Name)
			if err != nil {
				return err
			}
			target.alreadyDeleted = false
		}
		receipt.Status = "deleted"
		if err := recordDeletedSandbox(ctx, state, *receipt); err != nil {
			return err
		}
		return target.save(ctx)
	}
	return recordDeletedSandbox(ctx, state, *receipt)
}

func (receipt *downReceipt) cleanTailnet(ctx context.Context, diagnostic io.Writer) {
	if !receipt.TailnetPending {
		return
	}
	if err := removeDeletedTailnetDevice(ctx, *receipt); err != nil {
		notice(diagnostic, warning, "Tailscale cleanup pending for %s. Retry deletion to check its ephemeral device. %v", receipt.Name, err)
		return
	}
	receipt.TailnetPending = false
}

func completeRecordedDown(ctx context.Context, client *agent37.Client, receipt downReceipt, original []byte, path, state string, output, diagnostic io.Writer) error {
	target := recordedTarget{receipt: receipt, original: original, path: path}
	if err := target.deleteCloud(ctx, client, state); err != nil {
		return err
	}
	title := "Sandbox already deleted"
	if !target.alreadyDeleted {
		title = "Sandbox deleted"
		if target.receipt.CleanupPending {
			showDesktopCleanup(ctx, target.receipt.Name, diagnostic)
		}
	}
	if target.receipt.complete() {
		return showLifecycleResult(output, title, target.receipt.instance(), "")
	}
	target.receipt.cleanTailnet(ctx, diagnostic)
	if target.receipt.CleanupPending {
		target.receipt.CleanupPending = !cleanupSandboxLinks(ctx, client, target.receipt.Name, target.receipt.ID, diagnostic)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := showDeletionResult(output, title, target.receipt); err != nil {
		return err
	}
	target.receipt.ResultPending = false
	return target.save(ctx)
}

func removeDeletedTailnetDevice(ctx context.Context, receipt downReceipt) error {
	clientID, secret, err := tailscaleCredentials()
	if err != nil {
		return err
	}
	tags, err := workstationTailscaleTags()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return peers.RemoveSandboxDevice(ctx, clientID, secret, tags, "a37-"+receipt.Name+"-"+receipt.ID)
}

func showDeletionResult(output io.Writer, title string, receipt downReceipt) error {
	if !receipt.CleanupPending && !receipt.TailnetPending {
		return showLifecycleResult(output, title, receipt.instance(), "")
	}
	if _, terminal := ttyOutput(output); terminal {
		var pending []detail
		if receipt.CleanupPending {
			pending = append(pending, detail{"T3 cleanup", "Pending"})
		}
		if receipt.TailnetPending {
			pending = append(pending, detail{"Tailscale cleanup", "Pending"})
		}
		return printSandboxResult(output, title, receipt.instance(), "", pending...)
	}
	row := summary(receipt.instance())
	row["cleanup_pending"] = receipt.CleanupPending
	row["tailscale_cleanup_pending"] = receipt.TailnetPending
	return jsonOutput(output, row, false)
}

func deletedSandboxPath(state, id string) string {
	return filepath.Join(state, "deleted", id+".json")
}

func recordDeletedSandbox(ctx context.Context, state string, receipt downReceipt) error {
	path := deletedSandboxPath(state, receipt.ID)
	saved, original, err := readDownReceipt(path)
	if err != nil || saved.ID == receipt.ID && saved.Name == receipt.Name {
		return err
	}
	return saveConfigJSON(ctx, path, original, receipt)
}

func sandboxNotFound(err error) bool {
	apiError, ok := errors.AsType[*agent37.Error](err)
	return ok && apiError.Status == http.StatusNotFound
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

func previewSandboxLifecycle(output io.Writer, instance agent37.Instance, action string) error {
	if instance.Status == "deleted" {
		if _, terminal := ttyOutput(output); terminal {
			return printSandboxResult(output, "Preview · sandbox already deleted", instance, "")
		}
		target := summary(instance)
		target["action"] = "none"
		return jsonOutput(output, target, true)
	}
	title := "Preview · stop sandbox"
	switch action {
	case "down":
		title, action = "Preview · delete sandbox and disk", "delete"
	case "sleep":
		title = "Preview · put sandbox to sleep"
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
		if name == "" {
			notice(output, info, "No managed Agent37 sandboxes.")
		} else {
			notice(output, info, "No managed Agent37 sandbox named %s.", name)
		}
		return nil
	}
	result := map[string]string{"status": "absent"}
	if name != "" {
		result["name"] = name
	}
	if dryRun {
		result["action"] = "none"
	}
	return jsonOutput(output, result, dryRun)
}
