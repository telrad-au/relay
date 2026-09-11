package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type runner struct {
	root, out, work, id, network, volume string
	p                                    profile
	mode                                 string
	cpus                                 float64
	memory                               int64
	externalRelay, supportHost           string
	containers                           []string
	sequence                             int
	supporting                           []string
	hostCPUs                             int
	cleanupErrors                        []string
	coverage                             string
	helperImage, targetImage             string
	imageTags                            []string
	fixtureVolume                        string
	remoteDocker                         string
	remoteContainers                     map[string]bool
}

func workerUser() string {
	if runtime.GOOS == "windows" {
		return "0:0"
	} // Windows bind mounts have no Unix UID.
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}

func (r *runner) docker(ctx context.Context, args ...string) ([]byte, error) {
	if r.remoteCommand(args) {
		args = append([]string{"--host", r.remoteDocker}, args...)
	}
	c := exec.CommandContext(ctx, "docker", args...)
	c.Dir = r.root
	b, err := c.CombinedOutput()
	if err != nil {
		// Only commands with no credentials or file contents are sent to Docker.
		_ = os.MkdirAll(r.out, 0700)
		_ = os.WriteFile(filepath.Join(r.out, "docker-error.log"), b, 0600)
		return b, fmt.Errorf("docker %s failed; see docker-error.log", args[0])
	}
	return b, nil
}

func (r *runner) start(ctx context.Context, role string, options []string, image string, command []string) (string, error) {
	r.sequence++
	name := fmt.Sprintf("%s-%s-%d", r.id, role, r.sequence)
	args := []string{"run", "--detach", "--name", name, "--label", "telrad.perf=" + r.id}
	remote := r.remoteDocker != "" && (role == "relay" || role == "init" || role == "observer" || role == "target-hardware")
	if remote {
		if r.remoteContainers == nil {
			r.remoteContainers = map[string]bool{}
		}
		r.remoteContainers[name] = true
		network := "none"
		if role == "relay" {
			network = "host"
		}
		args = append(args, "--network", network)
	}
	customNetwork := false
	for _, option := range options {
		if option == "--network" {
			customNetwork = true
		}
	}
	if r.network != "" && !customNetwork && !remote {
		args = append(args, "--network", r.network, "--network-alias", role)
	}
	args = append(args, options...)
	if r.coverage != "" && image == "telrad-relay-perf:local" {
		args = append(args, "--env", "GOCOVERDIR=/coverage", "--mount", "type=bind,src="+r.coverage+",dst=/coverage")
	}
	if image == "telrad-relay-perf:local" && r.helperImage != "" {
		image = r.helperImage
	}
	if image == "telrad-relay-perf-target:local" && r.targetImage != "" {
		image = r.targetImage
	}
	args = append(args, image)
	args = append(args, command...)
	// Register before creation so a daemon timeout after successful creation can
	// still be cleaned up. Only this run's unique names are ever removed.
	r.containers = append(r.containers, name)
	_, err := r.docker(ctx, args...)
	return name, err
}

func (r *runner) waitContainer(ctx context.Context, name string) error {
	b, err := r.docker(ctx, "wait", name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "0" {
		return errors.New("test container exited unsuccessfully")
	}
	return nil
}

func (r *runner) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := len(r.containers) - 1; i >= 0; i-- {
		name := r.containers[i]
		if b, err := r.docker(ctx, "inspect", name); err != nil {
			if !strings.Contains(string(b), "No such object") && !strings.Contains(string(b), "No such container") {
				r.cleanupErrors = append(r.cleanupErrors, "container_inventory_failed")
			}
			continue
		}
		// Let helpers flush measurements and optional test coverage before removal.
		if _, err := r.docker(ctx, "stop", "--time", "5", name); err != nil {
			r.cleanupErrors = append(r.cleanupErrors, "container_stop_failed")
		}
		logs, err := r.docker(ctx, "logs", "--tail", "200", name)
		if err == nil && len(logs) > 0 {
			_ = os.WriteFile(filepath.Join(r.out, strings.TrimPrefix(name, r.id+"-")+".log"), logs, 0600)
		}
		if _, err := r.docker(ctx, "rm", "--force", name); err != nil {
			r.cleanupErrors = append(r.cleanupErrors, "container_cleanup_failed")
		}
	}
	for _, volume := range []string{r.volume, r.fixtureVolume} {
		if volume != "" {
			if _, err := r.docker(ctx, "volume", "rm", volume); err != nil {
				r.cleanupErrors = append(r.cleanupErrors, "volume_cleanup_failed")
			}
		}
	}
	if r.network != "" {
		if _, err := r.docker(ctx, "network", "rm", r.network); err != nil {
			r.cleanupErrors = append(r.cleanupErrors, "network_cleanup_failed")
		}
	}
	for _, tag := range r.imageTags {
		if _, err := r.docker(ctx, "image", "rm", tag); err != nil {
			r.cleanupErrors = append(r.cleanupErrors, "image_tag_cleanup_failed")
		}
	}
	r.cleanupRemote(ctx)
	if r.work != "" {
		if err := os.RemoveAll(r.work); err != nil {
			r.cleanupErrors = append(r.cleanupErrors, "private_workspace_cleanup_failed")
		}
	}
}

func (r *runner) prepare(ctx context.Context, res *result) (workerConfig, error) {
	cfg := workerConfig{Profile: r.p, Origin: "https://cloud:8443", Admin: "http://cloud:8080", Relay: "relay", RIS: "ris:2576", Calibration: true, Mode: r.mode}
	if r.mode == "external-fault-check" {
		cfg.Mode = "faults"
	}
	var err error
	r.work, err = os.MkdirTemp("", "relay-perf-")
	if err != nil {
		return cfg, err
	}
	r.work, err = filepath.EvalSymlinks(r.work)
	if err != nil {
		return cfg, err
	}
	r.id = "relay-perf-" + filepath.Base(r.work)[len("relay-perf-"):]
	r.network = r.id
	if _, err := r.docker(ctx, "network", "create", "--ipv6=false", "--label", "telrad.perf="+r.id, r.network); err != nil {
		return cfg, err
	}
	info, err := r.docker(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return cfg, err
	}
	var host map[string]any
	if json.Unmarshal(info, &host) != nil {
		return cfg, errors.New("invalid Docker host information")
	}
	res.Host = map[string]any{}
	for source, target := range map[string]string{"OperatingSystem": "os", "Architecture": "architecture", "KernelVersion": "kernel", "NCPU": "cpus", "MemTotal": "memoryBytes", "CgroupVersion": "cgroupVersion", "MemoryLimit": "memoryLimit", "SwapLimit": "swapLimit", "CpuCfsQuota": "cpuQuota", "ServerVersion": "engine"} {
		res.Host[target] = host[source]
	}
	res.Host["controllerOS"], res.Host["controllerArch"] = runtime.GOOS, runtime.GOARCH
	res.Host["dependencies"] = map[string]string{"orthanc": orthancImage, "dcm4che": dcm4cheImage}
	res.Host["helperCoverage"] = r.coverage != ""
	res.Host["calibration"] = map[string]any{"bodyBytesPerSecond": 0, "networkShaping": false, "offeredFactor": r.p.Factor * 2, "receiptMillis": r.p.Receipt, "risAckMillis": r.p.ACK}
	res.Host["calibrationWorkers"] = map[string]int{"dicom": trafficWorkers(r.p, "dicom", true), "hl7": trafficWorkers(r.p, "hl7", true), "report": trafficWorkers(r.p, "report", true)}
	res.Host["calibrationHTTPSWarmupRequests"] = trafficWorkers(r.p, "dicom", true)
	r.hostCPUs = int(res.Host["cpus"].(float64))
	if res.Host["cgroupVersion"] != "2" || res.Host["memoryLimit"] != true || res.Host["swapLimit"] != true || res.Host["cpuQuota"] != true {
		return cfg, errors.New("host cannot verify hard CPU/memory/no-swap limits")
	}
	git := func(args ...string) string {
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = r.root
		b, _ := c.Output()
		return strings.TrimSpace(string(b))
	}
	res.Revision = git("rev-parse", "HEAD")
	res.Dirty = git("status", "--porcelain") != ""
	coverage := "false"
	if r.coverage != "" {
		coverage = "true"
	}
	helperTag, targetTag := "telrad-relay-perf:"+r.id, "telrad-relay-perf-target:"+r.id
	if _, err := r.docker(ctx, "build", "--build-arg", "PERF_COVERAGE="+coverage, "--file", "tools/relay-perf/Dockerfile", "--tag", helperTag, "."); err != nil {
		return cfg, err
	}
	r.imageTags = append(r.imageTags, helperTag)
	helperID, err := r.docker(ctx, "image", "inspect", "--format", "{{.Id}}", helperTag)
	if err != nil {
		return cfg, err
	}
	r.helperImage = strings.TrimSpace(string(helperID))
	res.Host["helperImage"] = r.helperImage
	if r.externalRelay == "" || r.remoteDocker != "" {
		if _, err := r.docker(ctx, "build", "--build-arg", "VERSION=0.0.0-perf", "--build-arg", "REVISION="+res.Revision, "--tag", targetTag, "."); err != nil {
			return cfg, err
		}
		r.imageTags = append(r.imageTags, targetTag)
		image, err := r.docker(ctx, "image", "inspect", "--format", "{{.Id}}", targetTag)
		if err != nil {
			return cfg, err
		}
		res.Image = strings.TrimSpace(string(image))
		r.targetImage = res.Image
	}
	metadata, err := r.start(ctx, "hardware", nil, "telrad-relay-perf:local", []string{"host-info"})
	if err != nil {
		return cfg, err
	}
	if err := r.waitContainer(ctx, metadata); err != nil {
		return cfg, err
	}
	metadataBytes, err := r.docker(ctx, "logs", metadata)
	if err != nil {
		return cfg, err
	}
	var hardware map[string]any
	if json.Unmarshal(metadataBytes, &hardware) != nil {
		return cfg, errors.New("host CPU identity unavailable")
	}
	res.Host["hardware"] = hardware
	if r.externalRelay != "" {
		cfg.Relay = r.externalRelay
		cfg.RIS = net.JoinHostPort(r.supportHost, "2576")
		cfg.Origin = "https://" + net.JoinHostPort(r.supportHost, "8443")
	}
	if err := makeTLS(r.work, r.supportHost); err != nil {
		return cfg, err
	}
	defaults, err := os.ReadFile(filepath.Join(r.root, "packaging/relay.example.json"))
	if err != nil {
		return cfg, err
	}
	if err := os.WriteFile(filepath.Join(r.work, "relay-defaults.json"), defaults, 0600); err != nil {
		return cfg, err
	}
	random := make([]byte, 48)
	if _, err := rand.Read(random); err != nil {
		return cfg, err
	}
	reportSeed := make([]byte, 32)
	if _, err := rand.Read(reportSeed); err != nil {
		return cfg, err
	}
	cfg.ReportSigningSeed = base64.RawURLEncoding.EncodeToString(reportSeed)
	cfg.Credential = "trr_v1_" + base64.RawURLEncoding.EncodeToString(random[:16]) + "_" + base64.RawURLEncoding.EncodeToString(random[16:])
	cfg.Fixtures, err = r.prepareFixtures(ctx)
	if err != nil {
		return cfg, err
	}
	if len(r.p.Mix) > 0 {
		if err := r.stageFixtureVolume(ctx); err != nil {
			return cfg, err
		}
		cfg.FixtureDirectory = "/fixtures"
		res.Host["fixtureStorage"] = "temporary Docker volume; 128 MiB generator cache"
	}
	res.Fixtures = append([]fixture(nil), cfg.Fixtures...)
	for i := range res.Fixtures {
		res.Fixtures[i].Instance = ""
	}
	if err := writeJSON(filepath.Join(r.work, "worker.json"), cfg); err != nil {
		return cfg, err
	}
	if r.remoteDocker != "" {
		if err := r.prepareRemote(ctx, res); err != nil {
			return cfg, err
		}
	}
	if err := writeJSON(filepath.Join(r.out, "profile.json"), r.p); err != nil {
		return cfg, err
	}
	if err := writeJSON(filepath.Join(r.out, "fixtures.json"), res.Fixtures); err != nil {
		return cfg, err
	}
	var resolved map[string]any
	if err := json.Unmarshal(defaults, &resolved); err != nil {
		return cfg, err
	}
	for k, v := range map[string]any{"relayId": "perf-relay", "pairingUrl": cfg.Origin + "/v1/relay/pairing-enrollments", "controlUrl": cfg.Origin + "/v1/relay/control", "dicomUrl": cfg.Origin + "/v1/relay/ingest/dicom", "hl7Url": cfg.Origin + "/v1/relay/ingest/hl7", "reportHost": "ris", "hl7MaxBytes": r.p.HL7Limit} {
		resolved[k] = v
	}
	if r.externalRelay != "" {
		resolved["reportHost"] = r.supportHost
	}
	if cfg.Mode == "faults" {
		resolved["dicomIdleTimeoutSeconds"] = 5
		resolved["dicomLifetimeSeconds"] = 15
	}
	if err := writeJSON(filepath.Join(r.out, "relay-config.json"), resolved); err != nil {
		return cfg, err
	}
	mount := []string{"--mount", "type=bind,src=" + r.work + ",dst=/work,readonly"}
	cloudOptions := append([]string{}, mount...)
	cloudOptions = append(cloudOptions, "--publish", "127.0.0.1::8080")
	cloud, err := r.start(ctx, "cloud", cloudOptions, "telrad-relay-perf:local", []string{"cloud"})
	if err != nil {
		return cfg, err
	}
	r.supporting = append(r.supporting, cloud)
	if r.externalRelay != "" {
		proxy, err := r.start(ctx, "proxy", []string{"--publish", "8443:8443"}, "telrad-relay-perf:local", []string{"proxy"})
		if err != nil {
			return cfg, err
		}
		r.supporting = append(r.supporting, proxy)
	}
	risOptions := append([]string{}, mount...)
	if r.externalRelay != "" {
		risOptions = append(risOptions, "--publish", "2576:2576")
	}
	ris, err := r.start(ctx, "ris", risOptions, "telrad-relay-perf:local", []string{"ris"})
	if err != nil {
		return cfg, err
	}
	r.supporting = append(r.supporting, ris)
	port, err := r.docker(ctx, "port", cloud, "8080/tcp")
	if err != nil {
		return cfg, err
	}
	local := cfg
	local.Admin = "http://" + strings.TrimSpace(string(port))
	if err := waitHTTP(ctx, local.Admin+"/health"); err != nil {
		return cfg, err
	}
	return local, nil
}

func (r *runner) startRelay(ctx context.Context, cfg workerConfig) (string, error) {
	r.volume = r.id + "-state"
	if _, err := r.docker(ctx, "volume", "create", "--label", "telrad.perf="+r.id, r.volume); err != nil {
		return "", err
	}
	init, err := r.start(ctx, "init", []string{"--mount", "type=bind,src=" + r.work + ",dst=/work,readonly", "--mount", "type=volume,src=" + r.volume + ",dst=/relaystate"}, "telrad-relay-perf:local", []string{"init"})
	if err != nil {
		return "", err
	}
	if err := r.waitContainer(ctx, init); err != nil {
		return "", err
	}
	return r.start(ctx, "relay", []string{"--cpus", strconv.FormatFloat(r.cpus, 'g', -1, 64), "--memory", strconv.FormatInt(r.memory, 10), "--memory-swap", strconv.FormatInt(r.memory, 10), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--mount", "type=volume,src=" + r.volume + ",dst=/var/lib/telrad-relay", "--mount", "type=bind,src=" + filepath.Join(r.work, "ca.pem") + ",dst=/test-ca.pem,readonly", "--env", "SSL_CERT_FILE=/test-ca.pem"}, "telrad-relay-perf-target:local", nil)
}

func (r *runner) traffic(ctx context.Context, cfg workerConfig, calibrate bool) (trafficResult, error) {
	worker, err := readWorker(r.work)
	if err != nil {
		return trafficResult{}, err
	}
	worker.Calibration = calibrate
	if err := writeJSON(filepath.Join(r.work, "worker.json"), worker); err != nil {
		return trafficResult{}, err
	}
	name := "traffic"
	if calibrate {
		name = "calibration"
	}
	options := []string{"--user", workerUser(), "--mount", "type=bind,src=" + r.work + ",dst=/work,readonly", "--mount", "type=bind,src=" + r.out + ",dst=/out"}
	options = append(options, r.fixtureMount()...)
	container, err := r.start(ctx, name, options, "telrad-relay-perf:local", []string{"traffic", "--out", "/out/" + name + ".json"})
	if err != nil {
		return trafficResult{}, err
	}
	err = r.waitContainer(ctx, container)
	var output trafficResult
	if readErr := readJSON(filepath.Join(r.out, name+".json"), &output); readErr != nil {
		return output, readErr
	}
	return output, err
}

func (r *runner) run(ctx context.Context) (res result, err error) {
	res = result{Schema: 1, Mode: r.mode, Status: "INCONCLUSIVE", Profile: r.p, Toolchain: runtime.Version()}
	if err = os.MkdirAll(r.out, 0700); err != nil {
		return res, err
	}
	if directory := os.Getenv("RELAY_PERF_COVERAGE_DIR"); directory != "" {
		if r.mode != "smoke" {
			return res, errors.New("test-helper coverage is supported only by smoke mode")
		}
		r.coverage, err = filepath.Abs(directory)
		if err != nil {
			return res, err
		}
		if err := os.MkdirAll(r.coverage, 0777); err != nil {
			return res, err
		}
		if err := os.Chmod(r.coverage, 0777); err != nil {
			return res, err
		}
		r.coverage, err = filepath.EvalSymlinks(r.coverage)
		if err != nil {
			return res, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, r.p.total()+20*time.Minute)
	defer cancel()
	defer func() {
		// Cancellation can interrupt Docker before a worker flushes its output.
		// Preserve the cause instead of reporting a secondary missing-file error.
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		checkCtx, stopCheck := context.WithTimeout(context.Background(), 5*time.Second)
		for _, name := range r.containers {
			if strings.HasPrefix(strings.TrimPrefix(name, r.id+"-"), "relay-") {
				b, inspectErr := r.docker(checkCtx, "inspect", "--format", "{{json .State}}", name)
				var state struct {
					OOMKilled bool
					ExitCode  int
				}
				if inspectErr == nil && json.Unmarshal(b, &state) == nil && (state.OOMKilled || state.ExitCode != 0) {
					res.Status = "FAIL"
					res.Reasons = append(res.Reasons, "relay_process_failed")
				}
			}
		}
		stopCheck()
		r.cleanup()
		// A cancelled generator can finish exporting while cleanup stops it.
		if len(res.Events) == 0 {
			var partial trafficResult
			if readJSON(filepath.Join(r.out, "traffic.json"), &partial) == nil {
				res.Start, res.Events = partial.Start, partial.Events
				res.Summary = summarize(res.Events, r.p.Nominal+r.p.Headroom)
			}
		}
		res.CleanupErrors = r.cleanupErrors
		if err != nil {
			res.Reasons = append(res.Reasons, err.Error())
			if res.Status == "PASS" {
				res.Status = "INCONCLUSIVE"
			}
		}
		if len(r.cleanupErrors) > 0 {
			res.Status = "FAIL"
		}
		if writeErr := writeJSON(filepath.Join(r.out, "metadata.json"), map[string]any{"revision": res.Revision, "dirty": res.Dirty, "imageId": res.Image, "goVersion": res.Toolchain, "host": res.Host}); writeErr != nil {
			if res.Status != "FAIL" {
				res.Status = "INCONCLUSIVE"
			}
			res.Reasons = append(res.Reasons, "metadata_export_failed")
		}
		if writeErr := writeJSON(filepath.Join(r.out, "result.json"), res); err == nil {
			err = writeErr
		}
	}()
	fmt.Println("Preparing pinned images and independently validated synthetic fixtures...")
	cfg, err := r.prepare(ctx, &res)
	if err != nil {
		return res, err
	}
	calibration, supportCalibration, err := r.monitoredTraffic(ctx, cfg, true)
	res.Calibration = calibration.Events
	res.CalibrationSupporting = supportCalibration
	if err != nil {
		return res, err
	}
	if err := calibrationCheck(res.Calibration, r.p, r.mode != "smoke"); err != nil {
		return res, err
	}
	if r.remoteDocker != "" {
		for _, s := range res.CalibrationSupporting {
			if s.Error != "" || s.Saturated {
				return res, errors.New("support calibration exhausted sender capacity")
			}
		}
	}
	if err := adminCall(ctx, cfg, "/reset", struct{}{}, nil); err != nil {
		return res, err
	}
	if r.externalRelay != "" && r.remoteDocker == "" {
		// Keep enrollment material private; callers install it on a disposable
		// native host. No production service or remote machine is modified here.
		if err := r.shapeExternalNetwork(ctx); err != nil {
			return res, err
		}
		if err := writeExternalSetup(r.out, cfg, r.work); err != nil {
			return res, err
		}
		fmt.Println("Waiting for the separately installed native service; private setup is in native-setup.")
		if err := waitControl(ctx, cfg, 5*time.Minute); err != nil {
			return res, err
		}
	} else {
		relay, err := r.startRelay(ctx, cfg)
		if err != nil {
			return res, err
		}
		if err := waitControl(ctx, cfg, 30*time.Second); err != nil {
			return res, err
		}
		shape := r.shapeNetwork
		if r.remoteDocker != "" {
			shape = func(ctx context.Context, _ string) error { return r.shapeExternalNetwork(ctx) }
		}
		if err := shape(ctx, relay); err != nil {
			return res, err
		}
		observer, err := r.startObserver(ctx, relay, "observer")
		if err != nil {
			return res, err
		}
		defer func() {
			if len(res.Samples) > 0 {
				return
			}
			res.Samples, _ = r.collectObserver(ctx, observer)
		}()
	}
	// Exclude setup polls from traffic accounting, while retaining idle polling.
	if err := adminCall(ctx, cfg, "/reset", struct{}{}, nil); err != nil {
		return res, err
	}
	fmt.Println("Running offered mixed traffic against the release executable...")
	traffic, monitored, trafficErr := r.monitoredTraffic(ctx, cfg, false)
	res.Start, res.Events = traffic.Start, traffic.Events
	res.Supporting = monitored
	if trafficErr != nil {
		return res, trafficErr
	}
	if err := adminCall(ctx, cfg, "/state", nil, &res.Cloud); err != nil {
		return res, err
	}
	// Capture nominal measurements before restarting with fault timeouts.
	for _, name := range r.containers {
		if strings.Contains(name, "-observer-") {
			res.Samples, err = r.collectObserver(ctx, name)
			if err != nil {
				return res, err
			}
		}
	}
	if r.mode == "qualify" || r.mode == "external-fault-check" || r.mode == "fault-check" {
		fmt.Println("Measuring report retries and DICOM fault recovery...")
		if r.externalRelay == "" {
			if err := r.prepareFaultRuntime(ctx); err != nil {
				return res, err
			}
		}
		faultObserver := ""
		if r.externalRelay == "" {
			for _, target := range r.containers {
				if strings.HasPrefix(strings.TrimPrefix(target, r.id+"-"), "relay-") {
					faultObserver, err = r.startObserver(ctx, target, "fault-observer")
					if err != nil {
						return res, err
					}
					break
				}
			}
			defer func() {
				if len(res.FaultSamples) == 0 && faultObserver != "" {
					res.FaultSamples, _ = r.collectObserver(ctx, faultObserver)
				}
			}()
		}
		stopFaultSupport := r.supportObservation(ctx)
		defer func() { res.FaultSupporting = stopFaultSupport() }()
		options := []string{"--user", workerUser(), "--mount", "type=bind,src=" + r.work + ",dst=/work,readonly", "--mount", "type=bind,src=" + r.out + ",dst=/out"}
		options = append(options, r.fixtureMount()...)
		name, err := r.start(ctx, "faults", options, "telrad-relay-perf:local", []string{"faults", "--out", "/out/faults.json"})
		if err != nil {
			return res, err
		}
		if err := r.waitContainer(ctx, name); err != nil {
			return res, err
		}
		if err := readJSON(filepath.Join(r.out, "faults.json"), &res.Faults); err != nil {
			return res, err
		}
		res.FaultSupporting = stopFaultSupport()
		if faultObserver != "" {
			res.FaultSamples, err = r.collectObserver(ctx, faultObserver)
			if err != nil {
				return res, err
			}
		}
	}
	evaluate(&res, r.cpus, r.memory)
	if r.externalRelay != "" && r.remoteDocker == "" {
		if res.Status != "FAIL" {
			res.Status = "INCONCLUSIVE"
		}
		res.Reasons = append(res.Reasons, "native_machine_and_lifecycle_evidence_required")
	}
	return res, nil
}

func makeTLS(dir, host string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	t := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Synthetic Relay performance"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), DNSNames: []string{"cloud", "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if host != "" {
		if ip := net.ParseIP(host); ip != nil {
			t.IPAddresses = append(t.IPAddresses, ip)
		} else {
			t.DNSNames = append(t.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, t, t, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "server-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600)
}

func initializeRelayState(cfg workerConfig, dir, state string) error {
	var config map[string]any
	// Keep packaged defaults as the configuration source of truth.
	if err := readJSON(filepath.Join(dir, "relay-defaults.json"), &config); err != nil {
		return err
	}
	config["pairingUrl"] = cfg.Origin + "/v1/relay/pairing-enrollments"
	config["controlUrl"] = cfg.Origin + "/v1/relay/control"
	config["dicomUrl"] = cfg.Origin + "/v1/relay/ingest/dicom"
	config["hl7Url"] = cfg.Origin + "/v1/relay/ingest/hl7"
	config["relayId"] = "perf-relay"
	config["credentialPath"] = "relay-credential.json"
	config["reportHost"] = "ris"
	if cfg.RIS != "" {
		host, _, err := net.SplitHostPort(cfg.RIS)
		if err != nil {
			return err
		}
		config["reportHost"] = host
	}
	config["reportPort"] = 2576
	config["hl7MaxBytes"] = cfg.Profile.HL7Limit
	if cfg.Mode == "faults" {
		config["dicomIdleTimeoutSeconds"] = 5
		config["dicomLifetimeSeconds"] = 15
	}
	if err := os.MkdirAll(state, 0700); err != nil {
		return err
	}
	for name, value := range map[string]any{"report-signing-key.json": syntheticReportKeyRecord(cfg), "relay.json": config, "relay-credential.json": map[string]any{"schemaVersion": 1, "credential": cfg.Credential}} {
		path := filepath.Join(state, name)
		if err := writeJSON(path, value); err != nil {
			return err
		}
		if err := os.Chown(path, 10001, 10001); err != nil {
			return err
		}
	}
	if err := os.Chmod(state, 0700); err != nil {
		return err
	}
	return os.Chown(state, 10001, 10001)
}

func waitControl(ctx context.Context, cfg workerConfig, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var s cloudSnapshot
		if adminCall(ctx, cfg, "/state", nil, &s) == nil && s.Polls > 0 {
			return nil
		}
		if !waitUntil(ctx, time.Now().Add(time.Second)) {
			return ctx.Err()
		}
	}
	return errors.New("Relay control readiness timeout")
}
