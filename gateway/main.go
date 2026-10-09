// Command fleet-buzz-gateway lets a fleet agent running outside the fleet
// host's loopback (a Buzz agent pod in a cluster on the same machine) reach
// fleet's chat API without holding fleet's shared server token.
//
// fleet's chat API listens on loopback only and trusts two headers: the
// shared token (X-Chat-Server-Token), which can act as any fleet user, and
// the user (X-User-Email). The gateway listens on an address only the pods
// reach (the cluster bridge, e.g. 10.42.0.1), forwards to the chat API, and
// on every request:
//
//   - replaces X-Chat-Server-Token with the real token, read from a file it
//     alone can read, so the pod needs no fleet secret;
//   - replaces X-User-Email with the one user it was started for, so whatever
//     runs in the pod can act only as that user;
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
	tokenFile := fs.String("token-file", "", "file holding fleet's shared server token")
	email := fs.String("email", "", "the fleet user every request runs as")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.New(stderr, "fleet-buzz-gateway: ", 0)
	if *listen == "" || *tokenFile == "" || *email == "" {
		logger.Print("-listen, -token-file and -email are required")
		return 2
	}
	b, err := os.ReadFile(*tokenFile)
	if err != nil {
		logger.Print(err)
		return 1
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		logger.Printf("%s is empty", *tokenFile)
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
		Handler:           newGateway(up, token, *email, logger),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
		<-sigs
		_ = srv.Close()
	}()
	logger.Printf("listening on %s, forwarding to %s as %s", *listen, up, *email)
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

// Headers fleet's servers trust from a proxy in front of them; the gateway is
// one, so none may pass through from the pod.
var trustHeaders = []string{
	"X-Chat-Server-Token", "X-User-Email", "X-User-Session-Epoch",
	"X-Orchestrator-Server-Token", "X-Forwarded-For", "X-Real-Ip",
}

func newGateway(up *url.URL, token, email string, logger *log.Logger) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			for _, h := range trustHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-Chat-Server-Token", token)
			pr.Out.Header.Set("X-User-Email", email)
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
