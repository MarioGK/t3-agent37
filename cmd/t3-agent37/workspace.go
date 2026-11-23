package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	flag "github.com/spf13/pflag"
)

const (
	workspacePlaybookFile = "workspace.json"
	workspaceDisabledKey  = "workspace_disabled"
	workspaceSetupKey     = "workspace_setup"
	workspaceReadyFile    = "workspace-ready"
)

func requiresWorkspaceSetup(instance agent37.Instance) bool {
	return instance.Metadata[workspaceSetupKey] == "required"
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

func workspaceProofError(playbook *workspacePlaybook, name string, proof setupProof) error {
	if playbook == nil {
		return nil
	}
	var failures []error
	for _, item := range proof.WorkspaceErrors {
		failures = append(failures, checkoutError(item.Status, playbook.Path, name, item.Repo, item.Folder))
	}
	switch proof.ManifestError {
	case "":
	case "too_large":
		failures = append(failures, errors.New("TerraScale repos.txt is too large"))
	case "empty":
		failures = append(failures, errors.New("TerraScale repos.txt is empty"))
	case "invalid", "duplicate", "directory_conflict":
		failures = append(failures, errors.New("TerraScale repos.txt contains an invalid repository entry"))
	default:
		failures = append(failures, fmt.Errorf("could not read the TerraScale repository manifest in %s", name))
	}
	switch proof.ToolsStage {
	case "":
	case "skills", "mise", "orchestrator", "toolchain":
		failures = append(failures, fmt.Errorf("TerraScale %s setup did not complete in %s", proof.ToolsStage, name))
	default:
		failures = append(failures, errors.New("sandbox returned an unknown TerraScale setup result"))
	}
	if proof.ProjectsError != "" {
		failures = append(failures, fmt.Errorf("could not verify T3 project registrations in %s: %s", name, proof.ProjectsError))
	}
	if len(failures) == 0 && !proof.WorkspaceReady {
		failures = append(failures, fmt.Errorf("workspace setup did not complete in %s", name))
	}
	return errors.Join(failures...)
}

func checkoutError(status, workspace, name, repo, folder string) error {
	switch status {
	case "cloned", "existing", "seeded":
		return nil
	case "conflict":
		return fmt.Errorf("%s in %s conflicts with the checkout of %s. Resolve the conflict before retrying", path.Join(workspace, folder), name, repo)
	case "clone_failed":
		return fmt.Errorf("could not clone %s in %s. Check GitHub access and add --github when retrying the credential handoff", repo, name)
	case "fetch_failed":
		return fmt.Errorf("could not refresh %s in %s. Check GitHub access and retry with up %s --github", repo, name, name)
	case "pull_conflict":
		return fmt.Errorf("could not fast-forward %s in %s. Check the upstream branch and local changes", repo, name)
	case "timeout":
		return fmt.Errorf("checking out %s timed out in %s", repo, name)
	case "filesystem_error":
		return fmt.Errorf("could not prepare %s in %s. Check the workspace path, permissions, and disk space", path.Join(workspace, folder), name)
	default:
		return errors.New("sandbox returned an unknown workspace clone status")
	}
}
