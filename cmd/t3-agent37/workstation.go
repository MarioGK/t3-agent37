package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
	_ "modernc.org/sqlite"
)

const defaultPort = 43737

func statePath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" && runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, owner), nil
}

// Reuse the existing SQLite registry so installations upgraded from Python
// keep their loopback origins and T3 desktop pairing.
func assignedPort(ctx context.Context, state, id string, requested int) (int, error) {
	path := filepath.Join(state, "ports.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS ports (instance TEXT PRIMARY KEY, port INTEGER UNIQUE)"); err != nil {
		return 0, err
	}
	var existing int
	err = conn.QueryRowContext(ctx, "SELECT port FROM ports WHERE instance = ?", id).Scan(&existing)
	if err == nil {
		if requested != 0 && requested != existing {
			return 0, fmt.Errorf("this environment is already assigned loopback port %d", existing)
		}
		if existing < 1024 || existing > 65535 {
			return 0, errors.New("saved relay port is invalid")
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	first, end := defaultPort, defaultPort+100
	if requested != 0 {
		first, end = requested, requested+1
	}
	for port := first; port < end; port++ {
		var reserved int
		err := conn.QueryRowContext(ctx, "SELECT 1 FROM ports WHERE port = ?", port).Scan(&reserved)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		listener, err := net.Listen("tcp4", loopback(port))
		if err != nil {
			continue
		}
		insert, err := conn.ExecContext(ctx, "INSERT INTO ports VALUES (?, ?)", id, port)
		listener.Close()
		if err != nil {
			return 0, err
		}
		if count, err := insert.RowsAffected(); err != nil || count != 1 {
			return 0, errors.New("could not reserve the relay port")
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return 0, err
		}
		return port, nil
	}
	return 0, errors.New("no free loopback relay port in the requested range")
}

func releasePort(ctx context.Context, state, id string) error {
	path := filepath.Join(state, "ports.sqlite3")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS ports (instance TEXT PRIMARY KEY, port INTEGER UNIQUE)"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "DELETE FROM ports WHERE instance = ?", id)
	return err
}

func loopback(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func openPairing(pairing string) {
	link := "t3code://pair?" + url.Values{"pairingUrl": {pairing}}.Encode()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", link)
	case "windows":
		command = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", link)
	default:
		command = exec.Command("xdg-open", link)
	}
	if err := command.Start(); err == nil {
		_ = command.Process.Release()
	}
}

func serve(ctx context.Context, client *agent37.Client, relay *relayConfig, output io.Writer) error {
	origin, err := agent37.ProtectedOrigin(relay.instance, relay.remote)
	if err != nil {
		return err
	}
	address := loopback(relay.port)
	handler, err := agent37.RelayHandler(origin, client.Key, address)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	fmt.Fprintf(output, "Relay running at http://%s\n", address)
	if relay.pairing != "" {
		fmt.Fprintf(output, "Pair in T3 Code (expires in 5 minutes): %s\n", relay.pairing)
		openPairing(relay.pairing)
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
