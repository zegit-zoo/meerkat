package intake

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// CheckSource reports whether a deposited fallback source may be handed
// to the researcher agent, which is told to open every source it cites
// (meerkat-mob#34). Only an https URL to a public host name or a public
// IP literal is accepted: no other scheme (file, git, http, ...), no
// credentials in the URL, and no loopback, link-local, private,
// shared-address, multicast or unspecified address. Nothing is resolved:
// a name is judged as written, and names that are addresses in disguise
// (localhost, a bare or dotted number, a hex label) are refused with the
// addresses.
func CheckSource(s string) error {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return errors.New("not a URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not https", u.Scheme)
	}
	if u.User != nil {
		return errors.New("credentials in the URL")
	}
	host := u.Hostname()
	if host == "" || u.Opaque != "" {
		return errors.New("no host")
	}
	if strings.Contains(host, "%") {
		return errors.New("a zoned address is not a public host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return checkAddr(a)
	}
	return checkHostName(host)
}

// nonPublic are ranges netip's predicates do not cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // shared address space (CGNAT)
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, broadcast
	netip.MustParsePrefix("::/96"),         // IPv4-compatible (deprecated)
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64: embeds an IPv4 address
	netip.MustParsePrefix("2002::/16"),     // 6to4: embeds an IPv4 address
}

func checkAddr(a netip.Addr) error {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return errors.New("loopback address")
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return errors.New("link-local address")
	case a.IsPrivate():
		return errors.New("private address")
	case a.IsUnspecified(), a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return errors.New("not a unicast public address")
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return errors.New("not a public address")
		}
	}
	return nil
}

func checkHostName(host string) error {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return errors.New("loopback host name")
	}
	labels := strings.Split(h, ".")
	// No top-level domain is numeric or hex: a name whose last label is
	// one is an address another parser would read as such (127.1,
	// 2130706433, 0x7f.0.0.1).
	last := labels[len(labels)-1]
	if last == "" || isNumericLabel(last) {
		return errors.New("an address written as a host name")
	}
	for _, l := range labels {
		if l == "" {
			return errors.New("empty label in host name")
		}
	}
	return nil
}

func isNumericLabel(l string) bool {
	if rest, ok := strings.CutPrefix(l, "0x"); ok {
		l = rest
		if l == "" {
			return true
		}
		return strings.Trim(l, "0123456789abcdef") == ""
	}
	return strings.Trim(l, "0123456789") == ""
}
