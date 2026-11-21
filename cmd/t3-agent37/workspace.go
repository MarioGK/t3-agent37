package main

import (
	"context"
	_ "embed"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	flag "github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
)

const (
	workspacePlaybookFile = "workspace.json"
	workspaceDisabledKey  = "workspace_disabled"
	workspaceSetupKey     = "workspace_setup"
	workspaceReadyFile    = "workspace-ready"
	workspaceReadyPath    = "$HOME/.local/share/t3-agent37/" + workspaceReadyFile
)

func requiresWorkspaceSetup(instance agent37.Instance) bool {
	return instance.Metadata[workspaceSetupKey] == "required"
}

// Instance metadata declares the prerequisite before the guest becomes discoverable.
// The guest marker lets later commands verify completion without the workstation playbook.
func requireWorkspaceReady(ctx context.Context, client *agent37.Client, instance agent37.Instance, name string, completed bool) error {
	if !requiresWorkspaceSetup(instance) {
		return nil
	}
	if completed {
		_, err := client.Exec(ctx, instance.ID, `mkdir -p -m 700 "$HOME/.local/share/t3-agent37" && touch "`+workspaceReadyPath+`"`)
		if err != nil {
			return fmt.Errorf("could not record workspace readiness in %s: %w", name, err)
		}
		return nil
	}
	proof, err := client.ExecJSON[struct{ Ready bool }](ctx, instance.ID,
		`if test -f "`+workspaceReadyPath+`"; then printf '%s\n' '{"ready":true}'; else printf '%s\n' '{"ready":false}'; fi`)
	if err != nil {
		return err
	}
	if !proof.Ready {
		return fmt.Errorf("finish workspace setup with t3-agent37 up %s before linking T3", name)
	}
	return nil
}

type workspacePlaybook struct {
	Path         string                `json:"path"`
	RootRepo     string                `json:"root_repository,omitempty"`
	Repositories []workspaceRepository `json:"repositories"`
}

type workspaceRepository struct {
	Repo      string `json:"repo"`
	Ref       string `json:"ref,omitempty"`
	Directory string `json:"directory,omitempty"`
}

func (repo workspaceRepository) directory() string {
	if repo.Directory != "" {
		return repo.Directory
	}
	return path.Base(repo.Repo)
}

func (playbook workspacePlaybook) terraScale() bool {
	return strings.EqualFold(playbook.RootRepo, "TerraScale/workspace")
}

var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)

//go:embed guest/workspace.py
var guestWorkspace string

//go:embed guest/terrascale.py
var guestTerraScale string

func addWorkspaceFlag(flags *flag.FlagSet) *bool {
	return flags.Bool("workspace", true, "Use the local workspace playbook when configured")
}

func (opts *options) loadWorkspacePlaybook(metadata map[string]string) error {
	startManagedWorkspace := opts.action == "start" && metadata[workspaceSetupKey] == "required"
	if (!opts.up && !startManagedWorkspace) || opts.workspace != nil && !*opts.workspace ||
		opts.workspace == nil && metadata[workspaceDisabledKey] == "true" {
		return nil
	}
	filename := os.Getenv("T3_AGENT37_WORKSPACE_FILE")
	explicit := filename != "" || opts.workspace != nil
	if filename == "" {
		var err error
		filename, err = workstationConfigPath(workspacePlaybookFile)
		if err != nil {
			return err
		}
	}
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not read workspace playbook: %w", err)
	}
	var playbook workspacePlaybook
	if err := json.Unmarshal(data, &playbook, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid workspace playbook %s: %w", filename, err)
	}
	if err := playbook.validate(); err != nil {
		return fmt.Errorf("invalid workspace playbook %s: %w", filename, err)
	}
	opts.playbook = &playbook
	return nil
}

func (playbook workspacePlaybook) validate() error {
	relative, homeRelative := strings.CutPrefix(playbook.Path, "~/")
	if !homeRelative || !validWorkspacePath(relative) {
		return errors.New("path must name a directory below the sandbox home, such as ~/Projects/Example")
	}
	if playbook.RootRepo != "" && !validWorkspaceRepository(playbook.RootRepo) {
		return errors.New("root_repository requires a GitHub owner/name")
	}
	if len(playbook.Repositories) == 0 && playbook.RootRepo == "" {
		return errors.New("repositories must contain at least one GitHub repository")
	}
	directories := make(map[string]struct{}, len(playbook.Repositories))
	for i, repo := range playbook.Repositories {
		if !validWorkspaceRepository(repo.Repo) {
			return fmt.Errorf("repository %d requires a GitHub owner/name", i+1)
		}
		directory := repo.directory()
		if !validWorkspacePath(directory) || strings.Contains(directory, "/") {
			return fmt.Errorf("repository %d requires a single directory name", i+1)
		}
		if _, duplicate := directories[directory]; duplicate {
			return fmt.Errorf("repository %d reuses the directory %q", i+1, directory)
		}
		directories[directory] = struct{}{}
		if strings.HasPrefix(repo.Ref, "-") || strings.IndexFunc(repo.Ref, unicode.IsControl) >= 0 {
			return fmt.Errorf("repository %d has an invalid ref", i+1)
		}
	}
	return nil
}

func validWorkspaceRepository(repo string) bool {
	return githubRepositoryPattern.MatchString(repo) && validWorkspacePath(path.Base(repo))
}

func validWorkspacePath(value string) bool {
	return value != "." && fs.ValidPath(value) && !strings.Contains(value, "\\") &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func (playbook *workspacePlaybook) previewDetails() []detail {
	if playbook == nil {
		return nil
	}
	repositories := make([]string, 0, len(playbook.Repositories))
	for _, repo := range playbook.Repositories {
		name := repo.Repo
		if repo.Ref != "" {
			name += "@" + repo.Ref
		}
		if repo.Directory != "" {
			name += " → " + repo.Directory
		}
		repositories = append(repositories, name)
	}
	fields := []detail{{"Workspace", playbook.Path}}
	if playbook.RootRepo != "" {
		fields = append(fields, detail{"Workspace repository", playbook.RootRepo})
	}
	if len(repositories) != 0 {
		fields = append(fields, detail{"GitHub repositories", strings.Join(repositories, ", ")})
	}
	return fields
}

func previewCreatePayload(opts options) map[string]any {
	payload := createPayload(opts, nil)
	if opts.playbook != nil {
		payload["workspace"] = opts.playbook
	}
	return payload
}

const workspaceCloneParallelism = 4

func (playbook *workspacePlaybook) clone(ctx context.Context, client *agent37.Client, id, name string, diagnostic io.Writer) error {
	started := time.Now()
	progress, _ := diagnostic.(*launchProgress)
	if progress != nil && progress.total == 1 {
		progress.setPhase("Cloning workspace repositories")
	}
	baseCommand := "python3 -B -c " + shellQuote(guestWorkspace) + " " + shellQuote(playbook.Path)
	if playbook.RootRepo != "" {
		if err := cloneWorkspaceRepository(ctx, client, id, name, baseCommand, playbook.Path, workspaceRepository{Repo: playbook.RootRepo}, ""); err != nil {
			return err
		}
	}
	if playbook.terraScale() {
		command := "git -C \"$HOME\"/" + shellQuote(strings.TrimPrefix(playbook.Path, "~/")) + " show HEAD:repos.txt"
		manifest, err := client.Exec(ctx, id, command)
		if err != nil {
			return fmt.Errorf("could not read the TerraScale repository manifest in %s: %w", name, err)
		}
		if err := playbook.applyTerraScaleManifest(manifest); err != nil {
			return err
		}
	}
	notice(diagnostic, info, "%s Cloning %d workspace repositories", name, len(playbook.Repositories))
	var clones errgroup.Group
	clones.SetLimit(workspaceCloneParallelism)
	for _, repo := range playbook.Repositories {
		clones.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return cloneWorkspaceRepository(ctx, client, id, name, baseCommand, playbook.Path, repo, repo.directory())
		})
	}
	if err := clones.Wait(); err != nil {
		return err
	}
	notice(diagnostic, success, "%s Checkouts ready at %s", name, playbook.Path)
	launchTiming(diagnostic, name, "Repository checkout", started)
	return nil
}

func (playbook *workspacePlaybook) applyTerraScaleManifest(manifest string) error {
	if len(manifest) > 16<<10 {
		return errors.New("TerraScale repos.txt is too large")
	}
	// Copy the slice before adding repos. Batch launches share the original playbook.
	repositories := append([]workspaceRepository(nil), playbook.Repositories...)
	seen := make(map[string]string, len(repositories))
	for _, repo := range repositories {
		seen[strings.ToLower(repo.Repo)] = repo.directory()
	}
	entries := strings.Fields(manifest)
	if len(entries) == 0 {
		return errors.New("TerraScale repos.txt is empty")
	}
	for _, entry := range entries {
		repo := "TerraScale/" + entry
		if !validWorkspaceRepository(repo) {
			return errors.New("TerraScale repos.txt contains an invalid repository name")
		}
		key := strings.ToLower(repo)
		if directory, exists := seen[key]; !exists {
			repositories = append(repositories, workspaceRepository{Repo: repo})
			seen[key] = entry
		} else if directory != entry {
			return fmt.Errorf("TerraScale repos.txt requires %s at %s", repo, entry)
		}
	}
	playbook.Repositories = repositories
	return playbook.validate()
}

func (playbook workspacePlaybook) installTerraScaleTools(ctx context.Context, client *agent37.Client, id, name string, diagnostic io.Writer) error {
	started := time.Now()
	notice(diagnostic, info, "%s Preparing TerraScale skills and tools", name)
	command := "python3 -B -c " + shellQuote(guestTerraScale) + " " + shellQuote(playbook.Path)
	result, err := client.ExecJSON[struct{ Stage string }](ctx, id, command)
	if err != nil {
		return fmt.Errorf("could not prepare TerraScale tools in %s: %w", name, err)
	}
	switch result.Stage {
	case "ready":
		launchTiming(diagnostic, name, "TerraScale skills and tools", started)
		notice(diagnostic, success, "%s TerraScale skills and tools ready", name)
		return nil
	case "skills", "mise", "orchestrator", "toolchain":
		return fmt.Errorf("TerraScale %s setup did not complete in %s", result.Stage, name)
	default:
		return errors.New("sandbox returned an unknown TerraScale setup result")
	}
}

func cloneWorkspaceRepository(ctx context.Context, client *agent37.Client, id, name, baseCommand, workspace string, repo workspaceRepository, folder string) error {
	command := baseCommand + " " + shellQuote(repo.Repo) + " " + shellQuote(folder) + " " + shellQuote(repo.Ref)
	result, err := client.ExecJSON[struct{ Status string }](ctx, id, command)
	if err != nil {
		return fmt.Errorf("could not clone %s in %s: %w", repo.Repo, name, err)
	}
	switch result.Status {
	case "cloned", "existing":
		return nil
	case "conflict":
		return fmt.Errorf("%s in %s conflicts with the checkout of %s. Resolve the conflict before retrying", path.Join(workspace, folder), name, repo.Repo)
	case "clone_failed":
		return fmt.Errorf("could not clone %s in %s. Check GitHub access and add --github when retrying the credential handoff", repo.Repo, name)
	case "fetch_failed":
		return fmt.Errorf("could not refresh %s in %s. Check GitHub access and retry with up %s --github", repo.Repo, name, name)
	case "pull_conflict":
		return fmt.Errorf("could not fast-forward %s in %s. Check the upstream branch and local changes", repo.Repo, name)
	case "timeout":
		return fmt.Errorf("checking out %s timed out in %s", repo.Repo, name)
	case "filesystem_error":
		return fmt.Errorf("could not prepare %s in %s. Check the workspace path, permissions, and disk space", path.Join(workspace, folder), name)
	default:
		return errors.New("sandbox returned an unknown workspace clone status")
	}
}

func (playbook workspacePlaybook) register(ctx context.Context, client *agent37.Client, id, name string, diagnostic io.Writer) error {
	started := time.Now()
	projects := make(map[string]string, len(playbook.Repositories)+1)
	if playbook.RootRepo != "" {
		projects[playbook.Path] = playbook.RootRepo
	}
	for _, repo := range playbook.Repositories {
		projects[path.Join(playbook.Path, repo.directory())] = repo.Repo
	}
	payload, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	if progress, ok := diagnostic.(*launchProgress); ok && progress.total == 1 {
		progress.setPhase("Registering T3 projects")
	}
	notice(diagnostic, info, "%s Registering %d projects in T3 Code", name, len(projects))
	if _, err := client.Exec(ctx, id, remote+" register-projects "+shellQuote(string(payload))); err != nil {
		return fmt.Errorf("could not verify T3 project registrations in %s: %w", name, err)
	}
	notice(diagnostic, success, "%s T3 projects verified", name)
	launchTiming(diagnostic, name, "T3 project registration", started)
	return nil
}
