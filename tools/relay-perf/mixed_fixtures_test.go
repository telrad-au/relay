package main

import (
	"context"
	"encoding/xml"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/telrad-au/relay/internal/synthetic"
)

// Stand in for the external validator/decoder so corrupt outputs can be injected
// deterministically. Real Orthanc/dcm4che interoperability remains a separate check.
func TestMixedFixtureValidationRejectsChangedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake Docker executable")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\nwait) echo 0;;\nlogs) echo \"$PERF_VALIDATOR_OUTPUT\";;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, modality := range []string{"CT", "MR", "DX", "US"} {
		for _, outcome := range []string{"valid", "validator-failed", "changed-pixel", "changed-dimensions", "malformed-png", "changed-upload", "rejected-upload"} {
			t.Run(modality+"/"+outcome, func(t *testing.T) {
				t.Setenv("PERF_VALIDATOR_OUTPUT", " ... OK")
				if outcome == "validator-failed" {
					t.Setenv("PERF_VALIDATOR_OUTPUT", "FAILED: missing attribute")
				}
				spec := synthetic.ImageSpec{Modality: modality, Rows: 2, Columns: 3, Bits: 16, Samples: 1, Frames: 1}
				if modality == "US" {
					spec.Bits = 8
					spec.Samples = 3
				}
				work := t.TempDir()
				columns := spec.Columns
				if outcome == "changed-dimensions" {
					columns++
				}
				decoded := image.NewNRGBA(image.Rect(0, 0, columns, spec.Rows))
				for y := 0; y < spec.Rows; y++ {
					for x := 0; x < columns; x++ {
						v := byte((y*17 + x*31 + (y*x)%251) % 256)
						c := color.NRGBA{R: v, G: v, B: v, A: 255}
						if spec.Samples == 3 {
							c.G = v + 47
							c.B = v + 94
						}
						decoded.SetNRGBA(x, y, c)
					}
				}
				if outcome == "changed-pixel" {
					decoded.SetNRGBA(0, 0, color.NRGBA{R: 1, A: 255})
				}
				file, err := os.Create(filepath.Join(work, "mixed-pixels.png"))
				if err != nil {
					t.Fatal(err)
				}
				if err := png.Encode(file, decoded); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				if outcome == "malformed-png" {
					if err := os.WriteFile(filepath.Join(work, "mixed-pixels.png"), []byte("invalid"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				p := profile{Mix: []modalityMix{{Modality: modality, Variants: []studyVariant{{Name: "small", Groups: []imageGroup{{ImageSpec: spec, Instances: 2, PerSeries: 1}}}}}}}
				r := runner{root: work, work: work, out: work, id: "relay-perf-123", p: p}
				var stored []byte
				request := func(method, path string, data []byte) ([]byte, error) {
					if method == "POST" && path == "/instances" {
						stored = append([]byte(nil), data...)
						if outcome == "rejected-upload" {
							return []byte(`{}`), nil
						}
						return []byte(`{"ID":"synthetic"}`), nil
					}
					if method != "GET" || path != "/instances/synthetic/file" {
						t.Fatalf("unexpected request %s %s", method, path)
					}
					b := append([]byte(nil), stored...)
					if outcome == "changed-upload" {
						b[len(b)-1] ^= 1
					}
					return b, nil
				}
				fixtures, err := r.prepareMixedFixtures(context.Background(), request)
				if outcome != "valid" {
					if err == nil {
						t.Fatal("corrupt external output accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(fixtures) != 2 || fixtures[0].Instance == fixtures[1].Instance {
					t.Fatal("fixture identities lost")
				}
				for _, f := range fixtures {
					data, err := os.ReadFile(filepath.Join(work, f.File))
					if err != nil {
						t.Fatal(err)
					}
					if hash(data) != f.FileDigest || hash(data[f.Offset:]) != f.Digest || int64(len(data))-f.Offset != f.DatasetBytes {
						t.Fatal("manifest does not match written bytes")
					}
				}
				var iod struct {
					Elements []struct {
						Tag   string `xml:"tag,attr"`
						Value string `xml:"Value"`
					} `xml:"DataElement"`
				}
				if err := xml.Unmarshal([]byte(modalityIOD(spec)), &iod); err != nil {
					t.Fatal(err)
				}
				tags := map[string]string{}
				for _, e := range iod.Elements {
					tags[e.Tag] = e.Value
				}
				if tags["00080016"] != spec.Class() || tags["00080060"] != modality {
					t.Fatal("validator contract has wrong modality or SOP class")
				}
			})
		}
	}
}

func TestMultiFrameValidationContract(t *testing.T) {
	spec := synthetic.ImageSpec{Modality: "US", Rows: 2, Columns: 3, Bits: 8, Samples: 3, Frames: 3}
	iod := modalityIOD(spec)
	for _, tag := range []string{"00280008", "00280009", "00181063", "00280006"} {
		if !strings.Contains(iod, `tag="`+tag+`"`) {
			t.Fatalf("missing multi-frame/color contract %s", tag)
		}
	}
}
