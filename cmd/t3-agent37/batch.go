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

type launchContext struct {
	instances  []agent37.Instance
	enrollment *peers.Enrollment
	configured []peers.Peer
	launcher   []string
	opts       options
}

// prepareLaunch runs credential checks, sandbox inventory, desktop launcher
// detection, CLIProxy catalog fetch, and GitHub handoff lookup concurrently
// before allocating any sandbox.
func prepareLaunch(ctx context.Context, client *agent37.Client, verifyEnrollment func(context.Context) (*peers.Enrollment, error), opts options, diagnostic io.Writer) (launchContext, error) {
	started := time.Now()
	out := launchContext{opts: opts}
	var err error
	if opts.generatedName && !opts.noDesktop {
		out.launcher, err = desktopLauncher(ctx)
		if err != nil {
			return out, fmt.Errorf("desktop confirmation is unavailable. Use --no-desktop for headless provisioning: %w", err)
		}
	}
	if opts.tailscale {
		out.configured, err = peers.Configured()
		if err != nil {
			return out, err
		}
	}
	var proxyEnv map[string]string
	if opts.provider == "cliproxy" {
		proxyEnv, err = cliproxyEnv()
		if err != nil {
			return out, err
		}
	}

	var ghHandoff githubHandoff
	var ghErr error
	var ghDone bool
	var catalog cliproxyCatalog
	group, gctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		out.enrollment, err = verifyEnrollment(gctx)
		return err
	})
	group.Go(func() error {
		var err error
		out.instances, err = client.List(gctx)
		return err
	})
	if opts.githubAccess {
		group.Go(func() error {
			ghHandoff, ghErr = readGitHubHandoff(gctx)
			ghDone = true
			if opts.playbook != nil && opts.playbook.terraScale() {
				if ghErr != nil {
					return fmt.Errorf("could not prepare GitHub access for the TerraScale workspace: %w", ghErr)
				}
				if ghHandoff.Token == "" {
					return errors.New("sign in with gh auth login --hostname github.com before creating the TerraScale workspace")
				}
			}
			return nil
		})
	}
	if opts.provider == "cliproxy" {
		group.Go(func() error {
			var err error
			catalog, err = fetchCLIProxyCatalog(gctx, proxyEnv)
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return out, err
	}
	if ghDone {
		out.opts.preparedGitHub = func() (githubHandoff, error) { return ghHandoff, ghErr }
	}
	if len(catalog.Models) != 0 {
		out.opts.cliproxyModels = catalog.Models
		out.opts.cliproxyModel = catalog.DefaultModel
		out.opts.cliproxyHasSol = catalog.HasSol
	}
	launchTiming(diagnostic, "preflight", "Preflight", started)
	return out, nil
}

type batchSandbox struct {
	name     string
	instance agent37.Instance
	proof    setupProof
	err      error
}

func runBatch(ctx context.Context, client *agent37.Client, verifyEnrollment func(context.Context) (*peers.Enrollment, error), opts options, state string, output io.Writer, progress *launchProgress) error {
	progress.setPhase("Checking credentials and sandbox inventory")
	launch, err := prepareLaunch(ctx, client, verifyEnrollment, opts, progress)
	if err != nil {
		return err
	}
	opts = launch.opts
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	reserved, err := reserveSandboxNames(ctx, state, opts.namePrefix, launch.instances, opts.batchSize)
	if err != nil {
		return err
	}
	configured := peers.PrepareConfiguredPeers(ctx, launch.configured)
	parallel := min(opts.parallelism, len(reserved))
	progress.preflightDone("Creating and setting up sandboxes")
	notice(progress, info, "Provisioning %d Agent37 sandboxes, up to %d at a time…", len(reserved), parallel)
	gate := make(desktopPromptGate, 1)
	results := provisionAndPairBatch(len(reserved), parallel, parallel, func(i int) batchSandbox {
		item := provisionBatchSandbox(ctx, client, launch.enrollment, opts, state, reserved[i], progress)
		progress.provisioned(item.err == nil)
		if item.err == nil {
			notice(progress, success, "%s is ready on Tailscale.", item.name)
		}
		return item
	}, func(item *batchSandbox) {
		if launch.launcher == nil {
			return
		}
		if item.err = ctx.Err(); item.err != nil {
			return
		}
		progress.setPhase("Pairing T3 Code as sandboxes become ready")
		proof, err := pairSandboxWithPromptGate(ctx, client, item.instance, item.proof, state, item.name, launch.launcher, progress, gate, nil)
		progress.pairedDesktop(err == nil)
		if ctx.Err() != nil && err != nil {
			item.err = ctx.Err()
		} else if err != nil {
			item.err = fmt.Errorf("run t3-agent37 link %s to finish pairing: %w", item.name, err)
		} else {
			item.proof = proof
		}
	})
	var failures []error
	meshErr := finishBatchMesh(ctx, client, configured, results, progress)
	for _, item := range results {
		if item.err != nil && item.err != ctx.Err() {
			failures = append(failures, fmt.Errorf("%s: %w", item.name, item.err))
		}
	}
	progress.close()
	return errors.Join(errors.Join(failures...), meshErr, ctx.Err(), showBatchResult(output, results, opts.noDesktop, meshErr != nil))
}

func finishBatchMesh(ctx context.Context, client *agent37.Client, configured []peers.Peer, results []batchSandbox, progress *launchProgress) error {
	ready := false
	for _, item := range results {
		ready = ready || item.proof.TailnetURL != ""
	}
	if !ready {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	started := time.Now()
	progress.setPhase("Syncing T3 environments")
	notice(progress, info, "Syncing T3 environments…")
	if err := syncSandboxLinksWithDiscovery(ctx, client, configured, false, io.Discard, progress,
		func(ctx context.Context, existing []agent37.Instance) ([]peers.MeshNode, error) {
			return batchMeshNodes(ctx, client, existing, results)
		}); err != nil {
		notice(progress, warning, "T3 mesh has pending links. Run t3-agent37 sync. %v", err)
		progress.meshFinished(false)
		return fmt.Errorf("T3 mesh is incomplete: %w", err)
	}
	progress.meshFinished(true)
	launchTiming(progress, "batch", "Peer sync", started)
	return nil
}

func provisionAndPairBatch(count, parallel, pairingWorkers int, provision func(int) batchSandbox, pair func(*batchSandbox)) []batchSandbox {
	type completedSandbox struct {
		index int
		item  batchSandbox
	}
	jobs := make(chan int, count)
	for i := range count {
		jobs <- i
	}
	close(jobs)
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
	var pairing sync.WaitGroup
	for range pairingWorkers {
		pairing.Go(func() {
			for result := range completed {
				if result.item.err == nil {
					pair(&result.item)
				}
				results[result.index] = result.item
			}
		})
	}
	pairing.Wait()
	return results
}

func provisionBatchSandbox(ctx context.Context, client *agent37.Client, enrollment *peers.Enrollment, opts options, state string, entry sandboxReservation, progress *launchProgress) (item batchSandbox) {
	defer func() {
		if entry.lock != nil {
			if err := entry.lock.Unlock(); err != nil {
				item.err = errors.Join(item.err, err)
			}
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
		entry.lock, item.err = lockSandbox(ctx, client, state, &opts, nil)
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
		if ctx.Err() != nil {
			item.err = ctx.Err()
		} else {
			item.err = fmt.Errorf("sandbox exists. Run t3-agent37 up %s to retry setup: %w", opts.name, item.err)
		}
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
		} else if errors.Is(item.err, context.Canceled) && !errors.Is(item.err, errCreateUnconfirmed) {
			value = "Canceled before creation"
			row["status"] = "canceled"
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
