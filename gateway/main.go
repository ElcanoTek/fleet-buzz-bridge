// Command fleet-buzz-gateway lets a fleet agent running outside the fleet
// host's loopback (a Buzz agent pod in a cluster on the same machine) reach
// fleet's chat API without holding fleet's API key.
//
// fleet's chat API authenticates callers with a per-user API key (X-API-Key,
// ADR-0108), which names one fleet user. The gateway listens on an address
// only the pods reach (the cluster bridge, e.g. 10.42.0.1), forwards to the
// chat API, and on every request:
//
//   - replaces X-API-Key with the real user API key, read from a file it
//     alone can read, and strips Authorization and any retired/proxy identity
//     headers, so the pod needs no fleet secret and can act only as the user
//     the key was minted for;
//   - lets through only what `fleet acp` uses (POST /chat, GET /me,
//     POST /conversations/{id}/cancel, GET /client-config, GET /healthz) and
//     refuses everything else;
//   - streams responses unbuffered (a turn is server-sent events).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("fleet-buzz-gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "address to listen on, e.g. 10.42.0.1:8081 (bound even before the address exists)")
	upstream := fs.String("upstream", "http://127.0.0.1:8080", "fleet's chat API")
	keyFile := fs.String("key-file", "", "file holding the fleet user API key (fleet sched apikey create ... --type user --user <email>)")
	tokenFile := fs.String("token-file", "", "alias for -key-file (kept for existing systemd unit files)")
	email := fs.String("email", "", "unused (kept for existing systemd unit files; the user API key names the account)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.New(stderr, "fleet-buzz-gateway: ", 0)
	path := *keyFile
	if path == "" {
		path = *tokenFile
	}
	if *listen == "" || path == "" {
		logger.Print("-listen and -key-file (or -token-file) are required")
		return 2
	}
	b, err := os.ReadFile(path)
	if err != nil {
		logger.Print(err)
		return 1
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		logger.Printf("%s is empty", path)
		return 1
	}
	up, err := url.Parse(*upstream)
	if err != nil || up.Scheme == "" || up.Host == "" {
		logger.Printf("bad -upstream %q", *upstream)
		return 2
	}
	ln, err := listenFreebind(*listen)
	if err != nil {
		logger.Print(err)
		return 1
	}
	srv := &http.Server{
		Handler:           newGateway(up, key, logger),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
		<-sigs
		_ = srv.Close()
	}()
	if *email != "" {
		logger.Printf("listening on %s, forwarding to %s (for %s)", *listen, up, *email)
	} else {
		logger.Printf("listening on %s, forwarding to %s", *listen, up)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Print(err)
		return 1
	}
	return 0
}

// listenFreebind listens with IP_FREEBIND, so the gateway can start before
// the cluster bridge address exists (k3s creates it with the first pod).
func listenFreebind(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	return lc.Listen(context.Background(), "tcp", addr)
}

// allowed is what `fleet acp` calls (internal/chattui's client).
var cancelPath = regexp.MustCompile(`^/conversations/[A-Za-z0-9._-]+/cancel$`)

func allowed(r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/chat":
	case r.Method == http.MethodGet && r.URL.Path == "/me":
	case r.Method == http.MethodGet && r.URL.Path == "/client-config":
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
	case r.Method == http.MethodPost && cancelPath.MatchString(r.URL.EscapedPath()):
	default:
		return false
	}
	return true
}

// Credential, proxy, and retired identity headers that must never pass through
// from the pod; the gateway pins the request's credential to its own X-API-Key.
var stripHeaders = []string{
	"X-API-Key", "Authorization",
	"X-Chat-Server-Token", "X-User-Email", "X-User-Session-Epoch",
	"X-Orchestrator-Server-Token", "X-Forwarded-For", "X-Real-Ip",
}

func newGateway(up *url.URL, apiKey string, logger *log.Logger) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			for _, h := range stripHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-API-Key", apiKey)
		},
		FlushInterval: -1, // a turn is server-sent events
		ErrorLog:      logger,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed(r) {
			http.Error(w, fmt.Sprintf("fleet-buzz-gateway: %s %s is not forwarded", r.Method, r.URL.Path), http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
