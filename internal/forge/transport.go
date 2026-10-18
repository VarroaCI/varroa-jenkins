package forge

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// callTimeout bounds one forge call, including its body read.
const callTimeout = 15 * time.Second

// maxMetaBody caps the JSON that a metadata call decodes.
const maxMetaBody = 1 << 20

// refuseIP reports whether the operator must not dial ip. Only global
// unicast addresses pass, so loopback, link-local, group, and
// unspecified addresses are refused.
func refuseIP(ip net.IP) bool { return !ip.IsGlobalUnicast() }

// dialControl runs after DNS resolution and before each connect. It refuses
// a connection to a non-global address, so a forge name that resolves to a
// local address never reaches the socket. Loopback, link-local, group, and
// unspecified addresses are refused; private ranges are allowed by design, so
// a self-hosted forge on a LAN address is reachable.
func dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("forge dial address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("forge dial to a non-IP address refused")
	}
	if refuseIP(ip) {
		return fmt.Errorf("forge dial to %s refused", ip)
	}
	return nil
}

// NewHTTPClient returns the client that the operator uses for every forge
// call. It refuses non-global addresses, follows no redirect, and times out
// each call after 15 seconds.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: callTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: callTimeout,
				Control: dialControl,
			}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirect
		},
	}
}

// cancelBody ties the call's context cancel function to the response body,
// so the call's deadline lasts until the caller closes the body.
type cancelBody struct {
	body   io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Read(p []byte) (int, error) { return c.body.Read(p) }

func (c *cancelBody) Close() error {
	err := c.body.Close()
	c.cancel()
	return err
}
