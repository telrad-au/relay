package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
)

// reportReceiverCommand shows or sets the clinic report receiver.
//
//	report-receiver              show it
//	report-receiver HOST[:PORT]  set it in relay.json and restart the service
//
// An IPv6 address takes a port as [ADDRESS]:PORT. Without a port the current
// one is kept.
func reportReceiverCommand(cfg *config, configPath string, args []string, out io.Writer) error {
	if len(args) > 1 {
		return errors.New("report-receiver accepts one HOST[:PORT] argument")
	}
	if len(args) == 0 {
		showReportReceiver(out, cfg)
		return nil
	}
	if distribution == "docker" {
		return errors.New("a container's report receiver comes from TELRAD_RELAY_REPORT_HOST and TELRAD_RELAY_REPORT_PORT; change them and recreate the container")
	}
	return setReportReceiver(newOperatorEnv(cfg.StatusAddress, out), cfg, configPath, args[0])
}

func receiverAddress(host string, port int) string {
	return net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port))
}

func showReportReceiver(out io.Writer, cfg *config) {
	if reportReceiverConfigured(cfg.ReportHost) {
		fmt.Fprintf(out, "report receiver: %s\n", receiverAddress(cfg.ReportHost, cfg.ReportPort))
		return
	}
	fmt.Fprintln(out, "report receiver: NOT CONFIGURED")
	if distribution == "docker" {
		fmt.Fprintln(out, "Set TELRAD_RELAY_REPORT_HOST (and TELRAD_RELAY_REPORT_PORT) and recreate the container.")
		return
	}
	fmt.Fprintf(out, "Set it with: %s\n", reportReceiverUsage)
}

// parseReportReceiver splits HOST[:PORT]. A port of 0 means none was given.
// Hosts follow the installers' rules: letters, digits, '.', ':', '_' and '-',
// not starting with '.' or '-', at most 253 characters; a host containing ':'
// must be an IPv6 address, written in brackets when a port follows.
func parseReportReceiver(value string) (string, int, error) {
	host, portText := value, ""
	if strings.HasPrefix(value, "[") {
		end := strings.Index(value, "]")
		if end < 0 {
			return "", 0, errors.New("report receiver: an IPv6 address in brackets needs the closing ]")
		}
		host, portText = value[1:end], value[end+1:]
		if portText != "" {
			if !strings.HasPrefix(portText, ":") {
				return "", 0, errors.New("report receiver: expected [ADDRESS]:PORT")
			}
			portText = portText[1:]
			if portText == "" {
				return "", 0, errors.New("report receiver: the port after ':' is missing")
			}
		}
		if !strings.Contains(host, ":") {
			return "", 0, errors.New("report receiver: brackets are only for IPv6 addresses")
		}
	} else if strings.Count(value, ":") == 1 {
		host, portText, _ = strings.Cut(value, ":")
		if portText == "" {
			return "", 0, errors.New("report receiver: the port after ':' is missing")
		}
	}
	if !validReportHost(host) {
		return "", 0, errors.New("report receiver host must be a hostname or IP address, for example ris.clinic.local, 192.0.2.20 or [2001:db8::20]:2576")
	}
	port := 0
	if portText != "" {
		if !validPortText(portText) {
			return "", 0, errors.New("report receiver port must be from 1 to 65535")
		}
		port, _ = strconv.Atoi(portText)
	}
	return host, port, nil
}

func validReportHost(host string) bool {
	if host == "" || len(host) > 253 || host[0] == '.' || host[0] == '-' {
		return false
	}
	for _, character := range host {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') &&
			character != '.' && character != ':' && character != '_' && character != '-' {
			return false
		}
	}
	if strings.Contains(host, ":") {
		return net.ParseIP(host) != nil
	}
	return true
}

// validPortText accepts 1 to 65535 without a sign or leading zeros.
func validPortText(value string) bool {
	if value == "" || len(value) > 5 || value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	port, _ := strconv.Atoi(value)
	return port <= 65535
}

func setReportReceiver(env *operatorEnv, cfg *config, configPath, value string) error {
	host, port, err := parseReportReceiver(value)
	if err != nil {
		return err
	}
	if err := env.requireElevation("report-receiver"); err != nil {
		return err
	}
	updated := *cfg
	updated.ReportHost = host
	if port != 0 {
		updated.ReportPort = port
	}
	if err := validateConfig(&updated); err != nil {
		return fmt.Errorf("invalid relay configuration: %w", err)
	}
	address := receiverAddress(updated.ReportHost, updated.ReportPort)
	if cfg.ReportHost == updated.ReportHost && cfg.ReportPort == updated.ReportPort {
		fmt.Fprintf(env.out, "The report receiver is already %s; nothing was changed.\n", address)
		return nil
	}
	if err := updateReportReceiver(configPath, host, port); err != nil {
		return fmt.Errorf("update %s: %w", configPath, err)
	}
	fmt.Fprintf(env.out, "Report receiver set to %s in %s.\n", address, configPath)
	running, err := env.service.running()
	if err != nil {
		return err
	}
	if !running {
		fmt.Fprintf(env.out, "%s is not running; the new report receiver applies when it starts (telrad start).\n", serviceName())
		return nil
	}
	if err := env.service.restart(); err != nil {
		return err
	}
	configured := reportReceiverConfigured(host)
	if _, ok := env.waitForStatus(func(report *statusReport) bool {
		return report.ReportReceiverConfigured == configured && (!configured || report.ReportReceiver == address)
	}); !ok {
		return fmt.Errorf("restarted %s, but its status does not show the new report receiver; check telrad status", serviceDisplayName)
	}
	fmt.Fprintf(env.out, "Restarted %s; it now delivers reports to %s.\n", serviceDisplayName, address)
	return nil
}

// serviceName is serviceDisplayName for the start of a sentence.
func serviceName() string {
	return strings.ToUpper(serviceDisplayName[:1]) + serviceDisplayName[1:]
}

// updateReportReceiver sets reportHost, and reportPort unless port is 0, in
// the relay.json at path, creating the file as the installers would when it is
// absent. The edit must decode as a relay configuration carrying the new
// values before it replaces the file.
func updateReportReceiver(path, host string, port int) error {
	encodedHost, _ := json.Marshal(host)
	members := []jsonMember{{name: "reportHost", value: encodedHost}}
	if port != 0 {
		members = append(members, jsonMember{name: "reportPort", value: []byte(strconv.Itoa(port))})
	}
	data, err := os.ReadFile(path)
	missing := errors.Is(err, os.ErrNotExist)
	if missing {
		data = []byte(fmt.Sprintf("{\n  \"schemaVersion\": %d\n}\n", currentConfigSchemaVersion))
	} else if err != nil {
		return err
	}
	edited, err := setJSONMembers(data, members)
	if err != nil {
		return err
	}
	check := defaultConfig()
	if err := decodeConfig(edited, check); err != nil {
		return err
	}
	if check.ReportHost != host || port != 0 && check.ReportPort != port {
		return errors.New("the edited configuration does not carry the new report receiver")
	}
	if missing {
		// relay.json holds no secrets; the installers make it readable to all.
		return atomicWriteFile(path, edited, 0644)
	}
	return replaceFileAtomically(path, edited)
}

type jsonMember struct {
	name  string
	value []byte // encoded JSON
}

// setJSONMembers sets top-level members of the JSON object in data, touching
// nothing else. A member present under its name (matched without regard to
// case, as encoding/json does) has its value replaced in place; a missing one
// is added after the last member with that member's indentation.
func setJSONMembers(data []byte, members []jsonMember) ([]byte, error) {
	type edit struct {
		start, end int
		text       string
	}
	invalid := errors.New("relay configuration is not a JSON object")
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, invalid
	}
	open := int(decoder.InputOffset())
	var edits []edit
	found := make(map[string]bool)
	count, lastEnd := 0, open
	// Additions copy the file's own spacing: separator from a name to its
	// value, between from one member to the next.
	separator, between, indent, multiline := ": ", "", "  ", true
	for decoder.More() {
		keyStart := skipJSONSpace(data, int(decoder.InputOffset()))
		token, err := decoder.Token()
		if err != nil {
			return nil, invalid
		}
		name, _ := token.(string)
		keyEnd := int(decoder.InputOffset())
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, invalid
		}
		valueEnd := int(decoder.InputOffset())
		valueStart := valueEnd - len(raw)
		if count == 1 {
			between = string(data[lastEnd:keyStart])
		}
		if count == 0 {
			separator = string(data[keyEnd:valueStart])
			lineStart := bytes.LastIndexByte(data[:keyStart], '\n')
			multiline = lineStart >= open
			if multiline {
				indent = string(data[lineStart+1 : keyStart])
			}
		}
		for _, member := range members {
			if strings.EqualFold(name, member.name) {
				edits = append(edits, edit{valueStart, valueEnd, string(member.value)})
				found[member.name] = true
			}
		}
		count++
		lastEnd = valueEnd
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, invalid
	}
	closing := int(decoder.InputOffset()) - 1
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid
	}
	var added strings.Builder
	for _, member := range members {
		if found[member.name] {
			continue
		}
		switch {
		case between != "":
			added.WriteString(between)
		case count == 0 && added.Len() == 0:
			added.WriteString("\n" + indent)
		case multiline:
			added.WriteString(",\n" + indent)
		default:
			added.WriteString(",")
		}
		added.WriteString(strconv.Quote(member.name) + separator + string(member.value))
	}
	if added.Len() > 0 {
		if count == 0 {
			// An empty object is rewritten with one member per line.
			edits = append(edits, edit{open, closing, added.String() + "\n"})
		} else {
			edits = append(edits, edit{lastEnd, lastEnd, added.String()})
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	result := append([]byte(nil), data...)
	for _, change := range edits {
		result = append(result[:change.start], append([]byte(change.text), result[change.end:]...)...)
	}
	return result, nil
}

func skipJSONSpace(data []byte, offset int) int {
	for offset < len(data) && strings.IndexByte(" \t\r\n,", data[offset]) >= 0 {
		offset++
	}
	return offset
}
