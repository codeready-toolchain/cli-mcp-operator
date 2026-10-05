package kubeconfig

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalHostPort returns a lowercase host:port. IPv6 is bracketed via
// net.JoinHostPort. Wildcards, leading-dot suffixes, and missing or invalid
// ports are rejected. A bare host does not match.
func CanonicalHostPort(hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", fmt.Errorf("host:port %q: %w", hostport, err)
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", fmt.Errorf("host:port %q: empty host", hostport)
	}
	if strings.Contains(host, "*") {
		return "", fmt.Errorf("host %q: wildcards are not allowed", host)
	}
	if strings.HasPrefix(host, ".") {
		return "", fmt.Errorf("host %q: leading-dot suffix is not allowed", host)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("host:port %q: invalid port", hostport)
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// HostPortFromServerURL extracts host:port from an https kubeconfig server URL.
// A missing port defaults to 443. Any other scheme is rejected.
func HostPortFromServerURL(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("parse server URL %q: %w", server, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("server URL %q must use https", server)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("server URL %q has no host", server)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	hp, err := CanonicalHostPort(net.JoinHostPort(host, port))
	if err != nil {
		return "", fmt.Errorf("server URL %q: %w", server, err)
	}
	return hp, nil
}

// ParseAllowlistDomain canonicalizes an allowlist domain. A bare host becomes
// host:443. Bracketed IPv6 without a port becomes [host]:443.
func ParseAllowlistDomain(domain string) (string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", fmt.Errorf("domain is empty")
	}
	if strings.Contains(domain, "*") {
		return "", fmt.Errorf("domain %q: wildcards are not allowed", domain)
	}
	if strings.HasPrefix(domain, ".") {
		return "", fmt.Errorf("domain %q: leading-dot suffix is not allowed", domain)
	}
	if _, _, err := net.SplitHostPort(domain); err == nil {
		return CanonicalHostPort(domain)
	}
	host := domain
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && len(host) > 2 {
		host = host[1 : len(host)-1]
	} else if strings.Contains(host, ":") {
		return "", fmt.Errorf("domain %q: IPv6 literals must use [host]:port", domain)
	}
	return CanonicalHostPort(net.JoinHostPort(host, "443"))
}
