package peers

import (
	"context"
	"slices"
	"time"
)

// The owner session lasts five minutes. Keep enough time for the full
// two-minute mesh operation, including retries.
const preparedOwnerMaxAge = 2 * time.Minute
const preparedOwnerTimeout = 8 * time.Second

// PreparedOwnerSession overlaps issuing a scoped owner session with sandbox setup.
type PreparedOwnerSession struct {
	started time.Time
	ready   chan struct{}
	token   string
}

func PrepareOwnerSession(ctx context.Context, issue func(context.Context) (string, error)) *PreparedOwnerSession {
	session := &PreparedOwnerSession{started: time.Now(), ready: make(chan struct{})}
	go func() {
		defer close(session.ready)
		ctx, cancel := context.WithTimeout(ctx, preparedOwnerTimeout)
		defer cancel()
		if token, err := issue(ctx); err == nil {
			session.token = token
		}
	}()
	return session
}

func (session *PreparedOwnerSession) currentToken(ctx context.Context) string {
	select {
	case <-ctx.Done():
		return ""
	case <-session.ready:
		if ctx.Err() == nil && time.Since(session.started) < preparedOwnerMaxAge {
			return session.token
		}
		return ""
	}
}

// PrepareConfiguredPeers starts the owner commands early. The mesh still
// verifies the peer identity over private HTTPS before using any session.
func PrepareConfiguredPeers(ctx context.Context, configured []Peer) []Peer {
	configured = slices.Clone(configured)
	limit := make(chan struct{}, meshWorkers)
	for i, peer := range configured {
		configured[i].prepared = PrepareOwnerSession(ctx, func(ctx context.Context) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case limit <- struct{}{}:
				defer func() { <-limit }()
			}
			return issueOverSSH(ctx, peer)
		})
	}
	return configured
}
