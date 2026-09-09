package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExternalNetworkShaping(t *testing.T) {
	if os.Getenv("TELRAD_PERF_NETWORK_TEST") != "1" {
		t.Skip("set TELRAD_PERF_NETWORK_TEST=1 for Docker network-helper validation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp("", "relay-perf-net-")
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadProfile("clinic-v1")
	if err != nil {
		t.Fatal(err)
	}
	r := runner{root: root, out: work, work: work, id: filepath.Base(work), p: p}
	t.Cleanup(func() {
		r.cleanup()
		if len(r.cleanupErrors) > 0 {
			t.Errorf("network test cleanup: %v", r.cleanupErrors)
		}
	})
	tag := "telrad-relay-perf:" + r.id
	if _, err := r.docker(ctx, "build", "--file", "tools/relay-perf/Dockerfile", "--tag", tag, "."); err != nil {
		t.Fatal(err)
	}
	r.imageTags = append(r.imageTags, tag)
	r.helperImage = tag
	r.network = r.id
	if _, err := r.docker(ctx, "network", "create", "--ipv6=false", "--label", "telrad.perf="+r.id, r.network); err != nil {
		t.Fatal(err)
	}
	if err := makeTLS(work, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(work, "worker.json"), workerConfig{Profile: p}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.start(ctx, "cloud", []string{"--mount", "type=bind,src=" + work + ",dst=/work,readonly"}, "telrad-relay-perf:local", []string{"cloud"}); err != nil {
		t.Fatal(err)
	}
	proxy, err := r.start(ctx, "proxy", []string{"--publish", "127.0.0.1::8443"}, "telrad-relay-perf:local", []string{"proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.shapeExternalNetwork(ctx); err != nil {
		if len(r.containers) > 0 {
			b, _ := r.docker(ctx, "logs", r.containers[len(r.containers)-1])
			t.Logf("network helper: %s", b)
		}
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(work, "shaper-*.jsonl"))
	if err != nil || len(paths) != 2 {
		t.Fatal("missing shaped directions")
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(b), `"netem"`) != 1 || !strings.Contains(string(b), "u32") {
			t.Fatal("external cloud queue evidence missing")
		}
	}
	port, err := r.docker(ctx, "port", proxy, "8443/tcp")
	if err != nil {
		t.Fatal(err)
	}
	client, err := tlsClient(work)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, "GET", "https://"+strings.TrimSpace(string(port))+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("TLS cloud health through proxy: %d", response.StatusCode)
	}
}
