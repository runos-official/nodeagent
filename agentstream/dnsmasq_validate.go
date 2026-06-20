package agentstream

import (
	"fmt"
	"strings"
)

// dnsmasq config validation for UPDATE_DNSMASQ.
//
// The instruction writes FileContents verbatim to /etc/dnsmasq.d/runos.conf and
// restarts dnsmasq as root. dnsmasq has directives that pull in external config
// or run arbitrary scripts (conf-file, addn-config, addn-hosts, servers-file,
// dhcp-script, the script* family); those turn a config write into code/config
// execution and are hard-blocked here.
//
// Policy: parse line by line; allow comments and blank lines; allow a curated
// set of known-safe directives RunOS uses for split-horizon DNS; HARD-BLOCK the
// script/conf-include family. We deliberately allow-list known-safe directives
// rather than try to enumerate every dangerous one, but the explicit deny list
// is the security-critical part: even if a directive is mistakenly allow-listed,
// the deny family is rejected first.

// deniedDnsmasqDirectives are directives that load external config/hosts files
// or run scripts. They are rejected outright (these are the dangerous family).
var deniedDnsmasqDirectives = map[string]struct{}{
	"conf-file":      {},
	"conf-dir":       {},
	"addn-config":    {},
	"addn-hosts":     {},
	"hostsdir":       {},
	"servers-file":   {},
	"dhcp-script":    {},
	"dhcp-luascript": {},
	"dhcp-hostsfile": {},
	"dhcp-optsfile":  {},
	"script-arp":     {},
	"tftp-root":      {},
	"resolv-file":    {}, // pulls upstream servers from an attacker-named file
	"authoritative":  {}, // not used by RunOS split-horizon; avoid surprises
	"enable-dbus":    {},
}

// allowedDnsmasqDirectives are the directives RunOS legitimately uses for
// in-cluster split-horizon DNS. Both flag-style ("no-resolv") and key=value
// style ("listen-address=...") appear; the key is the part before any '='.
var allowedDnsmasqDirectives = map[string]struct{}{
	"address":             {}, // address=/foo.local/10.0.0.1
	"server":              {}, // server=/foo/1.2.3.4 or server=1.2.3.4
	"listen-address":      {},
	"bind-interfaces":     {},
	"bind-dynamic":        {},
	"interface":           {},
	"except-interface":    {},
	"no-resolv":           {},
	"no-poll":             {},
	"no-hosts":            {},
	"domain-needed":       {},
	"bogus-priv":          {},
	"cache-size":          {},
	"local":               {},
	"domain":              {},
	"expand-hosts":        {},
	"local-ttl":           {},
	"neg-ttl":             {},
	"max-ttl":             {},
	"min-cache-ttl":       {},
	"max-cache-ttl":       {},
	"dns-forward-max":     {},
	"all-servers":         {},
	"strict-order":        {},
	"stop-dns-rebind":     {},
	"rebind-localhost-ok": {},
	"port":                {},
	"user":                {},
	"group":               {},
	"log-queries":         {},
	"log-facility":        {},
	"pid-file":            {},
	"clear-on-reload":     {},
	"add-cpe-id":          {},
	"filter-aaaa":         {},
}

// validateDnsmasqContents enforces the policy above on the proposed config. It
// returns nil if every non-comment line is an allowed directive, else an error
// naming the first offending line. Directive names are matched
// case-insensitively after stripping a leading "--" (dnsmasq accepts both
// "no-resolv" and "--no-resolv").
func validateDnsmasqContents(contents string) error {
	for i, rawLine := range strings.Split(contents, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Directive is the token before the first '=' or whitespace.
		key := line
		if idx := strings.IndexAny(line, "= \t"); idx >= 0 {
			key = line[:idx]
		}
		key = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(key), "--"))

		if _, denied := deniedDnsmasqDirectives[key]; denied {
			return fmt.Errorf("dnsmasq directive %q (line %d) is not allowed: it loads external config or runs scripts", key, i+1)
		}
		if _, ok := allowedDnsmasqDirectives[key]; !ok {
			return fmt.Errorf("dnsmasq directive %q (line %d) is not in the allowed set", key, i+1)
		}
		// Defense in depth: any directive starting with "script" or "conf" is
		// rejected even if not enumerated above.
		if strings.HasPrefix(key, "script") || strings.HasPrefix(key, "conf-") {
			return fmt.Errorf("dnsmasq directive %q (line %d) is not allowed (script/conf-include family)", key, i+1)
		}
	}
	return nil
}
