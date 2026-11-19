package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	_ "modernc.org/sqlite"
)

const (
	maxNameSuffix  = 999_999_999
	nameSequenceDB = "names.sqlite3"
)

func highestNameSuffix(prefix string, instances []agent37.Instance) int {
	var highest int
	for _, instance := range instances {
		for _, name := range []string{instance.Name, instance.Metadata["environment"]} {
			suffix, ok := strings.CutPrefix(name, prefix+"-")
			if !ok {
				continue
			}
			number, err := strconv.Atoi(suffix)
			if err == nil && number > highest && number <= maxNameSuffix && strconv.Itoa(number) == suffix {
				highest = number
			}
		}
	}
	return highest
}

func openNameSequence(state string, preview bool) (*sql.DB, error) {
	path, err := filepath.Abs(filepath.Join(state, nameSequenceDB))
	if err != nil {
		return nil, err
	}
	file := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	if !strings.HasPrefix(file.Path, "/") {
		file.Path = "/" + file.Path
	}
	params := url.Values{"_pragma": {"busy_timeout(5000)"}}
	if preview {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
		params.Set("mode", "ro")
	}
	file.RawQuery = params.Encode()
	return sql.Open("sqlite", file.String())
}

func previewSandboxName(ctx context.Context, state, prefix string) (string, error) {
	db, err := openNameSequence(state, true)
	if errors.Is(err, os.ErrNotExist) {
		return numberedSandboxName(prefix, 1)
	}
	if err != nil {
		return "", err
	}
	defer db.Close()
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'name_counters')").Scan(&exists); err != nil {
		return "", err
	}
	var last int
	if exists {
		err := db.QueryRowContext(ctx, "SELECT last FROM name_counters WHERE prefix = ?", prefix).Scan(&last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return numberedSandboxName(prefix, last+1)
}

func reserveSandboxName(ctx context.Context, state, prefix string, instances []agent37.Instance) (string, error) {
	floor := highestNameSuffix(prefix, instances) + 1
	if _, err := numberedSandboxName(prefix, floor); err != nil {
		return "", err
	}
	db, err := openNameSequence(state, false)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS name_counters (prefix TEXT PRIMARY KEY, last INTEGER NOT NULL)"); err != nil {
		return "", err
	}
	// Reserve a suffix in one statement so separate CLI processes cannot reuse it.
	var next int
	err = db.QueryRowContext(ctx, `
		INSERT INTO name_counters (prefix, last) VALUES (?, ?)
		ON CONFLICT(prefix) DO UPDATE SET last = MAX(name_counters.last + 1, excluded.last)
		WHERE name_counters.last < ?
		RETURNING last
	`, prefix, floor, maxNameSuffix).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return numberedSandboxName(prefix, maxNameSuffix+1)
	}
	if err != nil {
		return "", err
	}
	return numberedSandboxName(prefix, next)
}

func numberedSandboxName(prefix string, number int) (string, error) {
	if number > maxNameSuffix {
		return "", fmt.Errorf("numbered names for prefix %q are exhausted", prefix)
	}
	return prefix + "-" + strconv.Itoa(number), nil
}
