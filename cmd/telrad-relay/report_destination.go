package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/telrad-au/relay/internal/retrieval"
)

// Telrad sends the clinic RIS report receiver to Relays that advertise the
// reportDestination capability. A locally configured reportHost/reportPort is a
// pin: Telrad can confirm it but never redirect it. Without a pin, Telrad's
// destination is used only inside the allowed networks, so a compromised cloud
// cannot aim clinical reports at arbitrary hosts.

var defaultReportDestinationNetworks = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "100.64.0.0/10",
}

var errNoReportDestination = fmt.Errorf("%w: no report destination", retrieval.ErrPolicy)

type reportDestination struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// reportDestinationHolder keeps the latest destination Telrad sent. It is shared
// by a loaded configuration and every copy or reload of it.
type reportDestinationHolder struct {
	mu sync.RWMutex
	// spoken is false until a ready message carried the field (older servers never do).
	spoken bool
	value  *reportDestination
}

func (h *reportDestinationHolder) current() (bool, *reportDestination) {
	if h == nil {
		return false, nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.spoken, h.value
}

func (h *reportDestinationHolder) set(spoken bool, value *reportDestination) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spoken, h.value = spoken, value
}

// applyReadyReportDestination records the destination from a ready message. An
// absent field means an older server; null means the company has no receiver.
func applyReadyReportDestination(cfg *config, raw json.RawMessage) {
	if cfg.reportDestinations == nil {
		return
	}
	if len(raw) == 0 {
		cfg.reportDestinations.set(false, nil)
		return
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		cfg.reportDestinations.set(true, nil)
		return
	}
	var value reportDestination
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || validateTelradReportDestination(cfg, value) != nil {
		slog.Error("rejected report destination from Telrad", "reason", "outside allowed networks or invalid")
		cfg.reportDestinations.set(true, nil)
		return
	}
	cfg.reportDestinations.set(true, &value)
}

func validateTelradReportDestination(cfg *config, value reportDestination) error {
	address, err := netip.ParseAddr(value.Host)
	if err != nil || !address.Is4() || address.String() != value.Host {
		return errors.New("report destination must be a canonical IPv4 address")
	}
	if value.Port < 1 || value.Port > 65535 {
		return errors.New("report destination port must be from 1 to 65535")
	}
	networks, err := reportDestinationNetworks(cfg)
	if err != nil {
		return err
	}
	for _, network := range networks {
		if network.Contains(address) {
			return nil
		}
	}
	return errors.New("report destination is outside the allowed networks")
}

func reportDestinationNetworks(cfg *config) ([]netip.Prefix, error) {
	values := cfg.ReportDestinationAllowedCIDRs
	if len(values) == 0 {
		values = defaultReportDestinationNetworks
	}
	networks := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		network, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil || !network.Addr().Is4() || network.Masked() != network {
			return nil, fmt.Errorf("reportDestinationAllowedCidrs contains an invalid IPv4 network %q", value)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

func reportDestinationPin(cfg *config) (reportDestination, bool) {
	if strings.TrimSpace(cfg.ReportHost) == "" {
		return reportDestination{}, false
	}
	return reportDestination{Host: cfg.ReportHost, Port: cfg.ReportPort}, true
}

// effectiveReportDestination is where report permits are signed for and where
// authorized reports are sent. A destination change invalidates earlier permits.
func effectiveReportDestination(cfg *config) (reportDestination, error) {
	pin, pinned := reportDestinationPin(cfg)
	spoken, telrad := cfg.reportDestinations.current()
	switch {
	case pinned && spoken && telrad != nil && *telrad != pin:
		return reportDestination{}, fmt.Errorf("%w: Telrad report destination differs from the local pin", retrieval.ErrPolicy)
	case pinned:
		return pin, nil
	case telrad != nil:
		return *telrad, nil
	default:
		return reportDestination{}, errNoReportDestination
	}
}

func validateReportDestinationConfig(cfg *config) error {
	host := strings.TrimSpace(cfg.ReportHost)
	if host == "" && cfg.ReportPort != 0 {
		return errors.New("reportPort requires reportHost")
	}
	if host != "" && (cfg.ReportPort < 1 || cfg.ReportPort > 65535) {
		return errors.New("reportPort must be an integer from 1 to 65535")
	}
	if host != "" && net.ParseIP(host) == nil && strings.ContainsAny(host, " /") {
		return errors.New("reportHost must be a host name or IP address")
	}
	_, err := reportDestinationNetworks(cfg)
	return err
}

func applyReportDestinationEnvironment(cfg *config) error {
	if value := strings.TrimSpace(os.Getenv("TELRAD_RELAY_REPORT_DESTINATION_ALLOWED_CIDRS")); value != "" {
		cfg.ReportDestinationAllowedCIDRs = nil
		for _, network := range strings.Split(value, ",") {
			if network = strings.TrimSpace(network); network != "" {
				cfg.ReportDestinationAllowedCIDRs = append(cfg.ReportDestinationAllowedCIDRs, network)
			}
		}
	}
	return nil
}

func describeReportDestination(cfg *config) string {
	if pin, ok := reportDestinationPin(cfg); ok {
		return fmt.Sprintf("%s:%d (pinned)", pin.Host, pin.Port)
	}
	return "from Telrad"
}
