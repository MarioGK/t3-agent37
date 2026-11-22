package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/gofrs/flock"
	"golang.org/x/sync/singleflight"
)

const orchestratorRepo = "https://github.com/TerraScale/ts-orchestrator.git"
const maxOrchestratorSize = 200 << 20

var orchestratorRevision = regexp.MustCompile("^[a-f0-9]{40}$")
var orchestratorGoVersion = regexp.MustCompile("^go[0-9]+\\.[0-9]+\\.[0-9]+$")

type orchestratorTarget struct {
	Revision  string `json:"revision"`
	Arch      string `json:"arch"`
	GoVersion string `json:"go_version"`
}

func (target orchestratorTarget) valid() bool {
	return orchestratorRevision.MatchString(target.Revision) &&
		orchestratorGoVersion.MatchString(target.GoVersion) &&
		(target.Arch == "amd64" || target.Arch == "arm64")
}

func (target orchestratorTarget) matches(info *buildinfo.BuildInfo) bool {
	if info.GoVersion != target.GoVersion {
		return false
	}
	settings := make(map[string]string, len(info.Settings))
	for _, item := range info.Settings {
		settings[item.Key] = item.Value
	}
	return settings["GOOS"] == "linux" && settings["GOARCH"] == target.Arch &&
		settings["vcs.revision"] == target.Revision && settings["vcs.modified"] == "false"
}

type sharedOrchestrator struct {
	cache, git, goTool string
	builds             singleflight.Group
}

type orchestratorBundle struct {
	body   []byte
	digest string
}

func newSharedOrchestrator() *sharedOrchestrator {
	cache, cacheErr := os.UserCacheDir()
	git, gitErr := exec.LookPath("git")
	goTool, goErr := exec.LookPath("go")
	if cacheErr != nil || gitErr != nil || goErr != nil {
		return nil
	}
	return &sharedOrchestrator{cache: filepath.Join(cache, "t3-agent37", "ts-orch"), git: git, goTool: goTool}
}

func (shared *sharedOrchestrator) bundle(ctx context.Context, target orchestratorTarget) (orchestratorBundle, error) {
	if !target.valid() {
		return orchestratorBundle{}, errors.New("sandbox returned invalid orchestrator build metadata")
	}
	key := target.GoVersion + "-" + target.Arch + "-" + target.Revision
	result, err, _ := shared.builds.Do(key, func() (any, error) {
		return shared.buildBundle(ctx, key, target)
	})
	if err != nil {
		return orchestratorBundle{}, err
	}
	return result.(orchestratorBundle), nil
}

func (shared *sharedOrchestrator) buildBundle(ctx context.Context, key string, target orchestratorTarget) (orchestratorBundle, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := os.MkdirAll(shared.cache, 0o700); err != nil {
		return orchestratorBundle{}, err
	}
	lock := flock.New(filepath.Join(shared.cache, key+".lock"))
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil || !locked {
		return orchestratorBundle{}, errors.New("could not lock the orchestrator build cache")
	}
	defer lock.Unlock()
	binary := filepath.Join(shared.cache, key)
	info, err := buildinfo.ReadFile(binary)
	if err != nil || !target.matches(info) {
		if err := shared.build(ctx, target, binary); err != nil {
			return orchestratorBundle{}, err
		}
	}
	file, err := os.Open(binary)
	if err != nil {
		return orchestratorBundle{}, err
	}
	defer file.Close()
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return orchestratorBundle{}, err
	}
	digest := sha256.New()
	copied, err := io.Copy(io.MultiWriter(writer, digest), io.LimitReader(file, maxOrchestratorSize+1))
	closeErr := writer.Close()
	if err != nil || closeErr != nil || copied == 0 || copied > maxOrchestratorSize || compressed.Len() > 90<<20 {
		return orchestratorBundle{}, errors.New("could not encode the shared orchestrator")
	}
	return orchestratorBundle{body: compressed.Bytes(), digest: hex.EncodeToString(digest.Sum(nil))}, nil
}

func (shared *sharedOrchestrator) build(ctx context.Context, target orchestratorTarget, binary string) error {
	check := exec.CommandContext(ctx, shared.goTool, "env", "GOVERSION")
	check.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	version, err := check.Output()
	if err != nil || strings.TrimSpace(string(version)) != target.GoVersion {
		return errors.New("the workstation Go version does not match the sandbox")
	}
	stage, err := os.MkdirTemp(shared.cache, ".build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_INDEX_FILE", "GIT_CONFIG", "GOFLAGS":
			return true
		}
		return false
	})
	git := func(args ...string) error {
		command := exec.CommandContext(ctx, shared.git, args...)
		command.Dir = stage
		command.Env = append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_COUNT=0", "GIT_TERMINAL_PROMPT=0")
		return command.Run()
	}
	if err := git("init", "--quiet"); err != nil {
		return errors.New("could not initialize the orchestrator checkout")
	}
	if err := git("fetch", "--quiet", "--depth=1", "--no-tags", "--", orchestratorRepo, target.Revision); err != nil {
		return errors.New("could not fetch the orchestrator revision")
	}
	if err := git("checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return errors.New("could not check out the orchestrator revision")
	}
	artifact := filepath.Join(stage, "ts-orch")
	command := exec.CommandContext(ctx, shared.goTool, "build", "-trimpath", "-buildvcs=true", "-ldflags=-s -w",
		"-o", artifact, "./cmd/terrascale-orchestrator")
	command.Dir = stage
	command.Env = append(env, "GOOS=linux", "GOARCH="+target.Arch, "CGO_ENABLED=0", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local")
	if err := command.Run(); err != nil {
		return errors.New("could not build the orchestrator on the workstation")
	}
	info, err := buildinfo.ReadFile(artifact)
	if err != nil || !target.matches(info) {
		return errors.New("the workstation build does not match the sandbox revision")
	}
	return os.Rename(artifact, binary)
}

func (shared *sharedOrchestrator) install(ctx context.Context, client *agent37.Client, id, command string, target orchestratorTarget) error {
	bundle, err := shared.bundle(ctx, target)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	filename := "ts-orch-" + hex.EncodeToString(nonce) + ".gz"
	if err = uploadGuestFile(ctx, func() error {
		return client.UploadOrchestrator(ctx, id, filename, bundle.body)
	}); err != nil {
		return err
	}
	result, err := client.ExecJSON[struct{ Stage string }](ctx, id, command+" install-shared "+filename+" "+bundle.digest)
	if err != nil {
		return err
	}
	if result.Stage != "ready" {
		return errors.New("sandbox did not verify the shared orchestrator")
	}
	return nil
}
