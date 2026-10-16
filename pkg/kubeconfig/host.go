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
// A missing port defaults to 443. Any other scheme is rejected. The host must
// be an IP or a DNS name, the same rule as an allowlist domain. Userinfo is
// rejected so a URL user is not dropped and then treated as a route.
func HostPortFromServerURL(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("parse server URL %q: %w", server, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("server URL %q must use https", server)
	}
	if u.User != nil {
		return "", fmt.Errorf("server URL %q: userinfo is not allowed", server)
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
	canonicalHost, _, err := net.SplitHostPort(hp)
	if err != nil {
		return "", fmt.Errorf("server URL %q: %w", server, err)
	}
	if err := validateAllowlistHost(canonicalHost, canonicalHost); err != nil {
		return "", fmt.Errorf("server URL %q: %w", server, err)
	}
	return hp, nil
}

// ParseAllowlistDomain canonicalizes an allowlist domain. A bare host becomes
// host:443. Bracketed IPv6 without a port becomes [host]:443. The host must be
// an IP or a DNS name. A trailing dot is rejected: clients connect to
// example.com, which would not match a route for example.com.
func ParseAllowlistDomain(domain string) (string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", fmt.Errorf("domain is empty")
	}
	if strings.Contains(domain, "://") || strings.Contains(domain, "/") {
		return "", fmt.Errorf("domain %q: not a host", domain)
	}
	if strings.Contains(domain, "*") {
		return "", fmt.Errorf("domain %q: wildcards are not allowed", domain)
	}
	if strings.HasPrefix(domain, ".") {
		return "", fmt.Errorf("domain %q: leading-dot suffix is not allowed", domain)
	}
	var hostport string
	if _, _, err := net.SplitHostPort(domain); err == nil {
		hostport, err = CanonicalHostPort(domain)
		if err != nil {
			return "", err
		}
	} else {
		host := domain
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && len(host) > 2 {
			host = host[1 : len(host)-1]
		} else if strings.Contains(host, ":") {
			return "", fmt.Errorf("domain %q: IPv6 literals must use [host]:port", domain)
		}
		hostport, err = CanonicalHostPort(net.JoinHostPort(host, "443"))
		if err != nil {
			return "", err
		}
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", err
	}
	if err := validateAllowlistHost(domain, host); err != nil {
		return "", err
	}
	return hostport, nil
}

// validateAllowlistHost accepts an IP or a DNS name. Other strings would be
// stored as routes that do not match the host a client connects to.
func validateAllowlistHost(domain, host string) error {
	if net.ParseIP(host) != nil {
		return nil
	}
	if strings.HasSuffix(host, ".") {
		return fmt.Errorf("domain %q: trailing dot is not allowed", domain)
	}
	if !dnsName(host) {
		return fmt.Errorf("domain %q: not a host", domain)
	}
	return nil
}

func dnsName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
