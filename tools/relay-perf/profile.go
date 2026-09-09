package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

//go:embed profiles/*.json
var profiles embed.FS

type profile struct {
	StudyRate        float64       `json:"studiesPerSecond,omitempty"`
	Mix              []modalityMix `json:"modalityMix,omitempty"`
	TransferMargin   float64       `json:"transferMargin,omitempty"`
	Name             string        `json:"name"`
	DICOMRate        float64       `json:"dicomPerSecond"`
	HL7Rate          float64       `json:"hl7PerSecond"`
	ReportRate       float64       `json:"reportsPerSecond"`
	Rows             int           `json:"rows"`
	Columns          int           `json:"columns"`
	Instances        int           `json:"instancesPerStudy"`
	DICOMConnections int           `json:"dicomConnections"`
	HL7Connections   int           `json:"hl7Connections"`
	IdleHL7          int           `json:"idleHl7Connections"`
	HL7Bytes         int           `json:"hl7Bytes"`
	ReportBytes      int           `json:"reportBytes"`
	HL7Limit         int           `json:"hl7Limit"`
	PDU              int           `json:"pduBytes"`
	Syntax           string        `json:"syntax"`
	Bandwidth        float64       `json:"bandwidthMbit"`
	RTT              float64       `json:"cloudRttMillis"`
	Receipt          float64       `json:"receiptMillis"`
	BodyRate         int64         `json:"bodyBytesPerSecond"`
	ACK              float64       `json:"risAckMillis"`
	Idle             float64       `json:"idleSeconds"`
	Nominal          float64       `json:"nominalSeconds"`
	Headroom         float64       `json:"headroomSeconds"`
	Recovery         float64       `json:"recoverySeconds"`
	Factor           float64       `json:"headroomFactor"`
	Repetitions      int           `json:"repetitions"`
	DICOMP99         float64       `json:"dicomP99Millis"`
	HL7P99           float64       `json:"hl7P99Millis"`
	ReportP99        float64       `json:"reportP99Millis"`
	StudyMax         float64       `json:"studyMaxMillis"`
}

func loadProfile(name string) (profile, error) {
	b, err := profiles.ReadFile("profiles/" + name + ".json")
	if err != nil {
		b, err = os.ReadFile(name)
	}
	if err != nil {
		return profile{}, errors.New("profile is unavailable")
	}
	var p profile
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return p, fmt.Errorf("invalid profile: %w", err)
	}
	return p, p.validate()
}

func (p profile) validate() error {
	if len(p.Mix) > 0 {
		if err := p.validateMix(); err != nil {
			return err
		}
	}
	if p.Name == "" || len(p.Mix) == 0 && (p.Rows < 1 || p.Rows > 8192 || p.Columns < 1 || p.Columns > 8192 || p.Instances < 1 || p.Instances > 1000) {
		return errors.New("invalid fixture dimensions or study size")
	}
	if p.DICOMConnections < 1 || p.DICOMConnections > 128 || p.HL7Connections < 1 || p.HL7Connections+p.IdleHL7 > 128 || p.IdleHL7 < 0 {
		return errors.New("profile exceeds configured connection slots")
	}
	if p.HL7Limit < 1024 || p.HL7Limit > 8*1024*1024 || p.HL7Bytes < 512 || p.HL7Bytes > p.HL7Limit || p.ReportBytes < 512 || p.ReportBytes > 32*1024 {
		return errors.New("invalid HL7 size or limit")
	}
	if p.PDU != 16*1024 && p.PDU != 64*1024 {
		return errors.New("PDU must be 16 or 64 KiB")
	}
	if p.Syntax != synthetic.ExplicitVRLittleEndian && p.Syntax != synthetic.JPEGLosslessSV1 {
		return errors.New("unsupported qualification syntax")
	}
	rate := p.DICOMRate
	if len(p.Mix) > 0 {
		rate = p.StudyRate
	}
	for _, v := range []float64{rate, p.HL7Rate, p.ReportRate, p.Nominal, p.Recovery, p.Factor, p.Bandwidth, p.DICOMP99, p.HL7P99, p.ReportP99, p.StudyMax} {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("profile rates, durations and bounds must be positive and finite")
		}
	}
	for _, v := range []float64{p.Idle, p.Headroom, p.RTT, p.Receipt, p.ACK} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("invalid delay")
		}
	}
	if p.BodyRate < 0 || p.Factor < 1.25 || p.Repetitions < 1 || p.Repetitions > 10 || p.Idle+p.Nominal+p.Headroom+p.Recovery > 86400 {
		return errors.New("invalid qualification duration or headroom")
	}
	return nil
}

func seconds(n float64) time.Duration  { return time.Duration(n * float64(time.Second)) }
func millis(n float64) time.Duration   { return time.Duration(n * float64(time.Millisecond)) }
func (p profile) total() time.Duration { return seconds(p.Idle + p.Nominal + p.Headroom + p.Recovery) }

func memoryBytes(s string) (int64, error) {
	var n int64
	for _, suffix := range []string{"MiB", "GiB"} {
		if strings.HasSuffix(s, suffix) {
			var err error
			n, err = strconv.ParseInt(strings.TrimSuffix(s, suffix), 10, 64)
			if err != nil {
				return 0, errors.New("memory requires an integer MiB or GiB value")
			}
			if suffix == "GiB" {
				if n < 1 || n > 64 {
					return 0, errors.New("memory must be at most 64 GiB")
				}
				n *= 1024
			}
			if n < 6 || n > 65536 {
				return 0, errors.New("memory must be between 6 MiB and 64 GiB")
			}
			return n * 1024 * 1024, nil
		}
	}
	return 0, errors.New("memory requires MiB or GiB units")
}

func experimentProfile(p profile, name string) (profile, error) {
	if len(p.Mix) > 0 {
		return p, errors.New("legacy object experiments require --profile clinic-v1; use screen for mixed studies")
	}
	p.Name += "-" + name
	switch name {
	case "small":
		p.Rows, p.Columns = 128, 256
	case "large":
		p.Rows, p.Columns, p.Instances = 8192, 8192, 1
		p.DICOMRate = 1.0 / 30
		p.DICOMP99, p.StudyMax = 30000, 60000
	case "jpeg":
		p.Syntax = synthetic.JPEGLosslessSV1
	case "max-connections":
		p.DICOMConnections, p.HL7Connections, p.IdleHL7 = 128, 128, 0
		p.Rows, p.Columns, p.Instances = 128, 256, 128
		p.DICOMRate, p.HL7Rate = 128, 128
	case "max-hl7":
		p.HL7Limit, p.HL7Bytes = 8*1024*1024, 8*1024*1024
		p.HL7Connections, p.IdleHL7 = 4, 16
		p.HL7Rate = .125
		p.HL7P99 = 60000
	case "backpressure":
		p.BodyRate = 256 * 1024
	case "associations-1":
		p.DICOMConnections = 1
	case "associations-4":
		p.DICOMConnections = 4
	case "associations-8":
		p.DICOMConnections = 8
	case "associations-32":
		p.DICOMConnections = 32
	default:
		return p, errors.New("unknown experiment")
	}
	return p, p.validate()
}
