package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseReportReceiver(t *testing.T) {
	for _, test := range []struct {
		value string
		host  string
		port  int
	}{
		{"ris.clinic.local", "ris.clinic.local", 0},
		{"RIS_1.clinic-a.local:2577", "RIS_1.clinic-a.local", 2577},
		{"192.0.2.20", "192.0.2.20", 0},
		{"192.0.2.20:1", "192.0.2.20", 1},
		{"192.0.2.20:65535", "192.0.2.20", 65535},
		{"2001:db8::20", "2001:db8::20", 0},
		{"[2001:db8::20]", "2001:db8::20", 0},
		{"[2001:db8::20]:2576", "2001:db8::20", 2576},
		{"[::1]:12576", "::1", 12576},
		{strings.Repeat("a", 253), strings.Repeat("a", 253), 0},
	} {
		host, port, err := parseReportReceiver(test.value)
		if err != nil || host != test.host || port != test.port {
			t.Fatalf("%q: host=%q port=%d err=%v", test.value, host, port, err)
		}
	}
	for _, value := range []string{
		"", ".ris.local", "-ris.local", "ris local", "ris.local/x", "user@ris.local", "ris.local:", "ris.local:0",
		"ris.local:65536", "ris.local:02576", "ris.local:+1", "ris.local:x", "a:b:c", "[2001:db8::20", "[2001:db8::20]2576",
		"[2001:db8::20]:", "[ris.local]:2576", "[192.0.2.20]:2576", "::ffff:192.0.2.20x", "fe80::1%eth0", strings.Repeat("a", 254),
	} {
		if host, port, err := parseReportReceiver(value); err == nil {
			t.Fatalf("%q accepted as host=%q port=%d", value, host, port)
		}
	}
}

func TestSetJSONMembersPreservesTheRest(t *testing.T) {
	host := jsonMember{name: "reportHost", value: []byte(`"192.0.2.20"`)}
	port := jsonMember{name: "reportPort", value: []byte(`2577`)}
	for _, test := range []struct {
		name, input, want string
		members           []jsonMember
	}{
		{
			"installer file", "{\n  \"schemaVersion\": 6,\n  \"reportHost\": \"report-receiver.invalid\",\n  \"reportPort\": 2576\n}\n",
			"{\n  \"schemaVersion\": 6,\n  \"reportHost\": \"192.0.2.20\",\n  \"reportPort\": 2577\n}\n", []jsonMember{host, port},
		},
		{
			"host only keeps the port", "{\n  \"schemaVersion\": 6,\n  \"reportHost\": \"old\",\n  \"reportPort\": 2576\n}\n",
			"{\n  \"schemaVersion\": 6,\n  \"reportHost\": \"192.0.2.20\",\n  \"reportPort\": 2576\n}\n", []jsonMember{host},
		},
		{
			"order, tabs and spacing", "{\n\t\"reportPort\" :  1,\n\t\"dicomPort\": 11112,\n\t\"schemaVersion\": 6,\n\t\"reportHost\" :  \"old\"   ,\n\t\"hl7Port\": 2575\n}",
			"{\n\t\"reportPort\" :  2577,\n\t\"dicomPort\": 11112,\n\t\"schemaVersion\": 6,\n\t\"reportHost\" :  \"192.0.2.20\"   ,\n\t\"hl7Port\": 2575\n}", []jsonMember{host, port},
		},
		{
			"missing members are appended with the file's indentation", "{\n    \"schemaVersion\": 6,\n    \"dataDir\": \"/var/lib/telrad-relay\"\n}\n",
			"{\n    \"schemaVersion\": 6,\n    \"dataDir\": \"/var/lib/telrad-relay\",\n    \"reportHost\": \"192.0.2.20\",\n    \"reportPort\": 2577\n}\n", []jsonMember{host, port},
		},
		{
			"single line", `{"schemaVersion":6,"reportHost":"old"}`,
			`{"schemaVersion":6,"reportHost":"192.0.2.20","reportPort":2577}`, []jsonMember{host, port},
		},
		{
			"keys match without case as encoding/json does", "{\"schemaVersion\": 6, \"ReportHost\": \"old\"}",
			"{\"schemaVersion\": 6, \"ReportHost\": \"192.0.2.20\"}", []jsonMember{host},
		},
		{
			"empty object", "{}\n", "{\n  \"reportHost\": \"192.0.2.20\",\n  \"reportPort\": 2577\n}\n", []jsonMember{host, port},
		},
		{
			"nested values stay as written", "{\n  \"schemaVersion\": 6,\n  \"reportHost\": \"a\\u002eb\",\n  \"listenAddress\": \"0.0.0.0\"\n}",
			"{\n  \"schemaVersion\": 6,\n  \"reportHost\": \"192.0.2.20\",\n  \"listenAddress\": \"0.0.0.0\"\n}", []jsonMember{host},
		},
	} {
		got, err := setJSONMembers([]byte(test.input), test.members)
		if err != nil || string(got) != test.want {
			t.Fatalf("%s:\n got %q (%v)\nwant %q", test.name, got, err, test.want)
		}
	}
	for _, input := range []string{"", "[]", "{", `{"a":1}{"b":2}`, `{"a":}`, "null"} {
		if _, err := setJSONMembers([]byte(input), []jsonMember{host}); err == nil {
			t.Fatalf("%q accepted", input)
		}
	}
}

// receiverConfig writes a relay.json with every field, in the example's order.
func receiverConfig(t *testing.T) (string, *config) {
	t.Helper()
	example, err := os.ReadFile(filepath.Join("..", "..", "packaging", "relay.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	example = bytes.Replace(example, []byte(`"/var/lib/telrad-relay"`), mustMarshal(t, dataDir), 1)
	path := filepath.Join(t.TempDir(), "relay.json")
	if err := os.WriteFile(path, example, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, cfg
}

func mustMarshal(t *testing.T, value string) []byte {
	t.Helper()
	return []byte(`"` + strings.ReplaceAll(value, `\`, `\\`) + `"`)
}

func TestSetReportReceiverEditsOnlyReceiver(t *testing.T) {
	path, cfg := receiverConfig(t)
	before, _ := os.ReadFile(path)
	operator := newTestOperator(t)
	operator.service.isRunning = true
	operator.status.set(&statusReport{State: "ready", ReportReceiverConfigured: true, ReportReceiver: "192.0.2.20:2576"})
	operator.service.onStart = func() {
		operator.status.set(&statusReport{State: "ready", ReportReceiverConfigured: true, ReportReceiver: "[2001:db8::20]:12576"})
	}
	if err := setReportReceiver(operator.operatorEnv, cfg, path, "[2001:db8::20]:12576"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	want := strings.Replace(strings.Replace(string(before), `"reportHost": "192.0.2.20"`, `"reportHost": "2001:db8::20"`, 1), `"reportPort": 2576`, `"reportPort": 12576`, 1)
	if string(after) != want {
		t.Fatalf("relay.json:\n%s\nwant:\n%s", after, want)
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0644 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".telrad-write-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	if operator.service.history() != "restart" {
		t.Fatalf("service actions=%q", operator.service.history())
	}
	output := operator.output.String()
	for _, line := range []string{
		"Report receiver set to [2001:db8::20]:12576 in " + path + ".",
		"Restarted " + serviceDisplayName + "; it now delivers reports to [2001:db8::20]:12576.",
	} {
		if !strings.Contains(output, line) {
			t.Fatalf("output missing %q:\n%s", line, output)
		}
	}

	// Without a port the current one is kept; a stopped service stays stopped.
	operator = newTestOperator(t)
	cfg, _ = loadConfig(path)
	if err := setReportReceiver(operator.operatorEnv, cfg, path, "ris.clinic.local"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = loadConfig(path)
	if cfg.ReportHost != "ris.clinic.local" || cfg.ReportPort != 12576 || operator.service.history() != "" {
		t.Fatalf("host=%q port=%d actions=%q", cfg.ReportHost, cfg.ReportPort, operator.service.history())
	}
	if !strings.Contains(operator.output.String(), "is not running; the new report receiver applies when it starts") {
		t.Fatalf("output=%q", operator.output.String())
	}

	// Setting the same receiver again changes nothing.
	operator = newTestOperator(t)
	written, _ := os.Stat(path)
	if err := setReportReceiver(operator.operatorEnv, cfg, path, "ris.clinic.local:12576"); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(path); !again.ModTime().Equal(written.ModTime()) || !strings.Contains(operator.output.String(), "already ris.clinic.local:12576") {
		t.Fatalf("unchanged receiver rewrote the file: %q", operator.output.String())
	}
}

func TestSetReportReceiverRefusals(t *testing.T) {
	path, cfg := receiverConfig(t)
	before, _ := os.ReadFile(path)
	unchanged := func(context string) {
		t.Helper()
		if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
			t.Fatalf("%s changed relay.json", context)
		}
	}
	operator := newTestOperator(t)
	operator.elevated = func() bool { return false }
	if err := setReportReceiver(operator.operatorEnv, cfg, path, "192.0.2.30"); err == nil || !strings.Contains(err.Error(), "changes the installation") {
		t.Fatalf("unelevated: %v", err)
	}
	unchanged("an unelevated command")
	operator = newTestOperator(t)
	for _, value := range []string{"bad host", "ris.local:0", "[ris.local]"} {
		if err := setReportReceiver(operator.operatorEnv, cfg, path, value); err == nil {
			t.Fatalf("%q accepted", value)
		}
	}
	unchanged("an invalid receiver")

	// The rest of the configuration must stay valid.
	invalid := *cfg
	invalid.DicomPort = invalid.HL7Port
	if err := setReportReceiver(operator.operatorEnv, &invalid, path, "192.0.2.30"); err == nil || !strings.Contains(err.Error(), "dicomPort and hl7Port") {
		t.Fatalf("invalid configuration: %v", err)
	}
	unchanged("an invalid configuration")

	// A restart that does not show the new receiver is reported.
	operator.service.isRunning = true
	operator.status.set(&statusReport{State: "ready", ReportReceiverConfigured: true, ReportReceiver: "192.0.2.20:2576"})
	if err := setReportReceiver(operator.operatorEnv, cfg, path, "192.0.2.30"); err == nil || !strings.Contains(err.Error(), "check telrad status") {
		t.Fatalf("unconfirmed restart: %v", err)
	}

	useContainer(t)
	if err := reportReceiverCommand(cfg, path, []string{"192.0.2.40"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "TELRAD_RELAY_REPORT_HOST") {
		t.Fatalf("container: %v", err)
	}
}

func TestSetReportReceiverCreatesMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	cfg := testConfig(t, nil)
	cfg.ReportHost = reportHostPlaceholder
	cfg.StatusAddress = "127.0.0.1:8425"
	operator := newTestOperator(t)
	if err := setReportReceiver(operator.operatorEnv, cfg, path, "192.0.2.20:2577"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{\n  \"schemaVersion\": 6,\n  \"reportHost\": \"192.0.2.20\",\n  \"reportPort\": 2577\n}\n" {
		t.Fatalf("relay.json=%q", data)
	}
}

func TestShowReportReceiver(t *testing.T) {
	path, _ := receiverConfig(t)
	var out bytes.Buffer
	if err := execute([]string{"--config", path, "report-receiver"}, &out); err != nil || out.String() != "report receiver: 192.0.2.20:2576\n" {
		t.Fatalf("configured: %v %q", err, out.String())
	}
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(data, []byte(`"192.0.2.20"`), []byte(`"report-receiver.invalid"`), 1), 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := execute([]string{"--config", path, "report-receiver"}, &out); err != nil ||
		out.String() != "report receiver: NOT CONFIGURED\nSet it with: "+reportReceiverUsage+"\n" {
		t.Fatalf("placeholder: %v %q", err, out.String())
	}
	if err := execute([]string{"--config", path, "report-receiver", "a", "b"}, &out); err == nil {
		t.Fatal("two arguments accepted")
	}
	useContainer(t)
	out.Reset()
	if err := execute([]string{"--config", path, "report-receiver"}, &out); err != nil || !strings.Contains(out.String(), "Set TELRAD_RELAY_REPORT_HOST") {
		t.Fatalf("container: %v %q", err, out.String())
	}
}
