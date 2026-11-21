package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/gofrs/flock"
)

const deleteParallelism = 6

type bulkDeletion struct {
	target recordedTarget
	err    error
}

func runDeleteAll(ctx context.Context, client *agent37.Client, dryRun bool, output, diagnostic io.Writer) (err error) {
	state, err := statePath()
	if err != nil {
		return err
	}
	if !dryRun {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return err
		}
		lock := flock.New(filepath.Join(state, "deletion.lock"))
		locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("could not lock the deletion targets")
		}
		defer func() { err = errors.Join(err, lock.Unlock()) }()
	}
	results, err := snapshotDeletions(ctx, client, state)
	if err != nil {
		return err
	}
	if dryRun {
		for i := range results {
			target := &results[i].target
			if target.original == nil || target.receipt.Status == "deleted" {
				continue
			}
			instance, err := readDownTarget(ctx, client, target.receipt)
			if err != nil {
				return err
			}
			target.receipt.Status = instance.Status
		}
	}
	if dryRun || len(results) == 0 {
		return showBulkDeletions(output, results, dryRun)
	}
	progress := startSandboxProgress(diagnostic, len(results), "Deleting Agent37 sandboxes", true)
	defer progress.close()
	var workers sync.WaitGroup
	parallel := min(deleteParallelism, len(results))
	for worker := range parallel {
		workers.Go(func() {
			for i := worker; i < len(results); i += parallel {
				item := &results[i]
				item.err = deleteBulkTarget(ctx, client, state, &item.target)
				if item.err != nil {
					progress.provisioned(false)
					continue
				}
				progress.allocated()
				item.target.receipt.cleanTailnet(ctx, progress)
			}
		})
	}
	workers.Wait()

	var failures []error
	for _, item := range results {
		if item.err != nil {
			failures = append(failures, fmt.Errorf("%s (%s): %w", item.target.receipt.Name, item.target.receipt.ID, item.err))
		}
	}
	if ctx.Err() == nil {
		finishBulkCleanup(ctx, client, results, progress)
	}
	progress.close()
	if err := showBulkDeletions(output, results, false); err != nil {
		return errors.Join(err, errors.Join(failures...), ctx.Err())
	}
	for i := range results {
		item := &results[i]
		if item.err == nil && ctx.Err() == nil {
			item.target.receipt.ResultPending = false
			failures = append(failures, item.target.save(ctx))
		}
	}
	return errors.Join(errors.Join(failures...), ctx.Err())
}

func snapshotDeletions(ctx context.Context, client *agent37.Client, state string) ([]bulkDeletion, error) {
	instances, err := client.List(ctx)
	if err != nil {
		return nil, err
	}
	scope := keyScope(client)
	dir := filepath.Join(state, "delete-all", scope)
	byID := make(map[string]recordedTarget)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || agent37.ValidateID(id) != nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		receipt, original, err := readDownReceipt(path)
		if err != nil {
			return nil, err
		}
		if receipt.ID != id || receipt.KeyScope != scope {
			return nil, fmt.Errorf("invalid bulk deletion receipt at %s", path)
		}
		byID[id] = recordedTarget{receipt: receipt, original: original, path: path}
	}
	for _, instance := range instances {
		if !isManaged(instance) {
			continue
		}
		receipt := newDownReceipt(instance, scope)
		if agent37.ValidateID(receipt.ID) != nil || !namePattern.MatchString(receipt.Name) {
			return nil, errors.New("Agent37 returned an invalid managed sandbox identity")
		}
		if previous, ok := byID[receipt.ID]; ok {
			if previous.receipt.Name != receipt.Name {
				return nil, fmt.Errorf("the saved sandbox %s changed its environment name", receipt.ID)
			}
			continue
		}
		byID[receipt.ID] = recordedTarget{receipt: receipt, path: filepath.Join(dir, receipt.ID+".json")}
	}
	results := make([]bulkDeletion, 0, len(byID))
	for _, target := range byID {
		if !target.receipt.complete() {
			results = append(results, bulkDeletion{target: target})
		}
	}
	slices.SortFunc(results, func(a, b bulkDeletion) int {
		return cmp.Or(strings.Compare(a.target.receipt.Name, b.target.receipt.Name), strings.Compare(a.target.receipt.ID, b.target.receipt.ID))
	})
	return results, nil
}

func deleteBulkTarget(ctx context.Context, client *agent37.Client, state string, target *recordedTarget) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	opts := options{name: target.receipt.Name}
	lock, err := lockSandbox(ctx, client, state, &opts)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	return target.deleteCloud(ctx, client, state)
}

func finishBulkCleanup(ctx context.Context, client *agent37.Client, results []bulkDeletion, progress *launchProgress) {
	links := make(map[string]string)
	for _, item := range results {
		if item.err == nil && item.target.receipt.CleanupPending {
			links[item.target.receipt.ID] = item.target.receipt.Name
		}
	}
	linksPending := false
	if len(links) > 0 {
		progress.setPhase("Cleaning up T3 links")
		if err := unlinkSandboxes(ctx, client, links, progress); err != nil {
			linksPending = true
			notice(progress, warning, "T3 cleanup pending. Retry delete --all or run t3-agent37 sync. %v", err)
		}
		if _, err := desktopLauncher(ctx); err == nil {
			notice(progress, info, "Remove any saved entries for deleted sandboxes under T3 Code → Settings → Connections.")
		}
	}
	for i := range results {
		item := &results[i]
		if item.err != nil {
			continue
		}
		item.target.receipt.CleanupPending = item.target.receipt.CleanupPending && linksPending
		if !item.target.receipt.CleanupPending && !item.target.receipt.TailnetPending {
			progress.provisioned(true)
		}
	}
}

func showBulkDeletions(output io.Writer, results []bulkDeletion, dryRun bool) error {
	rows := make([]map[string]any, 0, len(results))
	details := make([]detail, 0, len(results))
	for _, item := range results {
		receipt := item.target.receipt
		row := summary(receipt.instance())
		status := receipt.Status
		if dryRun {
			status = "delete"
			if receipt.Status == "deleted" {
				status = "cleanup"
			}
			row["action"] = status
		} else {
			row["cleanup_pending"] = receipt.CleanupPending
			row["tailscale_cleanup_pending"] = receipt.TailnetPending
			if item.err != nil {
				row["error"] = item.err.Error()
				status = "Failed"
			} else if receipt.CleanupPending || receipt.TailnetPending {
				status = "Deleted · cleanup pending"
			}
		}
		rows = append(rows, row)
		details = append(details, detail{receipt.Name + " (" + receipt.ID + ")", status})
	}
	if _, terminal := ttyOutput(output); terminal {
		if len(results) == 0 {
			return printDetails(output, "Delete sandboxes", detail{"Targets", "No managed sandboxes"})
		}
		if dryRun {
			return printDetails(output, "Preview · delete all managed sandboxes", details...)
		}
		return printDetails(output, "Sandbox deletion results", details...)
	}
	return jsonOutput(output, rows, true)
}
