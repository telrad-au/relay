//go:build !windows

package main

func defaultConfigPath() string {
	if distribution == "docker" {
		return "/var/lib/telrad-relay/relay.json"
	}
	return "/etc/telrad-relay/relay.json"
}

func defaultDataDir() string { return "/var/lib/telrad-relay" }
