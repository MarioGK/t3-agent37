package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/MarioGK/t3-agent37/internal/peers"
	"golang.org/x/sync/errgroup"
)

const maxBatchSize = 100
const batchParallelism = 16
const maxBatchParallelism = 32

func batchCountArgument(value string) bool {
	value = strings.TrimPrefix(value, "-")
	return value != "" && strings.Trim(value, "0123456789") == ""
}

func previewBatch(ctx context.Context, opts options, state string, output io.Writer) error {
	names, err := previewSandboxNames(ctx, state, opts.namePrefix, opts.batchSize)
	if err != nil {
		return err
	}
	if _, terminal := ttyOutput(output); terminal {
		fields := []detail{
			{"Sandboxes", strings.Join(names, ", ")}, {"Template", opts.template}, {"Provider", providerLabel(opts.provider)},
			{"Concurrent", fmt.Sprint(min(opts.parallelism, len(names)))},
			{"Per sandbox", fmt.Sprintf("%s · %d min auto-sleep", opts.size, sandboxIdle/time.Minute)},
			{"Batch RAM", fmt.Sprintf("%d GB", len(names)*opts.size.RAM)}, {"Network", "Private Tailscale"},
		}
		return printDetails(output, fmt.Sprintf("Preview · create %d sandboxes", len(names)), append(fields, opts.playbook.previewDetails()...)...)
	}
	plans := make([]map[string]any, 0, len(names))
	for _, name := range names {
		opts.name = name
		plans = append(plans, previewCreatePayload(opts))
	}
	return jsonOutput(output, plans, true)
}

type batchSandbox struct {
	name     string
	instance agent37.Instance
	proof    setupProof
	err      error
}

func runBatch(ctx context.Context, client *agent37.Client, verifyEnrollment func(context.Context) (*peers.Enrollment, error), opts options, state string, output io.Writer, progress *launchProgress) error {
	var launcher []string
	if !opts.noDesktop {
		var err error
		launcher, err = desktopLauncher(ctx)
		if err != nil {
			return fmt.Errorf("desktop confirmation is unavailable. Use --no-desktop for headless provisioning: %w", err)
		}
	}
	if _, err := peers.Configured(); err != nil {
		return err
	}
	if opts.provider == "cliproxy" {
		if _, err := cliproxyEnv(); err != nil {
			return err
		}
	}
	progress.setPhase("Checking OAuth and sandbox inventory")
	instances, enrollment, err := batchPreflight(ctx, client, verifyEnrollment)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	reserved, err := reserveSandboxNames(ctx, state, opts.namePrefix, instances, opts.batchSize)
	if err != nil {
		return err
	}
	parallel := min(opts.parallelism, len(reserved))
	progress.preflightDone("Creating and setting up sandboxes")
	notice(progress, info, "Provisioning %d Agent37 sandboxes, up to %d at a time…", len(reserved), parallel)
	var ready int
	results := provisionAndPairBatch(len(reserved), parallel, func(i int) batchSandbox {
		item := provisionBatchSandbox(ctx, client, enrollment, opts, state, reserved[i], progress)
		progress.provisioned(item.err == nil)
		if item.err == nil {
			notice(progress, success, "%s is ready on Tailscale.", item.name)
		}
		return item
	}, func(item *batchSandbox) {
		ready++
		if launcher == nil || ctx.Err() != nil {
			return
		}
		progress.setPhase("Waiting for T3 Code · " + item.name)
		proof, err := pairSandbox(ctx, client, item.instance, item.proof, state, item.name, launcher, progress, nil)
		progress.pairedDesktop(err == nil)
		if err != nil {
			item.err = fmt.Errorf("run t3-agent37 link %s to finish pairing: %w", item.name, err)
		} else {
			item.proof = proof
		}
		progress.setPhase("Creating and setting up sandboxes")
	})
	var failures []error
	for _, item := range results {
		if item.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", item.name, item.err))
		}
	}
	meshPending := ready > 0
	if ready > 0 && ctx.Err() == nil {
		started := time.Now()
		progress.setPhase("Syncing T3 environments")
		notice(progress, info, "Syncing T3 environments…")
		if err := syncSandboxLinks(ctx, client, false, io.Discard, progress); err != nil {
			notice(progress, warning, "T3 mesh has pending links. Run t3-agent37 sync. %v", err)
		} else {
			meshPending = false
			launchTiming(progress, "batch", "Peer sync", started)
		}
		progress.meshFinished(!meshPending)
	}
	progress.close()
	return errors.Join(errors.Join(failures...), ctx.Err(), showBatchResult(output, results, opts.noDesktop, meshPending))
}

func batchPreflight(ctx context.Context, client *agent37.Client, verifyEnrollment func(context.Context) (*peers.Enrollment, error)) ([]agent37.Instance, *peers.Enrollment, error) {
	var enrollment *peers.Enrollment
	var instances []agent37.Instance
	checks, checked := errgroup.WithContext(ctx)
	checks.Go(func() error {
		var err error
		enrollment, err = verifyEnrollment(checked)
		return err
	})
	checks.Go(func() error {
		var err error
		instances, err = client.List(checked)
		return err
	})
	err := checks.Wait()
	return instances, enrollment, err
}

func provisionAndPairBatch(count, parallel int, provision func(int) batchSandbox, pair func(*batchSandbox)) []batchSandbox {
	type completedSandbox struct {
		index int
		item  batchSandbox
	}
	jobs := make(chan int, count)
	for i := range count {
		jobs <- i
	}
	close(jobs)
	// A desktop approval may block the consumer. Provisioning workers can
	// publish their results and take the next job while the user approves.
	completed := make(chan completedSandbox, count)
	var workers sync.WaitGroup
	for range parallel {
		workers.Go(func() {
			for i := range jobs {
				completed <- completedSandbox{index: i, item: provision(i)}
			}
		})
	}
	go func() {
		workers.Wait()
		close(completed)
	}()
	results := make([]batchSandbox, count)
	for result := range completed {
		if result.item.err == nil {
			pair(&result.item)
		}
		results[result.index] = result.item
	}
	return results
}

func provisionBatchSandbox(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, opts options, state string, entry sandboxReservation, progress *launchProgress) (item batchSandbox) {
	defer func() {
		if entry.lock != nil {
			item.err = errors.Join(item.err, entry.lock.Unlock())
		}
	}()
	for {
		opts.name, item.name = entry.name, entry.name
		if item.err = ctx.Err(); item.err != nil {
			return item
		}
		item.instance, item.err = getOrCreate(ctx, client, opts, state, progress)
		if !errors.Is(item.err, errSandboxNameTaken) {
			break
		}
		if item.err = entry.lock.Unlock(); item.err != nil {
			return item
		}
		entry.lock, item.err = lockSandbox(ctx, client, state, &opts)
		if item.err != nil {
			return item
		}
		entry.name = opts.name
	}
	if item.err != nil {
		if apiError, ok := errors.AsType[*agent37.Error](item.err); ok && apiError.Code == "instance_limit_reached" {
			item.err = fmt.Errorf("Agent37 workspace memory limit reached. %d new sandboxes need %d GB of running memory: %w", opts.batchSize, opts.batchSize*opts.size.RAM, apiError)
		}
		return item
	}
	progress.allocated()
	item.proof, item.err = prepareCreated(ctx, client, enrollment, item.instance, opts, progress)
	if item.err != nil {
		item.err = fmt.Errorf("sandbox exists. Run t3-agent37 up %s to retry setup: %w", opts.name, item.err)
		return item
	}
	item.instance.Status = "running"
	return item
}

func showBatchResult(output io.Writer, results []batchSandbox, noDesktop, meshPending bool) error {
	rows := make([]map[string]any, 0, len(results))
	details := make([]detail, 0, len(results)+1)
	for _, item := range results {
		row := summary(item.instance)
		row["name"] = item.name
		desktop := "pending"
		if noDesktop {
			desktop = "skipped"
		} else if item.proof.DesktopConfirmed {
			desktop = "connected"
		}
		row["desktop"] = desktop
		value := item.proof.TailnetURL
		if value != "" {
			row["t3_url"] = value
			row["environment_id"] = item.proof.EnvironmentID
		} else if item.instance.ID != "" {
			value = "Setup pending"
		} else {
			value = "Creation unconfirmed"
			row["status"] = "unconfirmed"
		}
		if item.err != nil {
			row["error"] = item.err.Error()
		}
		details = append(details, detail{item.name, value})
		rows = append(rows, row)
	}
	if _, terminal := ttyOutput(output); terminal {
		if meshPending {
			details = append(details, detail{"T3 mesh", "Pending links"})
		}
		return printDetails(output, "Sandbox batch", details...)
	}
	return jsonOutput(output, map[string]any{"sandboxes": rows, "mesh_pending": meshPending}, true)
}
