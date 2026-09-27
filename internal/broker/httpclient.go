package broker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// NewStaticIPClient returns an HTTP client whose every connection originates
// from bindIP. SEBI's retail-algo framework requires order traffic to come from
// the static IP registered with the broker; Kite rejects order requests from
// any other address. On a VPS with several addresses (or IPv6 enabled) the OS
// may otherwise pick the wrong source address, so we pin it explicitly and
// force IPv4.
func NewStaticIPClient(bindIP string, timeout time.Duration) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	network := "tcp"
	if bindIP != "" {
		ip := net.ParseIP(bindIP)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("bind ip %q is not IPv4", bindIP)
		}
		dialer.LocalAddr = &net.TCPAddr{IP: ip}
		network = "tcp4"
	}
	tr := &http.Transport{
		Proxy: nil, // never route order traffic through a proxy: it would change the source IP
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}
