package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

const orthancImage = "jodogne/orthanc-plugins:1.12.11@sha256:e7bffe0351cd391eacab8e78098e236efe6cafed987830e9b462b2050a0eae4a"
const dcm4cheImage = "dcm4che/dcm4che-tools:5.33.1@sha256:c8fbede4a6cf6047370ad21ce12fcc6be7ab013ff4996f1d032eb55239f870ed"

type fixture struct {
	Class        string `json:"sopClass,omitempty"`
	Modality     string `json:"modality,omitempty"`
	Size         string `json:"size,omitempty"`
	Template     int    `json:"template,omitempty"`
	Pixels       int64  `json:"pixelBytes,omitempty"`
	Frames       int    `json:"frames,omitempty"`
	Index        int    `json:"index"`
	File         string `json:"file"`
	Instance     string `json:"instance,omitempty"`
	Syntax       string `json:"syntax"`
	Offset       int64  `json:"datasetOffset"`
	DatasetBytes int64  `json:"datasetBytes"`
	Digest       string `json:"datasetSha256"`
	FileDigest   string `json:"fileSha256"`
}

func (r *runner) prepareFixtures(ctx context.Context) ([]fixture, error) {
	configuration := map[string]any{"Name": "Synthetic performance sender", "RemoteAccessAllowed": true, "AuthenticationEnabled": false, "DicomServerEnabled": false, "HttpServerPort": 8042, "StorageDirectory": "/var/lib/orthanc/db", "Plugins": []string{"/usr/local/share/orthanc/plugins/libOrthancGdcm.so"}}
	if err := writeJSON(filepath.Join(r.work, "orthanc.json"), configuration); err != nil {
		return nil, err
	}
	name, err := r.start(ctx, "orthanc", []string{"--publish", "127.0.0.1::8042", "--mount", "type=bind,src=" + filepath.Join(r.work, "orthanc.json") + ",dst=/etc/orthanc/orthanc.json,readonly"}, orthancImage, nil)
	if err != nil {
		return nil, err
	}
	port, err := r.docker(ctx, "port", name, "8042/tcp")
	if err != nil {
		return nil, err
	}
	address := "http://" + strings.TrimSpace(string(port))
	if err := waitHTTP(ctx, address+"/system"); err != nil {
		return nil, err
	}
	request := func(method, path string, body []byte) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, address+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, errors.New("Orthanc fixture operation failed")
		}
		return io.ReadAll(io.LimitReader(resp.Body, 128*1024*1024))
	}
	var fixtures []fixture
	if len(r.p.Mix) > 0 {
		fixtures, err = r.prepareMixedFixtures(ctx, request)
		if err != nil {
			return nil, err
		}
		if _, err = r.docker(ctx, "stop", "--time", "5", name); err != nil {
			return nil, err
		}
		return fixtures, nil
	}
	// One synthetic study contains distinct instances; later study transfers
	// intentionally replay this fixture set as fresh arrivals.
	for i := 0; i < r.p.Instances; i++ {
		uid := fmt.Sprintf("1.2.826.0.1.3680043.10.543.99.%d", i+1)
		data := synthetic.DICOM(uid, "1.2.826.0.1.3680043.10.543.98.1", r.p.Rows, r.p.Columns)
		response, err := request("POST", "/instances", data)
		if err != nil {
			return nil, err
		}
		var imported struct{ ID string }
		if json.Unmarshal(response, &imported) != nil || imported.ID == "" {
			return nil, errors.New("Orthanc did not import fixture")
		}
		if r.p.Syntax == synthetic.JPEGLosslessSV1 {
			body, _ := json.Marshal(map[string]any{"Force": true, "Keep": []string{"StudyInstanceUID", "SeriesInstanceUID", "SOPInstanceUID"}, "Transcode": r.p.Syntax})
			data, err = request("POST", "/instances/"+imported.ID+"/modify", body)
		} else {
			data, err = request("GET", "/instances/"+imported.ID+"/file", nil)
		}
		if err != nil {
			return nil, err
		}
		reader := bufio.NewReader(bytes.NewReader(data))
		meta, err := synthetic.Part10(reader)
		if err != nil {
			return nil, err
		}
		dataset, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		if meta.Instance != uid || meta.Syntax != r.p.Syntax || meta.Class != synthetic.SecondaryCapture {
			return nil, errors.New("Orthanc changed fixture identity")
		}
		f := fixture{Index: i, File: fmt.Sprintf("fixture-%03d.dcm", i), Instance: uid, Syntax: meta.Syntax, Offset: int64(len(data) - len(dataset)), DatasetBytes: int64(len(dataset)), Digest: hash(dataset), FileDigest: hash(data)}
		if err := os.WriteFile(filepath.Join(r.work, f.File), data, 0644); err != nil {
			return nil, err
		}
		fixtures = append(fixtures, f)
		if i == 0 {
			if err := r.validateFixture(ctx, f); err != nil {
				return nil, err
			}
		}
	}
	// Orthanc generation/decoding is complete before any measured workload.
	if _, err := r.docker(ctx, "stop", "--time", "5", name); err != nil {
		return nil, err
	}
	return fixtures, nil
}

func (f fixture) storageClass() string {
	if f.Class != "" {
		return f.Class
	}
	return synthetic.SecondaryCapture
}

func (r *runner) validateFixture(ctx context.Context, f fixture) error {
	profileData, err := os.ReadFile(filepath.Join(r.root, "cmd/telrad-relay/testdata/secondary-capture-iod.xml"))
	if err != nil {
		return err
	}
	// Preserve the reviewed IOD constraints, replacing only the fixture-specific
	// expected dimensions. The original interoperability profile stays 512x512.
	for keyword, value := range map[string]int{"Rows": r.p.Rows, "Columns": r.p.Columns} {
		tag := map[string]string{"Rows": "00280010", "Columns": "00280011"}[keyword]
		old := fmt.Sprintf("keyword=\"%s\" tag=\"%s\" vr=\"US\" type=\"1\" vm=\"1\">\n    <Value>512</Value>", keyword, tag)
		if !bytes.Contains(profileData, []byte(old)) {
			return errors.New("fixture IOD dimensions changed; review profile generation")
		}
		profileData = bytes.Replace(profileData, []byte(old), []byte(strings.Replace(old, "<Value>512</Value>", fmt.Sprintf("<Value>%d</Value>", value), 1)), 1)
	}
	if err := os.WriteFile(filepath.Join(r.work, "iod.xml"), profileData, 0644); err != nil {
		return err
	}
	args := []string{"--mount", "type=bind,src=" + r.work + ",dst=/work"}
	name, err := r.start(ctx, "validate", args, dcm4cheImage, []string{"dcmvalidate", "--iod", "/work/iod.xml", "/work/" + f.File})
	if err != nil {
		return err
	}
	if err := r.waitContainer(ctx, name); err != nil {
		return errors.New("dcm4che fixture validation failed")
	}
	logs, err := r.docker(ctx, "logs", name)
	if err != nil {
		return err
	}
	if !strings.Contains(string(logs), " ... OK") || strings.Contains(string(logs), "FAILED:") {
		return errors.New("dcm4che did not confirm IOD validation")
	}
	name, err = r.start(ctx, "decode", args, dcm4cheImage, []string{"dcm2jpg", "--noauto", "--noshape", "-F", "PNG", "/work/" + f.File, "/work/pixels.png"})
	if err != nil {
		return err
	}
	if err := r.waitContainer(ctx, name); err != nil {
		return errors.New("dcm4che fixture decoding failed")
	}
	imageFile, err := os.Open(filepath.Join(r.work, "pixels.png"))
	if err != nil {
		return err
	}
	defer imageFile.Close()
	decoded, err := png.Decode(imageFile)
	if err != nil {
		return err
	}
	if decoded.Bounds().Dx() != r.p.Columns || decoded.Bounds().Dy() != r.p.Rows {
		return errors.New("decoded dimensions changed")
	}
	for row := 0; row < r.p.Rows; row++ {
		for col := 0; col < r.p.Columns; col++ {
			red, green, blue, _ := decoded.At(col, row).RGBA()
			want := uint32(byte((row*17+col*31+(row*col)%251)%256)) * 257
			if red != want || green != want || blue != want {
				return errors.New("decoded synthetic pixels changed")
			}
		}
	}
	return nil
}

func waitHTTP(ctx context.Context, address string) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		req, err := http.NewRequestWithContext(requestCtx, "GET", address, nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		if !waitUntil(ctx, time.Now().Add(250*time.Millisecond)) {
			return ctx.Err()
		}
	}
	return errors.New("support readiness timeout")
}
