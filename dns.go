package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// parseDNSServers turns "192.168.1.1, 1.1.1.1:53" into host:port addresses.
func parseDNSServers(v string) ([]string, error) {
	var out []string
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		host, port, err := net.SplitHostPort(s)
		if err != nil { // no port given
			host, port = strings.Trim(s, "[]"), "53"
		}
		if net.ParseIP(host) == nil {
			return nil, fmt.Errorf("DNS_SERVERS: %q ist keine IP-Adresse", s)
		}
		out = append(out, net.JoinHostPort(host, port))
	}
	return out, nil
}

// newDialer returns the dial function for connections to the LLM servers. With DNS servers
// configured, names are resolved there first (in the given order); if that fails, the container's
// normal resolver (Docker DNS, /etc/hosts, extra_hosts) is tried, so names of other containers keep working.
func newDialer(servers []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	base := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	if len(servers) == 0 {
		return base.DialContext
	}
	custom := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second, Resolver: &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			var last error
			for _, s := range servers {
				c, err := d.DialContext(ctx, network, s)
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
	}}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		if net.ParseIP(host) != nil {
			return base.DialContext(ctx, network, addr) // plain IP: nothing to resolve
		}
		c, err := custom.DialContext(ctx, network, addr)
		var dnsErr *net.DNSError
		if err != nil && errors.As(err, &dnsErr) {
			if c2, err2 := base.DialContext(ctx, network, addr); err2 == nil {
				return c2, nil
			}
		}
		return c, err
	}
}
