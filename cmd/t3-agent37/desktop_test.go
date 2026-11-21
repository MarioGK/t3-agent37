package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MarioGK/t3-agent37/internal/agent37"
)

func TestDesktopPairingURLPreservesPrivateOriginAndFragment(t *testing.T) {
	const pairing = "https://a37-test.example.ts.net/pair#token=fixture%2Btoken"
	link, err := url.Parse(desktopPairingURL(pairing))
	if err != nil {
		t.Fatal(err)
	}
	if link.Scheme != "t3code" || link.Host != "pair" || link.Query().Get("pairingUrl") != pairing {
		t.Fatal("incorrect T3 desktop pairing link")
	}
}

func TestWaitForDesktopRequiresConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened, attempts := 0, 0
		open := func(context.Context) (string, error) {
			opened++
			return "first", nil
		}
		var lastHealthy time.Time
		var lastFailure time.Time
		err := waitForDesktop(t.Context(), open, func(context.Context, string, string) (desktopPairingStatus, error) {
			attempts++
			if attempts == 1 {
				lastFailure = time.Now()
				return desktopPairingStatus{}, &agent37.Error{Status: http.StatusTooManyRequests}
			}
			if attempts <= 4 {
				want := 100 * time.Millisecond
				if attempts == 2 {
					want = time.Second
				}
				if time.Since(lastFailure) != want {
					t.Errorf("failed-probe delay was %v, want %v", time.Since(lastFailure), want)
				}
				lastFailure = time.Now()
				return desktopPairingStatus{}, errors.New("temporary probe failure")
			}
			if attempts == 5 {
				lastHealthy = time.Now()
				return desktopPairingStatus{}, nil
			}
			if time.Since(lastHealthy) != 100*time.Millisecond {
				t.Errorf("steady polling interval was %v", time.Since(lastHealthy))
			}
			return desktopPairingStatus{Confirmed: true}, nil
		}, func() { t.Error("probe errors should not reopen the approval prompt") }, nil)
		if err != nil || attempts != 6 || opened != 1 {
			t.Fatalf("waiting for desktop: opened=%d, attempts=%d, err=%v", opened, attempts, err)
		}
	})
}

func TestWaitForDesktopRetriesAStalledConnectionOnce(t *testing.T) {
	for _, recovers := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			var opened, retries int
			labels := []string{"first", "second"}
			var started time.Time
			open := func(context.Context) (string, error) {
				if opened == len(labels) {
					t.Fatal("opened more than one retry")
				}
				label := labels[opened]
				opened++
				started = time.Now()
				return label, nil
			}
			err := waitForDesktop(t.Context(), open, func(_ context.Context, label, previous string) (desktopPairingStatus, error) {
				if label == "second" && previous != "first" {
					t.Error("the first request stopped being checked after retry")
				}
				return desktopPairingStatus{Redeemed: true, Confirmed: label == "second" && recovers}, nil
			}, func() {
				if time.Since(started) < desktopConnectGrace {
					t.Error("retried before the desktop had time to connect")
				}
				retries++
			}, nil)
			if opened != 2 || retries != 1 || (err == nil) != recovers {
				t.Fatalf("opened=%d, retries=%d, err=%v", opened, retries, err)
			}
		})
	}
}

func TestWaitForDesktopDoesNotRepeatAnUnansweredPrompt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		err := waitForDesktop(ctx, func(context.Context) (string, error) { return "first", nil },
			func(context.Context, string, string) (desktopPairingStatus, error) {
				return desktopPairingStatus{}, nil
			},
			func() { t.Error("reopened an unanswered request") }, nil)
		if err == nil || !strings.Contains(err.Error(), "confirmation timed out") {
			t.Fatalf("unanswered confirmation: %v", err)
		}
	})
}

func TestDesktopPromptGateSeparatesApprovalFromConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(desktopPromptGate, 1)
		firstOpened, secondOpened := make(chan struct{}), make(chan struct{})
		redeemFirst, connectFirst := make(chan struct{}), make(chan struct{})
		connectSecond := make(chan struct{})
		firstResult, secondResult := make(chan error, 1), make(chan error, 1)
		go func() {
			firstResult <- waitForDesktop(t.Context(), func(context.Context) (string, error) {
				close(firstOpened)
				return "first", nil
			}, func(context.Context, string, string) (desktopPairingStatus, error) {
				select {
				case <-connectFirst:
					return desktopPairingStatus{Confirmed: true}, nil
				case <-redeemFirst:
					return desktopPairingStatus{Redeemed: true}, nil
				default:
					return desktopPairingStatus{}, nil
				}
			}, func() { t.Error("unexpected retry") }, gate)
		}()
		<-firstOpened
		go func() {
			secondResult <- waitForDesktop(t.Context(), func(ctx context.Context) (string, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) != desktopPairingTimeout {
					t.Error("queued approval did not get its full deadline")
				}
				close(secondOpened)
				return "second", nil
			}, func(context.Context, string, string) (desktopPairingStatus, error) {
				select {
				case <-connectSecond:
					return desktopPairingStatus{Confirmed: true}, nil
				default:
					return desktopPairingStatus{}, nil
				}
			}, func() { t.Error("unexpected retry") }, gate)
		}()
		time.Sleep(4 * desktopPollInterval)
		select {
		case <-secondOpened:
			t.Error("second prompt opened before desktop redeemed the first")
		default:
		}
		close(redeemFirst)
		<-secondOpened
		time.Sleep(desktopConnectGrace + 2*desktopPollInterval)
		select {
		case <-firstResult:
			t.Error("redemption completed the first pairing without connection verification")
			return
		default:
		}
		close(connectFirst)
		if err := <-firstResult; err != nil {
			t.Fatal(err)
		}
		close(connectSecond)
		if err := <-secondResult; err != nil {
			t.Fatal(err)
		}
	})
}

func TestDesktopRetryWaitsForPromptSlotAndCancellationReleasesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(desktopPromptGate, 1)
		firstOpened, firstRetried, secondOpened := make(chan struct{}), make(chan struct{}), make(chan struct{})
		firstResult, secondResult := make(chan error, 1), make(chan error, 1)
		go func() {
			opened := false
			firstResult <- waitForDesktop(t.Context(), func(context.Context) (string, error) {
				if !opened {
					opened = true
					close(firstOpened)
					return "first", nil
				}
				close(firstRetried)
				return "retry", nil
			}, func(_ context.Context, label, previous string) (desktopPairingStatus, error) {
				if label == "first" {
					return desktopPairingStatus{Redeemed: true}, nil
				}
				if previous != "first" {
					t.Error("retry stopped checking the original grant")
				}
				return desktopPairingStatus{Confirmed: true}, nil
			}, func() {}, gate)
		}()
		<-firstOpened
		secondCtx, cancelSecond := context.WithCancel(t.Context())
		defer cancelSecond()
		go func() {
			secondResult <- waitForDesktop(secondCtx, func(context.Context) (string, error) {
				close(secondOpened)
				return "second", nil
			}, func(context.Context, string, string) (desktopPairingStatus, error) {
				return desktopPairingStatus{}, nil
			}, func() { t.Error("retried an unanswered prompt") }, gate)
		}()
		<-secondOpened
		time.Sleep(desktopConnectGrace + 2*desktopPollInterval)
		select {
		case <-firstRetried:
			t.Error("retry opened while another prompt was outstanding")
		default:
		}
		cancelSecond()
		if err := <-secondResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
		<-firstRetried
		if err := <-firstResult; err != nil {
			t.Fatal(err)
		}
	})
}

func TestDesktopPromptGateReleasesOnLaunchFailure(t *testing.T) {
	gate := make(desktopPromptGate, 1)
	openFailure := errors.New("could not open link")
	err := waitForDesktop(t.Context(), func(context.Context) (string, error) { return "", openFailure },
		func(context.Context, string, string) (desktopPairingStatus, error) {
			t.Fatal("checked connection after launcher failed")
			return desktopPairingStatus{}, nil
		},
		func() { t.Error("retried a failed launcher") }, gate)
	if !errors.Is(err, openFailure) || len(gate) != 0 {
		t.Fatalf("launch failure did not release prompt slot: %v", err)
	}
}

func TestLinkDryRunListsAllManagedSandboxesOrOneByName(t *testing.T) {
	t.Setenv(agent37KeyName, "desktop-test")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = namingTestTransport(func(request *http.Request) string {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/instances" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return `{"data":[
			{"id":"sandbox002","name":"ts-2","status":"stopped","metadata":{"managed_by":"t3-agent37","environment":"ts-2"}},
			{"id":"sandbox001","name":"ts-1","status":"running","metadata":{"managed_by":"t3-agent37","environment":"ts-1"}},
			{"id":"sandbox003","name":"other","status":"running"},
			{"id":"sandbox004","name":"ts-4","status":"deleted","metadata":{"managed_by":"t3-agent37","environment":"ts-4"}}
		]}`
	})
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"link", "--dry-run"}, []string{"ts-1", "ts-2"}},
		{[]string{"link", "ts-2", "--dry-run"}, []string{"ts-2"}},
	} {
		var output bytes.Buffer
		if err := run(t.Context(), test.args, &output, &output); err != nil {
			t.Fatal(err)
		}
		var rows []struct{ Name string }
		if err := json.Unmarshal(output.Bytes(), &rows); err != nil || len(rows) != len(test.want) {
			t.Fatalf("link preview = %s, %v", output.String(), err)
		}
		for i, want := range test.want {
			if rows[i].Name != want {
				t.Errorf("link preview target = %q, want %q", rows[i].Name, want)
			}
		}
	}
}
