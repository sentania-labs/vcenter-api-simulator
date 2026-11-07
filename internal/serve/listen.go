package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Listeners serves one handler on several TLS addresses.
type Listeners struct {
	servers []*http.Server
	Addrs   []string
}

// Listen binds every address and starts serving. Binding is synchronous so a
// port in use is reported before the simulator claims to be up. Each
// handshake takes the certificate from cert, so a rotated pair is served
// without a restart.
func Listen(addrs []string, handler http.Handler, cert *Certificate) (*Listeners, error) {
	l := &Listeners{}
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("listen %s: %w", addr, err)
		}
		srv := &http.Server{
			Handler:           handler,
			TLSConfig:         &tls.Config{GetCertificate: cert.GetCertificate, MinVersion: tls.VersionTLS12},
			ReadHeaderTimeout: 30 * time.Second,
		}
		l.servers = append(l.servers, srv)
		l.Addrs = append(l.Addrs, ln.Addr().String())
		go func() {
			if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Println("serve:", err)
			}
		}()
	}
	return l, nil
}

// Close shuts the listeners down.
func (l *Listeners) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range l.servers {
		_ = s.Shutdown(ctx)
	}
}
