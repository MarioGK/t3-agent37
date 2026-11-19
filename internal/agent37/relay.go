package agent37

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxRelayBody = 64 << 20

func relayTransport() *http.Transport {
	// Restrict TLS negotiation to HTTP/1.1 for T3's WebSocket upgrade.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		Protocols:             protocols,
	}
}

// RelayHandler forwards the native T3 protocol to the protected Agent37
// origin. The caller must listen only on the provided IPv4 loopback address.
func RelayHandler(origin, key, localHost string) (http.Handler, error) {
	target, err := url.Parse(origin)
	if err != nil || target.User != nil || target.Hostname() == "" || target.RawQuery != "" ||
		target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return nil, errors.New("invalid upstream T3 relay origin")
	}
	if target.Scheme != "https" && (target.Scheme != "http" || !net.ParseIP(target.Hostname()).IsLoopback()) {
		return nil, errors.New("upstream T3 relay origin must use HTTPS")
	}
	host, port, err := net.SplitHostPort(localHost)
	number, portErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || portErr != nil || number < 1 || number > 65535 {
		return nil, errors.New("T3 relay requires an IPv4 loopback address")
	}
	localOrigin := "http://" + localHost
	proxy := &httputil.ReverseProxy{
		Transport: relayTransport(),
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = target.Host
			for name := range request.Out.Header {
				lower := strings.ToLower(name)
				if lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-") {
					delete(request.Out.Header, name)
				}
			}
			request.Out.Header.Set("X-Agent37-Key", key)
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("X-Agent37-Key")
			response.Header.Del("Forwarded")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "T3 relay request is too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Agent37 T3 endpoint is unavailable", http.StatusBadGateway)
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Host != localHost {
			http.Error(w, "Use the loopback relay address", http.StatusForbidden)
			return
		}
		origins := request.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && origins[0] != localOrigin && origins[0] != "t3code://app") {
			http.Error(w, "Untrusted relay origin", http.StatusForbidden)
			return
		}
		if upgrade := request.Header.Get("Upgrade"); upgrade != "" && !strings.EqualFold(upgrade, "websocket") {
			http.Error(w, "Unsupported T3 relay upgrade", http.StatusBadRequest)
			return
		}
		if request.ContentLength > maxRelayBody {
			http.Error(w, "T3 relay request is too large", http.StatusRequestEntityTooLarge)
			return
		}
		request.Body = http.MaxBytesReader(w, request.Body, maxRelayBody)
		proxy.ServeHTTP(w, request)
	}), nil
}
