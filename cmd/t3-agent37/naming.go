package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/gofrs/flock"
)

const maxNameSuffix = 999_999_999

var errSandboxNameTaken = errors.New("generated sandbox name is in use")

func occupiedSandboxNames(state string, instances []agent37.Instance) (map[string]struct{}, error) {
	occupied := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		if instance.Status != "deleted" {
			occupied[instance.Name] = struct{}{}
			occupied[instance.Metadata["environment"]] = struct{}{}
		}
	}

	entries, err := os.ReadDir(state)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".pending"); ok {
			occupied[name] = struct{}{}
		}
	}
	return occupied, nil
}

func nextSandboxName(prefix string, occupied map[string]struct{}, start int) (string, int, error) {
	for suffix := start; suffix <= maxNameSuffix; suffix++ {
		name := prefix + "-" + strconv.Itoa(suffix)
		if _, taken := occupied[name]; !taken {
			return name, suffix, nil
		}
	}
	return "", 0, fmt.Errorf("numbered names for prefix %q are exhausted", prefix)
}

func previewSandboxName(ctx context.Context, state, prefix string) (string, error) {
	key, err := agent37APIKey()
	if err != nil && !errors.Is(err, errAgent37KeyMissing) {
		return "", err
	}
	var instances []agent37.Instance
	if key != "" {
		instances, err = agent37.New(key).List(ctx)
		if err != nil {
			return "", err
		}
	}
	occupied, err := occupiedSandboxNames(state, instances)
	if err != nil {
		return "", err
	}
	name, _, err := nextSandboxName(prefix, occupied, 1)
	return name, err
}

func sandboxLock(state, name string) *flock.Flock {
	return flock.New(filepath.Join(state, name+".lifecycle.lock"))
}

func reserveSandboxName(ctx context.Context, state, prefix string, instances []agent37.Instance) (string, *flock.Flock, error) {
	occupied, err := occupiedSandboxNames(state, instances)
	if err != nil {
		return "", nil, err
	}
	for start := 1; ; {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		name, suffix, err := nextSandboxName(prefix, occupied, start)
		if err != nil {
			return "", nil, err
		}
		lock := sandboxLock(state, name)
		locked, err := lock.TryLock()
		if err != nil {
			return "", nil, err
		}
		if locked {
			return name, lock, nil
		}
		start = suffix + 1
	}
}
