package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	"github.com/gofrs/flock"
)

const maxNameSuffix = 999_999_999

var errSandboxNameTaken = errors.New("generated sandbox name is in use")
var windowsDeviceName = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`)

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
			occupied[strings.TrimPrefix(name, ".sandbox-")] = struct{}{}
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
	names, err := previewSandboxNames(ctx, state, prefix, 1)
	if err != nil {
		return "", err
	}
	return names[0], nil
}

func previewSandboxNames(ctx context.Context, state, prefix string, count int) ([]string, error) {
	key, err := agent37APIKey()
	if err != nil && !errors.Is(err, errAgent37KeyMissing) {
		return nil, err
	}
	var instances []agent37.Instance
	if key != "" {
		instances, err = agent37.New(key).List(ctx)
		if err != nil {
			return nil, err
		}
	}
	occupied, err := occupiedSandboxNames(state, instances)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, count)
	for start := 1; len(names) < count; {
		name, suffix, err := nextSandboxName(prefix, occupied, start)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
		start = suffix + 1
	}
	return names, nil
}

func sandboxLock(state, name string) *flock.Flock {
	return flock.New(sandboxStatePath(state, name, ".lifecycle.lock"))
}

func sandboxStatePath(state, name, suffix string) string {
	if runtime.GOOS == "windows" && windowsDeviceName.MatchString(name) {
		name = ".sandbox-" + name
	}
	return filepath.Join(state, name+suffix)
}

func reserveSandboxName(ctx context.Context, state, prefix string, instances []agent37.Instance) (string, *flock.Flock, error) {
	reserved, err := reserveSandboxNames(ctx, state, prefix, instances, 1)
	if err != nil {
		return "", nil, err
	}
	return reserved[0].name, reserved[0].lock, nil
}

type sandboxReservation struct {
	name string
	lock *flock.Flock
}

func reserveSandboxNames(ctx context.Context, state, prefix string, instances []agent37.Instance, count int) (_ []sandboxReservation, err error) {
	occupied, err := occupiedSandboxNames(state, instances)
	if err != nil {
		return nil, err
	}
	reserved := make([]sandboxReservation, 0, count)
	defer func() {
		if err != nil {
			for _, entry := range reserved {
				err = errors.Join(err, entry.lock.Unlock())
			}
		}
	}()
	for start := 1; len(reserved) < count; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, suffix, err := nextSandboxName(prefix, occupied, start)
		if err != nil {
			return nil, err
		}
		lock := sandboxLock(state, name)
		locked, err := lock.TryLock()
		if err != nil {
			return nil, err
		}
		if locked {
			reserved = append(reserved, sandboxReservation{name, lock})
		}
		start = suffix + 1
	}
	return reserved, nil
}
