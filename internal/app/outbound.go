package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Resolve once, reject non-public results, then dial that exact IP. No proxy or
// second DNS resolution may bypass the decision (including on redirects).
func publicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:2::/48",
		"2001:db8::/32", "2002::/16", "fec0::/10",
	} {
		if inCIDR(ip, cidr) {
			return false
		}
	}
	return true
}

func publicHTTPSURL(u *url.URL) error {
	if err := publicURL(u); err != nil {
		return err
	}
	if u.Scheme != "https" {
		return errors.New("an https URL is required")
	}
	return nil
}
func inCIDR(ip net.IP, cidr string) bool { _, n, _ := net.ParseCIDR(cidr); return n.Contains(ip) }
func publicURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("a public HTTP(S) URL without userinfo is required")
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return errors.New("only public HTTP(S) ports 80 and 443 are allowed")
	}
	host := strings.ToLower(strings.TrimRight(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return errors.New("private host blocked")
	}
	if ip := net.ParseIP(host); ip != nil && !publicIP(ip) {
		return errors.New("non-public address blocked")
	}
	return nil
}
func dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("no public address")
	}
	for _, a := range ips {
		if !publicIP(a.IP) || a.Zone != "" {
			return nil, errors.New("non-public address blocked")
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	for _, a := range ips {
		c, e := d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if e == nil {
			return c, nil
		}
		err = e
	}
	return nil, err
}

type publicRoundTripper struct {
	base      http.RoundTripper
	httpsOnly bool
}

func (t publicRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.httpsOnly {
		if err := publicHTTPSURL(r.URL); err != nil {
			return nil, err
		}
	} else if err := publicURL(r.URL); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(r)
}
func publicTransport() http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = dialPublic
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 20 * time.Second
	return publicRoundTripper{base: tr}
}
func publicHTTPSTransport() http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = dialPublic
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 20 * time.Second
	return publicRoundTripper{base: tr, httpsOnly: true}
}
func rejectRedirect(r *http.Request, via []*http.Request) error {
	return errors.New("redirects are not allowed for credential exchanges")
}

var oauthHTTPClient = &http.Client{Transport: publicHTTPSTransport(), Timeout: 20 * time.Second, CheckRedirect: rejectRedirect}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a != nil && b != nil && a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
