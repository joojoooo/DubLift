package dublift

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Native Termux builds can use Android's libc resolver. A pure-Go ARM64 build
// can instead use Termux's resolver file, since Android has no /etc/resolv.conf.
// An explicit override also works on Linux without changing system settings.
func platformResolver() *net.Resolver {
	server := strings.TrimSpace(os.Getenv("DUBLIFT_DNS"))
	prefix := os.Getenv("PREFIX")
	if server == "" && prefix != "" && (runtime.GOOS == "android" || strings.Contains(prefix, "com.termux")) {
		if b, err := os.ReadFile(filepath.Join(prefix, "etc", "resolv.conf")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[0] == "nameserver" && net.ParseIP(fields[1]) != nil {
					server = fields[1]
					break
				}
			}
		}
	}
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
