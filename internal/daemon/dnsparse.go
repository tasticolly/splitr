package daemon

import "strings"

// Parsing helpers for the networksetup output, kept apart from the exec calls
// so they can be tested without a Mac and without touching the system.

// parseDNSServers reads `networksetup -getdnsservers <service>`. With nothing
// configured the command does not print an empty list, it prints a sentence
// ("There aren't any DNS Servers set on Wi-Fi."), so anything that is not an
// address is dropped rather than stored and later handed back as a resolver.
func parseDNSServers(out string) []string {
	var servers []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.ContainsAny(line, " \t") {
			continue
		}
		servers = append(servers, line)
	}
	return servers
}

// serviceForDevice picks the service bound to device out of
// `networksetup -listnetworkserviceorder`, whose entries look like:
//
//	(1) Wi-Fi
//	(Hardware Port: Wi-Fi, Device: en0)
//
// The name is on the line before the one naming the device, so the last name
// seen is remembered until the device line either confirms or discards it.
// A service disabled in System Settings is prefixed with an asterisk, which is
// not part of its name.
func serviceForDevice(order, device string) string {
	var candidate string
	for _, line := range strings.Split(order, "\n") {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "("); ok {
			if _, rest, found := strings.Cut(name, ")"); found && !strings.HasPrefix(name, "Hardware Port:") {
				candidate = strings.TrimSpace(rest)
				continue
			}
		}
		if !strings.HasPrefix(line, "(Hardware Port:") {
			continue
		}
		if _, rest, ok := strings.Cut(line, "Device:"); ok {
			if strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), ")")) == device {
				return strings.TrimPrefix(candidate, "*")
			}
		}
	}
	return ""
}
