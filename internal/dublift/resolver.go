package dublift

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
)

// Use the system resolver unless DUBLIFT_DNS specifies a DNS server IP.
func platformResolver() *net.Resolver {
	server := strings.TrimSpace(os.Getenv("DUBLIFT_DNS"))
	if server == "" {
		return nil
	}
	if ip := net.ParseIP(server); ip != nil {
		server = net.JoinHostPort(server, "53")
	}
	host, _, err := net.SplitHostPort(server)
	if err != nil || net.ParseIP(host) == nil {
		return nil
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, network, server)
	}}
}
