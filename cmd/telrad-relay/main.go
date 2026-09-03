package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	version       = "dev"
	distribution  = "native"
	proxyResolver = http.ProxyFromEnvironment
	clientFactory = newProtocolClients
)

const (
	currentConfigSchemaVersion = 3
	protocolVersion            = "1"
	maxCloudResponseBytes      = 64 * 1024
	serviceDrainTimeout        = 60 * time.Second
	healthySessionThreshold    = time.Minute
	pairingEnrollmentTimeout   = 5 * time.Minute
	deviceAuthorizationTimeout = 15 * time.Minute
	previewPairingURL          = "https://ingest.dev.app.telrad.com.au/v1/relay/pairing-enrollments"
	deviceAuthorizationPath    = "/v1/relay/device-authorizations"
)

type config struct {
	SchemaVersion              int    `json:"schemaVersion"`
	PairingURL                 string `json:"pairingUrl"`
	ControlURL                 string `json:"controlUrl"`
	DicomURL                   string `json:"dicomUrl"`
	HL7URL                     string `json:"hl7Url"`
	RelayID                    string `json:"relayId"`
	CredentialPath             string `json:"credentialPath"`
	ListenAddress              string `json:"listenAddress"`
	DicomPort                  int    `json:"dicomPort"`
	HL7Port                    int    `json:"hl7Port"`
	ReportHost                 string `json:"reportHost"`
	ReportPort                 int    `json:"reportPort"`
	MaxConnections             int    `json:"maxConnections,omitempty"`
	MaxDicomConnections        int    `json:"maxDicomConnections,omitempty"`
	MaxHL7Connections          int    `json:"maxHl7Connections,omitempty"`
	ConnectTimeoutSeconds      int    `json:"connectTimeoutSeconds,omitempty"`
	TLSHandshakeTimeoutSeconds int    `json:"tlsHandshakeTimeoutSeconds,omitempty"`
	ResponseHeaderTimeoutSecs  int    `json:"responseHeaderTimeoutSeconds,omitempty"`
	DicomIdleTimeoutSeconds    int    `json:"dicomIdleTimeoutSeconds,omitempty"`
	DicomLifetimeSeconds       int    `json:"dicomLifetimeSeconds,omitempty"`
	HL7IdleTimeoutSeconds      int    `json:"hl7IdleTimeoutSeconds"`
	HL7LifetimeSeconds         int    `json:"hl7LifetimeSeconds"`
	HL7MaxBytes                int64  `json:"hl7MaxBytes,omitempty"`
	UpdateManifestURL          string `json:"updateManifestUrl,omitempty"`
	UpdatePublicKey            string `json:"updatePublicKey,omitempty"`

	configPath               string
	credentialPathConfigured string
	dockerPairingToken       []byte
}

type pairingResponse struct {
	RelayID         string `json:"relayId"`
	Credential      string `json:"credential"`
	ProtocolVersion int    `json:"protocolVersion"`

	// Legacy endpoint fields are decoded only to enforce transition equality.
	// They never select Relay destinations.
	LegacyPairingURL string `json:"pairingUrl,omitempty"`
	LegacyControlURL string `json:"controlUrl,omitempty"`
	LegacyDicomURL   string `json:"dicomUrl,omitempty"`
	LegacyHL7URL     string `json:"hl7Url,omitempty"`
}

type deviceAuthorizationResponse struct {
	RequestID       string    `json:"requestId"`
	DeviceSecret    string    `json:"deviceSecret"`
	VerificationURI string    `json:"verificationUri"`
	ExpiresAt       time.Time `json:"expiresAt"`
	IntervalSeconds int       `json:"intervalSeconds"`
}

type deviceAuthorizationTokenResponse struct {
	PairingToken string `json:"pairingToken"`
}

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fatal(err)
	}
}

func execute(args []string) error {
	var dockerToken []byte
	if distribution == "docker" {
		dockerToken = captureDockerPairingToken()
		defer zeroBytes(dockerToken)
	}
	if len(args) > 0 && args[0] == "apply-update" {
		return applyStagedUpdate(args[1:])
	}
	flags := flag.NewFlagSet("telrad", flag.ContinueOnError)
	flags.Usage = printHelp
	configPath := flags.String("config", defaultConfigPath(), "path to relay configuration")
	migrationManifestURL := flags.String("migration-update-manifest-url", "", "installer-managed update manifest URL")
	migrationPublicKey := flags.String("migration-update-public-key", "", "installer-managed update public key")
	migrationPairingURL := flags.String("migration-pairing-url", "", "installer-managed Relay pairing URL")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	command := "auth"
	if flags.NArg() > 0 {
		command = flags.Arg(0)
	}
	if command == "help" {
		printHelp()
		return nil
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command == "migrate-config" {
		if shouldElevate(command, *configPath) {
			return elevateWithSudo(args)
		}
		if *configPath == defaultConfigPath() {
			if err := disableService(); err != nil {
				return err
			}
		}
		return migrateConfig(*configPath, *migrationPairingURL, *migrationManifestURL, *migrationPublicKey)
	}
	if *migrationPairingURL != "" || *migrationManifestURL != "" || *migrationPublicKey != "" {
		return errors.New("migration trust options are valid only with migrate-config")
	}
	if shouldElevate(command, *configPath) {
		return elevateWithSudo(args)
	}
	if command == "status" {
		if status, err := readRuntimeStatus(*configPath); err == nil {
			printRuntimeStatus(status)
		}
		return serviceStatus()
	}
	if command == "start" || command == "stop" || command == "restart" {
		return serviceAction(command)
	}
	if err := recoverPairingTransaction(*configPath); err != nil {
		return fmt.Errorf("recover pairing transaction: %w", err)
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	cfg.dockerPairingToken = dockerToken
	bootstrap, err := pairingBootstrapForRun(command, cfg)
	if err != nil {
		return err
	}
	validationCommand := command
	if bootstrap {
		validationCommand = "enroll"
	}
	if err := validateConfig(cfg, validationCommand); err != nil {
		return fmt.Errorf("invalid relay configuration: %w", err)
	}
	if distribution == "docker" && command == "run" && !bootstrap {
		zeroBytes(cfg.dockerPairingToken)
		cfg.dockerPairingToken = nil
	}
	switch command {
	case "auth":
		return authenticateAndStart(cfg, *configPath)
	case "enroll":
		return enrollForService(cfg, *configPath)
	case "rotate-credential":
		return rotateCredentialForService(cfg, *configPath)
	case "doctor":
		return doctor(cfg)
	case "update":
		return updateRelay(context.Background(), cfg, *configPath, flags.Args()[1:], clientFactory(cfg).updates)
	case "ready":
		return runtimeReady(cfg, *configPath)
	case "run":
		if bootstrap {
			if err := enrollForService(cfg, *configPath); err != nil {
				return err
			}
			cfg, err = loadConfig(*configPath)
			if err != nil {
				return err
			}
			if err := validateConfig(cfg, "run"); err != nil {
				return err
			}
		}
		return runPlatformService(cfg, *configPath)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func enroll(ctx context.Context, cfg *config, configPath string) error {
	if distribution == "docker" {
		token := cfg.dockerPairingToken
		cfg.dockerPairingToken = nil
		if len(token) == 0 {
			var err error
			token, err = consumeDockerPairingToken()
			if err != nil {
				return err
			}
		} else if len(token) < 32 || len(token) > 200 {
			zeroBytes(token)
			return errors.New("pairing token has an invalid length")
		}
		defer zeroBytes(token)
		return enrollWithPairingToken(ctx, cfg, configPath, token)
	}
	return enrollWithDeviceAuthorization(ctx, cfg, configPath)
}

func enrollWithDeviceAuthorization(ctx context.Context, cfg *config, configPath string) error {
	token, err := authorizeNativeDevice(ctx, cfg)
	if err != nil {
		return err
	}
	defer zeroBytes(token)
	return enrollWithPairingToken(ctx, cfg, configPath, token)
}

func authorizeNativeDevice(ctx context.Context, cfg *config) ([]byte, error) {
	authorizationURL, err := url.Parse(cfg.PairingURL)
	if err != nil {
		return nil, errors.New("create device authorization request")
	}
	authorizationURL.Path = deviceAuthorizationPath
	authorizationURL.RawPath = ""
	authorizationURL.RawQuery = ""
	authorizationURL.Fragment = ""
	hostname, _ := os.Hostname()
	body, err := json.Marshal(map[string]string{
		"hostname":     hostname,
		"platform":     relayPlatform(),
		"agentVersion": version,
	})
	if err != nil {
		return nil, errors.New("encode device authorization request")
	}
	authorizationCtx, cancel := context.WithTimeout(ctx, deviceAuthorizationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(authorizationCtx, http.MethodPost, authorizationURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("create device authorization request")
	}
	req.Header.Set("Content-Type", "application/json")
	clients := clientFactory(cfg)
	resp, err := clients.secure.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device authorization request failed: %w", safeNetworkError(err))
	}
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		return nil, fmt.Errorf("device authorization failed: http_%d", resp.StatusCode)
	}
	if !mediaTypeEquals(resp.Header.Get("Content-Type"), "application/json") {
		resp.Body.Close()
		return nil, errors.New("device authorization failed: invalid_content_type")
	}
	var authorization deviceAuthorizationResponse
	decodeErr := decodeBoundedJSON(resp.Body, maxCloudResponseBytes, &authorization)
	resp.Body.Close()
	if decodeErr != nil || !validDeviceAuthorization(authorization, time.Now()) {
		return nil, errors.New("device authorization failed: invalid_response")
	}

	fmt.Printf("\nApprove this relay in your browser:\n\n  %s\n\nWaiting for authorization...\n", authorization.VerificationURI)
	return pollDeviceAuthorization(authorizationCtx, clients.secure, authorizationURL, authorization)
}

func validDeviceAuthorization(authorization deviceAuthorizationResponse, now time.Time) bool {
	if !validURLSegment(authorization.RequestID) || !validURLSegment(authorization.DeviceSecret) || len(authorization.DeviceSecret) < 32 || !authorization.ExpiresAt.After(now) || authorization.IntervalSeconds < 1 || authorization.IntervalSeconds > 60 {
		return false
	}
	verificationURI, err := url.Parse(authorization.VerificationURI)
	return err == nil && verificationURI.Scheme == "https" && verificationURI.Host != "" && verificationURI.User == nil && verificationURI.RawQuery == "" && verificationURI.Fragment == "" && !strings.Contains(verificationURI.EscapedPath(), authorization.DeviceSecret)
}

func validURLSegment(value string) bool {
	if len(value) < 1 || len(value) > 200 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func pollDeviceAuthorization(ctx context.Context, client *http.Client, authorizationURL *url.URL, authorization deviceAuthorizationResponse) ([]byte, error) {
	pollURL := *authorizationURL
	pollURL.Path = strings.TrimRight(pollURL.Path, "/") + "/" + authorization.RequestID + "/token"
	interval := time.Duration(authorization.IntervalSeconds) * time.Second
	if interval < 3*time.Second {
		interval = 3 * time.Second
	}
	if interval > time.Minute {
		return nil, errors.New("device authorization failed: invalid_response")
	}
	for {
		if !time.Now().Before(authorization.ExpiresAt) {
			return nil, errors.New("device authorization expired; run enroll again")
		}
		body, err := json.Marshal(map[string]string{"deviceSecret": authorization.DeviceSecret})
		if err != nil {
			return nil, errors.New("encode device authorization poll")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, pollURL.String(), bytes.NewReader(body))
		if err != nil {
			zeroBytes(body)
			return nil, errors.New("create device authorization poll")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		zeroBytes(body)
		if err != nil {
			if ctx.Err() != nil {
				return nil, deviceAuthorizationContextError(ctx)
			}
			return nil, fmt.Errorf("device authorization poll failed: %w", safeNetworkError(err))
		}
		if resp.StatusCode == http.StatusOK {
			var result deviceAuthorizationTokenResponse
			valid := mediaTypeEquals(resp.Header.Get("Content-Type"), "application/json") && decodeBoundedJSON(resp.Body, maxCloudResponseBytes, &result) == nil
			resp.Body.Close()
			if !valid || len(result.PairingToken) < 32 || len(result.PairingToken) > 200 {
				return nil, errors.New("device authorization failed: invalid_response")
			}
			return []byte(result.PairingToken), nil
		}
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusAccepted, http.StatusTooManyRequests:
			delay := retryAfterDelay(resp.Header.Get("Retry-After"), time.Now())
			if delay < interval {
				delay = interval
			}
			remaining := time.Until(authorization.ExpiresAt)
			if delay > remaining {
				delay = remaining
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, deviceAuthorizationContextError(ctx)
			case <-timer.C:
			}
		case http.StatusForbidden:
			return nil, errors.New("device authorization was denied")
		case http.StatusGone:
			return nil, errors.New("device authorization expired; run enroll again")
		case http.StatusUnauthorized:
			return nil, errors.New("device authorization is invalid; run enroll again")
		default:
			return nil, fmt.Errorf("device authorization failed: http_%d", resp.StatusCode)
		}
	}
}

func deviceAuthorizationContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("device authorization expired; run enroll again")
	}
	return ctx.Err()
}

func enrollWithPairingToken(ctx context.Context, cfg *config, configPath string, pairingToken []byte) error {
	if len(pairingToken) < 32 || len(pairingToken) > 200 {
		return errors.New("pairing token has an invalid length")
	}
	hostname, _ := os.Hostname()
	body, err := json.Marshal(map[string]string{
		"pairingToken": string(pairingToken),
		"hostname":     hostname,
		"platform":     relayPlatform(),
		"agentVersion": version,
	})
	if err != nil {
		return errors.New("encode pairing request")
	}
	defer zeroBytes(body)
	clients := clientFactory(cfg)
	pairingCtx, cancel := context.WithTimeout(ctx, pairingEnrollmentTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pairingCtx, http.MethodPost, cfg.PairingURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("create pairing request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := clients.secure.Do(req)
	if err != nil {
		return fmt.Errorf("pairing request failed: %w", safeNetworkError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("pairing failed: http_%d", resp.StatusCode)
	}
	if !mediaTypeEquals(resp.Header.Get("Content-Type"), "application/json") {
		return errors.New("pairing failed: invalid_content_type")
	}
	var result pairingResponse
	if err := decodeBoundedJSON(resp.Body, maxCloudResponseBytes, &result); err != nil {
		return errors.New("pairing failed: invalid_response")
	}
	endpoints, err := validatePairingResponse(cfg.PairingURL, result)
	if err != nil {
		return err
	}
	cfg.RelayID = result.RelayID
	cfg.PairingURL = endpoints.PairingURL
	cfg.ControlURL = endpoints.ControlURL
	cfg.DicomURL = endpoints.DicomURL
	cfg.HL7URL = endpoints.HL7URL
	if err := commitPairing(configPath, cfg, credentialFile{SchemaVersion: credentialSchemaVersion, Credential: result.Credential}); err != nil {
		return fmt.Errorf("commit pairing: %w", err)
	}
	fmt.Printf("Paired relay %s\n", result.RelayID)
	return nil
}

func validatePairingResponse(expectedPairingURL string, result pairingResponse) (protocolEndpoints, error) {
	if result.ProtocolVersion != 1 || !validOpaqueID(result.RelayID) || !credentialPattern.MatchString(result.Credential) {
		return protocolEndpoints{}, errors.New("pairing failed: invalid_response")
	}
	endpoints, err := deriveProtocolEndpoints(expectedPairingURL)
	if err != nil {
		return protocolEndpoints{}, errors.New("pairing failed: invalid_endpoints")
	}
	legacy := []struct {
		actual   string
		expected string
	}{
		{result.LegacyPairingURL, endpoints.PairingURL},
		{result.LegacyControlURL, endpoints.ControlURL},
		{result.LegacyDicomURL, endpoints.DicomURL},
		{result.LegacyHL7URL, endpoints.HL7URL},
	}
	provided := 0
	for _, item := range legacy {
		if item.actual == "" {
			continue
		}
		provided++
		if item.actual != item.expected {
			return protocolEndpoints{}, errors.New("pairing failed: endpoint_mismatch")
		}
	}
	if provided != 0 && provided != len(legacy) {
		return protocolEndpoints{}, errors.New("pairing failed: invalid_response")
	}
	return endpoints, nil
}

func rotateCredential(ctx context.Context, cfg *config) error {
	provider, err := newCredentialProvider(cfg.CredentialPath, time.Now())
	if err != nil {
		return errors.New("stored credential is invalid")
	}
	rotationURL, _ := url.Parse(cfg.PairingURL)
	rotationURL.Path = "/v1/relay/credentials/rotate"
	rotationCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rotationCtx, http.MethodPost, rotationURL.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+provider.Current())
	resp, err := clientFactory(cfg).secure.Do(req)
	if err != nil {
		return fmt.Errorf("credential rotation failed: %w", safeNetworkError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("credential rotation failed: http_%d", resp.StatusCode)
	}
	var result struct {
		Credential              string    `json:"credential"`
		OldCredentialValidUntil time.Time `json:"oldCredentialValidUntil"`
	}
	if !mediaTypeEquals(resp.Header.Get("Content-Type"), "application/json") || decodeBoundedJSON(resp.Body, maxCloudResponseBytes, &result) != nil || !credentialPattern.MatchString(result.Credential) || !result.OldCredentialValidUntil.After(time.Now()) {
		return errors.New("credential rotation failed: invalid_response")
	}
	record := credentialFile{SchemaVersion: credentialSchemaVersion, Credential: result.Credential, PreviousCredential: provider.Current(), PreviousValidUntil: &result.OldCredentialValidUntil}
	if err := commitCredential(cfg.CredentialPath, record); err != nil {
		return err
	}
	fmt.Println("Relay credential rotated")
	return nil
}

func run(cfg *config, configPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWithContext(ctx, cfg, configPath)
}

func runWithContext(ctx context.Context, cfg *config, configPath string) error {
	provider, err := newCredentialProvider(cfg.CredentialPath, time.Now())
	if err != nil {
		return errors.New("stored credential is invalid")
	}
	clients := clientFactory(cfg)
	status := newRuntimeStatus(configPath)
	work := newWorkDrainer()
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	limits := newConnectionLimiter(cfg)
	dicomListener, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.DicomPort)))
	if err != nil {
		return fmt.Errorf("listen for DICOM: %w", err)
	}
	hl7Listener, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.HL7Port)))
	if err != nil {
		_ = dicomListener.Close()
		return fmt.Errorf("listen for HL7: %w", err)
	}
	defer dicomListener.Close()
	defer hl7Listener.Close()
	status.SetIngestReady(true)
	errCh := make(chan error, 8)
	credentialChanged := make(chan struct{}, 1)
	go watchCredentialFile(ctx, provider, credentialChanged, status)
	go superviseControl(ctx, cfg, configPath, clients.secure, provider, credentialChanged, work, status)
	go maintainRuntimeStatus(ctx, status, errCh)
	go acceptRelayConnections(ctx, "dicom", dicomListener, work, limits, func(conn net.Conn) { serveDICOM(workCtx, conn, cfg, clients.secure, provider, status) }, errCh)
	go acceptRelayConnections(ctx, "hl7", hl7Listener, work, limits, func(conn net.Conn) { serveHL7(workCtx, conn, cfg, clients.secure, provider, status) }, errCh)
	select {
	case <-ctx.Done():
		status.SetIngestReady(false)
		if err := drainRelayWork(work, dicomListener, hl7Listener); err != nil {
			slog.Warn("relay shutdown drain deadline expired", "activeWork", work.Active())
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func acceptRelayConnections(ctx context.Context, protocol string, listener net.Listener, work *workDrainer, limits *connectionLimiter, serve func(net.Conn), errCh chan<- error) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				errCh <- err
			}
			return
		}
		if !limits.Acquire(protocol) {
			limits.RecordOverload(protocol)
			_ = conn.Close()
			continue
		}
		if !work.Start() {
			limits.Release(protocol)
			_ = conn.Close()
			continue
		}
		go func() {
			defer work.Done()
			defer limits.Release(protocol)
			defer conn.Close()
			serve(conn)
		}()
	}
}

func watchCredentialFile(ctx context.Context, provider *credentialProvider, changed chan<- struct{}, status *runtimeStatusManager) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	expiryTimer := time.NewTimer(time.Hour)
	defer expiryTimer.Stop()
	resetCredentialExpiryTimer(expiryTimer, provider)
	for {
		select {
		case <-ctx.Done():
			return
		case <-expiryTimer.C:
			if err := provider.ExpirePrevious(time.Now()); err != nil {
				status.SetCredentialFileAttention(true)
			} else {
				status.SetCredentialFileAttention(false)
			}
			resetCredentialExpiryTimer(expiryTimer, provider)
		case <-ticker.C:
			adopted, err := provider.Reload(time.Now())
			if err != nil {
				status.SetCredentialFileAttention(true)
				resetCredentialExpiryTimer(expiryTimer, provider)
				continue
			}
			status.SetCredentialFileAttention(false)
			resetCredentialExpiryTimer(expiryTimer, provider)
			if adopted {
				status.CredentialAdopted()
				select {
				case changed <- struct{}{}:
				default:
				}
			}
		}
	}
}

func resetCredentialExpiryTimer(timer *time.Timer, provider *credentialProvider) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	delay := time.Hour
	if deadline, ok := provider.PreviousDeadline(); ok {
		delay = time.Until(deadline)
		if delay < 0 {
			delay = 0
		}
	}
	timer.Reset(delay)
}

func drainRelayWork(work *workDrainer, listeners ...net.Listener) error {
	drained := work.BeginDrain()
	for _, listener := range listeners {
		_ = listener.Close()
	}
	timer := time.NewTimer(serviceDrainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
		return nil
	case <-timer.C:
		return errors.New("active relay work did not drain before the deadline")
	}
}

func doctor(cfg *config) error {
	if _, err := readCredentialFile(cfg.CredentialPath, time.Now()); err != nil {
		return errors.New("stored credential is invalid")
	}
	fmt.Printf("configuration and credential ok; DICOM %s:%d, HL7 %s:%d, reports %s:%d\n", cfg.ListenAddress, cfg.DicomPort, cfg.ListenAddress, cfg.HL7Port, cfg.ReportHost, cfg.ReportPort)
	return nil
}

func runtimeReady(cfg *config, configPath string) error {
	if _, err := readCredentialFile(cfg.CredentialPath, time.Now()); err != nil {
		return errors.New("stored credential is invalid")
	}
	if err := checkRuntimeReady(configPath, time.Now()); err != nil {
		return err
	}
	fmt.Println("relay ready")
	return nil
}

func relayIsEnrolled(cfg *config) bool {
	if cfg.RelayID == "" || cfg.ControlURL == "" || cfg.DicomURL == "" || cfg.HL7URL == "" {
		return false
	}
	_, err := readCredentialFile(cfg.CredentialPath, time.Now())
	return err == nil
}

func pairingBootstrapForRun(command string, cfg *config) (bool, error) {
	if command != "run" || relayIsEnrolled(cfg) {
		return false, nil
	}
	if distribution != "docker" {
		return false, nil
	}
	return true, nil
}

func consumeDockerPairingToken() ([]byte, error) {
	token := captureDockerPairingToken()
	if len(token) == 0 {
		return nil, errors.New("TELRAD_RELAY_PAIRING_TOKEN is required for Docker enrollment")
	}
	if len(token) < 32 || len(token) > 200 {
		zeroBytes(token)
		return nil, errors.New("pairing token has an invalid length")
	}
	return token, nil
}

func captureDockerPairingToken() []byte {
	value, ok := os.LookupEnv("TELRAD_RELAY_PAIRING_TOKEN")
	_ = os.Unsetenv("TELRAD_RELAY_PAIRING_TOKEN")
	if !ok || value == "" {
		return nil
	}
	return []byte(value)
}

func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := defaultConfig()
	cfg.SchemaVersion = 0
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decode relay configuration: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode relay configuration: multiple JSON values are not allowed")
	}
	applyConnectionDefaults(cfg)
	if value := strings.TrimSpace(os.Getenv("TELRAD_RELAY_PAIRING_URL")); value != "" {
		cfg.PairingURL = value
	}
	if value := strings.TrimSpace(os.Getenv("TELRAD_RELAY_REPORT_DESTINATION_HOST")); value != "" {
		cfg.ReportHost = value
	}
	if value := strings.TrimSpace(os.Getenv("TELRAD_RELAY_REPORT_DESTINATION_PORT")); value != "" {
		port, err := parsePort(value)
		if err != nil {
			return nil, fmt.Errorf("TELRAD_RELAY_REPORT_DESTINATION_PORT: %w", err)
		}
		cfg.ReportPort = port
	}
	cfg.configPath = path
	cfg.credentialPathConfigured = cfg.CredentialPath
	cfg.CredentialPath = absolute(filepath.Dir(path), cfg.CredentialPath)
	return cfg, nil
}

func defaultConfig() *config {
	return &config{
		SchemaVersion: currentConfigSchemaVersion, PairingURL: previewPairingURL, CredentialPath: "relay-credential.json",
		ListenAddress: "0.0.0.0", DicomPort: 11112, HL7Port: 2575, ReportHost: "127.0.0.1", ReportPort: 2576,
		MaxConnections: 256, MaxDicomConnections: 128, MaxHL7Connections: 128,
		ConnectTimeoutSeconds: 10, TLSHandshakeTimeoutSeconds: 15, ResponseHeaderTimeoutSecs: 30,
		DicomIdleTimeoutSeconds: 300, DicomLifetimeSeconds: 7200, HL7MaxBytes: 1024 * 1024,
	}
}

func applyConnectionDefaults(cfg *config) {
	defaults := []struct {
		value    *int
		fallback int
	}{
		{&cfg.MaxConnections, 256}, {&cfg.MaxDicomConnections, 128}, {&cfg.MaxHL7Connections, 128},
		{&cfg.ConnectTimeoutSeconds, 10}, {&cfg.TLSHandshakeTimeoutSeconds, 15}, {&cfg.ResponseHeaderTimeoutSecs, 30},
		{&cfg.DicomIdleTimeoutSeconds, 300}, {&cfg.DicomLifetimeSeconds, 7200},
	}
	for _, setting := range defaults {
		if *setting.value == 0 {
			*setting.value = setting.fallback
		}
	}
	if cfg.HL7MaxBytes == 0 {
		cfg.HL7MaxBytes = 1024 * 1024
	}
}

func validateConfig(cfg *config, command string) error {
	if cfg.SchemaVersion != currentConfigSchemaVersion {
		return fmt.Errorf("schemaVersion %d is unsupported; expected %d", cfg.SchemaVersion, currentConfigSchemaVersion)
	}
	if net.ParseIP(strings.TrimSpace(cfg.ListenAddress)) == nil {
		return errors.New("listenAddress must be an explicit IPv4 or IPv6 address")
	}
	for name, port := range map[string]int{"dicomPort": cfg.DicomPort, "hl7Port": cfg.HL7Port, "reportPort": cfg.ReportPort} {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%s must be an integer from 1 to 65535", name)
		}
	}
	if cfg.DicomPort == cfg.HL7Port {
		return errors.New("dicomPort and hl7Port must be different")
	}
	for name, value := range map[string]int{"maxConnections": cfg.MaxConnections, "maxDicomConnections": cfg.MaxDicomConnections, "maxHl7Connections": cfg.MaxHL7Connections, "connectTimeoutSeconds": cfg.ConnectTimeoutSeconds, "tlsHandshakeTimeoutSeconds": cfg.TLSHandshakeTimeoutSeconds, "responseHeaderTimeoutSeconds": cfg.ResponseHeaderTimeoutSecs, "dicomIdleTimeoutSeconds": cfg.DicomIdleTimeoutSeconds, "dicomLifetimeSeconds": cfg.DicomLifetimeSeconds} {
		if value < 1 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if cfg.HL7IdleTimeoutSeconds < 0 || cfg.HL7LifetimeSeconds < 0 {
		return errors.New("HL7 timeouts must be zero or positive")
	}
	if cfg.HL7MaxBytes < 1 || cfg.HL7MaxBytes > 8*1024*1024 {
		return errors.New("hl7MaxBytes must be from 1 byte through 8 MiB")
	}
	if cfg.MaxDicomConnections > cfg.MaxConnections || cfg.MaxHL7Connections > cfg.MaxConnections {
		return errors.New("per-protocol connection limits cannot exceed maxConnections")
	}
	if cfg.DicomIdleTimeoutSeconds > cfg.DicomLifetimeSeconds || (cfg.HL7IdleTimeoutSeconds > 0 && cfg.HL7LifetimeSeconds > 0 && cfg.HL7IdleTimeoutSeconds > cfg.HL7LifetimeSeconds) {
		return errors.New("enabled protocol idle timeouts cannot exceed their total lifetime")
	}
	if strings.TrimSpace(cfg.ReportHost) == "" || strings.TrimSpace(cfg.CredentialPath) == "" {
		return errors.New("reportHost and credentialPath are required")
	}
	if cfg.configPath != "" && filepath.Clean(cfg.configPath) == filepath.Clean(cfg.CredentialPath) {
		return errors.New("credentialPath must differ from the configuration path")
	}
	paired := command == "run" || command == "doctor" || command == "ready" || command == "rotate-credential"
	if err := validateProtocolEndpoints(cfg, paired); err != nil {
		return err
	}
	if paired && !validOpaqueID(cfg.RelayID) {
		return errors.New("relayId must be a bounded opaque identifier")
	}
	if (cfg.UpdateManifestURL == "") != (cfg.UpdatePublicKey == "") {
		return errors.New("updateManifestUrl and updatePublicKey must be configured together")
	}
	if cfg.UpdateManifestURL != "" {
		if err := validateEndpointURL("updateManifestUrl", cfg.UpdateManifestURL, "https", ""); err != nil {
			return err
		}
		if _, _, err := decodeUpdateSignature(cfg.UpdatePublicKey, base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))); err != nil {
			return fmt.Errorf("updatePublicKey: %w", err)
		}
	}
	return nil
}

func validateProtocolEndpoints(cfg *config, requirePaired bool) error {
	values := []struct{ name, value, scheme, path string }{
		{"pairingUrl", cfg.PairingURL, "https", "/v1/relay/pairing-enrollments"},
	}
	if requirePaired {
		values = append(values,
			struct{ name, value, scheme, path string }{"controlUrl", cfg.ControlURL, "wss", "/v1/relay/control"},
			struct{ name, value, scheme, path string }{"dicomUrl", cfg.DicomURL, "https", "/v1/relay/ingest/dicom"},
			struct{ name, value, scheme, path string }{"hl7Url", cfg.HL7URL, "https", "/v1/relay/ingest/hl7"},
		)
	}
	var origin string
	for _, item := range values {
		if err := validateEndpointURL(item.name, item.value, item.scheme, item.path); err != nil {
			return err
		}
		parsed, _ := url.Parse(item.value)
		candidate := strings.ToLower(parsed.Host)
		if origin == "" {
			origin = candidate
		} else if candidate != origin {
			return errors.New("all protocol URLs must use a common origin")
		}
	}
	return nil
}

func validateEndpointURL(name, value, expectedScheme, expectedPath string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != expectedScheme || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s must be an absolute %s URL without credentials", name, expectedScheme)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain a query or fragment", name)
	}
	if expectedPath != "" && parsed.EscapedPath() != expectedPath {
		return fmt.Errorf("%s must use path %s", name, expectedPath)
	}
	return nil
}

func validateSecureURL(name, value, expectedScheme string) error {
	return validateEndpointURL(name, value, expectedScheme, "")
}

func migrateConfig(path, pairingURL, updateURL, updateKey string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var old struct {
		SchemaVersion              int    `json:"schemaVersion"`
		EnrollmentURL              string `json:"enrollmentUrl"`
		ListenAddress              string `json:"listenAddress"`
		DicomPort                  int    `json:"dicomPort"`
		HL7Port                    int    `json:"hl7Port"`
		ReportHost                 string `json:"reportHost"`
		ReportPort                 int    `json:"reportPort"`
		UpdateManifestURL          string `json:"updateManifestUrl"`
		UpdatePublicKey            string `json:"updatePublicKey"`
		MaxConnections             int    `json:"maxConnections"`
		MaxDicomConnections        int    `json:"maxDicomConnections"`
		MaxHL7Connections          int    `json:"maxHl7Connections"`
		ConnectTimeoutSeconds      int    `json:"connectTimeoutSeconds"`
		TLSHandshakeTimeoutSeconds int    `json:"tlsHandshakeTimeoutSeconds"`
		DicomIdleTimeoutSeconds    int    `json:"dicomIdleTimeoutSeconds"`
		DicomLifetimeSeconds       int    `json:"dicomLifetimeSeconds"`
		HL7IdleTimeoutSeconds      int    `json:"hl7IdleTimeoutSeconds"`
		HL7LifetimeSeconds         int    `json:"hl7LifetimeSeconds"`
		CertificatePath            string `json:"certificatePath"`
		PrivateKeyPath             string `json:"privateKeyPath"`
		CACertificatePath          string `json:"caCertificatePath"`
	}
	if err := json.Unmarshal(data, &old); err != nil || old.SchemaVersion != 2 {
		return errors.New("migrate-config requires a schema v2 configuration")
	}
	cfg := defaultConfig()
	cfg.configPath = path
	if pairingURL != "" {
		cfg.PairingURL = pairingURL
	}
	cfg.ListenAddress, cfg.DicomPort, cfg.HL7Port = old.ListenAddress, old.DicomPort, old.HL7Port
	cfg.ReportHost, cfg.ReportPort = old.ReportHost, old.ReportPort
	cfg.MaxConnections, cfg.MaxDicomConnections, cfg.MaxHL7Connections = old.MaxConnections, old.MaxDicomConnections, old.MaxHL7Connections
	cfg.ConnectTimeoutSeconds, cfg.TLSHandshakeTimeoutSeconds = old.ConnectTimeoutSeconds, old.TLSHandshakeTimeoutSeconds
	cfg.DicomIdleTimeoutSeconds, cfg.DicomLifetimeSeconds = old.DicomIdleTimeoutSeconds, old.DicomLifetimeSeconds
	cfg.HL7IdleTimeoutSeconds, cfg.HL7LifetimeSeconds = old.HL7IdleTimeoutSeconds, old.HL7LifetimeSeconds
	cfg.UpdateManifestURL, cfg.UpdatePublicKey = old.UpdateManifestURL, old.UpdatePublicKey
	applyConnectionDefaults(cfg)
	if (updateURL == "") != (updateKey == "") {
		return errors.New("migration update manifest URL and public key must be provided together")
	}
	if updateURL != "" {
		cfg.UpdateManifestURL, cfg.UpdatePublicKey = updateURL, updateKey
	}
	if err := validateConfig(cfg, "enroll"); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(cfg, "", "  ")
	if err := secureWrite(path, append(encoded, '\n')); err != nil {
		return err
	}
	legacyPaths := []string{old.CertificatePath, old.PrivateKeyPath, old.CACertificatePath}
	for _, base := range []string{old.CertificatePath, old.PrivateKeyPath, old.CACertificatePath} {
		if base == "" {
			continue
		}
		for _, suffix := range []string{".next", ".previous"} {
			legacyPaths = append(legacyPaths, base+suffix)
		}
	}
	if old.PrivateKeyPath != "" {
		for _, suffix := range []string{".pairing", ".pairing.csr", ".pairing-key.pem", ".pairing.csr.pem", ".transaction.json"} {
			legacyPaths = append(legacyPaths, old.PrivateKeyPath+suffix)
		}
	}
	var cleanupError error
	for _, legacy := range legacyPaths {
		if legacy != "" {
			if err := os.Remove(absolute(filepath.Dir(path), legacy)); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupError = errors.Join(cleanupError, err)
			}
		}
	}
	if cleanupError != nil {
		return fmt.Errorf("remove obsolete credential material: %w", cleanupError)
	}
	fmt.Println("Relay configuration migrated to schema v3; re-pairing is required before the service can start")
	return nil
}

func nextReconnectBackoff(current time.Duration, established bool, sessionDuration time.Duration) time.Duration {
	if established && sessionDuration >= healthySessionThreshold {
		return time.Second
	}
	if current < time.Second {
		return time.Second
	}
	current *= 2
	if current > 30*time.Second {
		return 30 * time.Second
	}
	return current
}

func jitterReconnectDelay(base time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	return time.Duration(float64(base) * (0.75 + mathrand.Float64()*0.5))
}

func randomHL7IdempotencyKey() (string, error) {
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func zeroBytes(data []byte) {
	for i := range data {
		data[i] = 0
	}
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("must be an integer from 1 to 65535")
	}
	return port, nil
}

func relayPlatform() string {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if distribution != "" && distribution != "native" {
		return distribution + "/" + platform
	}
	return platform
}

func absolute(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func secureWrite(path string, data []byte) error { return atomicWriteFile(path, data, 0600) }

func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "Telrad", "Relay", "relay.json")
	}
	return "/etc/telrad-relay/relay.json"
}

func defaultUpdateTrustPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramFiles"), "Telrad Relay", "update-trust.json")
	}
	return "/usr/local/lib/telrad-relay/update-trust.json"
}

func fatal(err error) {
	slog.Error("relay failed", "error", err)
	os.Exit(1)
}
