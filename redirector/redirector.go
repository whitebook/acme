// Package redirector provides a basic HTTP server for redirecting HTTP
// requests to HTTPS requests and serving ACME HTTP challenge values.
package redirector

import "net"
import "net/http"
import "gopkg.in/tylerb/graceful.v1"
import "time"
import "github.com/hlandau/xlog"
import "sync/atomic"
import "html"
import "fmt"
import "gopkg.in/hlandau/service.v2/daemon/chroot"
import "os"

var log, Log = xlog.New("acme.redirector")

type Config struct {
	Bind          string `default:":80" usage:"Bind address"`
	ChallengePath string `default:"/var/run/acme/acme-challenge" usage:"Path containing HTTP challenge files"`
}

type Redirector struct {
	cfg          Config
	httpServer   graceful.Server
	httpListener net.Listener
	stopping     uint32
}

func New(cfg Config) (*Redirector, error) {
	r := &Redirector{
		cfg: cfg,
		httpServer: graceful.Server{
			Timeout:          100 * time.Millisecond,
			NoSignalHandling: true,
			Server: &http.Server{
				Addr: cfg.Bind,
			},
		},
	}

	// Try and make the challenge path if it doesn't exist.
	err := os.MkdirAll(r.cfg.ChallengePath, 0755)
	if err != nil {
		return nil, err
	}

	l, err := net.Listen("tcp", r.httpServer.Server.Addr)
	if err != nil {
		return nil, err
	}

	r.httpListener = l

	return r, nil
}

func (r *Redirector) commonHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Server", "acmetool-redirector")
		rw.Header().Set("Content-Security-Policy", "default-src 'none'")
		h.ServeHTTP(rw, req)
	})
}

func (r *Redirector) Start() error {
	serveMux := http.NewServeMux()
	r.httpServer.Handler = r.commonHandler(serveMux)

	challengePath, ok := chroot.Rel(r.cfg.ChallengePath)
	if !ok {
		return fmt.Errorf("challenge path is not addressible inside chroot: %s", r.cfg.ChallengePath)
	}

	serveMux.HandleFunc("/", r.handleRedirect)
	serveMux.Handle("/.well-known/acme-challenge/",
		http.StripPrefix("/.well-known/acme-challenge/", http.FileServer(http.Dir(challengePath))))

	go func() {
		err := r.httpServer.Serve(r.httpListener)
		if atomic.LoadUint32(&r.stopping) == 0 {
			log.Fatale(err, "serve")
		}
	}()

	return nil
}

func (r *Redirector) Stop() error {
	atomic.StoreUint32(&r.stopping, 1)
	r.httpServer.Stop(r.httpServer.Timeout)
	<-r.httpServer.StopChan()
	return nil
}

func (r *Redirector) handleRedirect(rw http.ResponseWriter, req *http.Request) {
	// Redirect.
	u := *req.URL
	u.Scheme = "https"
	if u.Host == "" {
		u.Host = req.Host
	}
	if u.Host == "" {
		rw.WriteHeader(400)
		return
	}

	us := u.String()

	rw.Header().Set("Location", us)

	// If we are receiving any cookies, these must be insecure cookies, ergo
	// cookies aren't being set securely properly. This is a security issue.
	// Deleting cookies after the fact doesn't change the fact that they were
	// sent in cleartext and are thus forever untrustworthy. But it increases
	// the probability of somebody noticing something is up.
	//
	// ... However, the HTTP specification makes it impossible to delete a cookie
	// unless we know its domain and path, which aren't transmitted in requests.
	/*for _, c := range req.Cookies() {
	  http.SetCookie(rw, &http.Cookie{
	    Name: c.Name,
	    MaxAge: -1,
	  })
	}*/

	if req.Method == "GET" {
		rw.Header().Set("Cache-Control", "public; max-age=31536000")
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	}

	// This is a permanent redirect and the request method should be preserved.
	// It's unfortunate if the client has transmitted information in cleartext
	// via POST, etc., but there's nothing we can do about it at this stage.
	rw.WriteHeader(308)

	if req.Method == "GET" {
		// Redirects issued in response to GET SHOULD have a body pointing to the
		// new URL for clients which don't support redirects. (Whether the set of
		// clients supporting acceptably modern versions of TLS and not supporting
		// HTTP redirects is non-empty is another matter.)
		ue := html.EscapeString(us)
		rw.Write([]byte(fmt.Sprintf(`<!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml" lang="en"><title>Permanently Moved</title></head><body><h1>Permanently Moved</h1><p>This resource has <strong>moved permanently</strong> to <a href="%s">%s</a>.</p></body></html>`, ue, ue)))
	}
}

// © 2015 Hugo Landau <hlandau@devever.net>    MIT License
