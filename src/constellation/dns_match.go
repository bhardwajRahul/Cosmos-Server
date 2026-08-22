package constellation

import (
	"path"
	"strings"
)

// ConstellationDNSSuffix lets clients that refuse to forward single-label names
// (systemd-resolved, Windows) reach devices as <name>.constellation
const ConstellationDNSSuffix = ".constellation."

// matchesDomain reports whether qName matches hostname exactly or on a label
// boundary. DNS names are case-insensitive (RFC 4343) and clients may
// randomise case, so compare lowercased.
func matchesDomain(qName string, hostname string) bool {
	qName = strings.ToLower(qName)
	hostname = strings.ToLower(hostname)
	return qName == hostname + "." || strings.HasSuffix(qName, "." + hostname + ".")
}

// matchesCustomEntry matches a custom DNS key. Keys containing "*" or "?" are
// globs over the whole name ("*.example.com", "ads*.net"); "*" also spans
// dots. Plain keys match exactly or on a label boundary as before.
func matchesCustomEntry(qName string, key string) bool {
	if !strings.ContainsAny(key, "*?") {
		return matchesDomain(qName, key)
	}
	name := strings.ToLower(strings.TrimSuffix(qName, "."))
	ok, err := path.Match(strings.ToLower(strings.TrimSuffix(key, ".")), name)
	return err == nil && ok
}

// deviceQueryName maps "laptop.constellation." to "laptop." so a device answers
// under both the bare name and the suffixed one
func deviceQueryName(qName string) string {
	lower := strings.ToLower(qName)
	if strings.HasSuffix(lower, ConstellationDNSSuffix) && len(lower) > len(ConstellationDNSSuffix) {
		return qName[:len(qName)-len(ConstellationDNSSuffix)] + "."
	}
	return qName
}

// isBlacklisted reports whether domain or any of its parent domains is in DNSBlacklist,
// so a blacklisted "example.com" also blocks "ads.example.com"
func isBlacklisted(domain string) bool {
	for {
		if DNSBlacklist[domain] {
			return true
		}
		idx := strings.Index(domain, ".")
		if idx == -1 {
			return false
		}
		domain = domain[idx+1:]
	}
}
