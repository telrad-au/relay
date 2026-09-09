package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/telrad-au/relay/internal/synthetic"
)

func (r *runner) prepareMixedFixtures(ctx context.Context, request func(string, string, []byte) ([]byte, error)) ([]fixture, error) {
	var fixtures []fixture
	template := 0
	for _, m := range r.p.Mix {
		for _, v := range m.Variants {
			fmt.Printf("Preparing %s/%s synthetic study...\n", m.Modality, v.Name)
			study := fmt.Sprintf("1.2.826.0.1.3680043.10.543.97.%d", template+1)
			seriesNumber := 0
			for _, g := range v.Groups {
				pixels, err := synthetic.ModalityPixels(g.ImageSpec)
				if err != nil {
					return nil, err
				}
				for j := 0; j < g.Instances; j++ {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					if j%g.PerSeries == 0 {
						seriesNumber++
					}
					uid := fmt.Sprintf("1.2.826.0.1.3680043.10.543.99.%d", len(fixtures)+1)
					header, err := synthetic.ModalityHeader(g.ImageSpec, study, fmt.Sprintf("%s.%d", study, seriesNumber), uid, seriesNumber, j%g.PerSeries+1)
					if err != nil {
						return nil, err
					}
					source := bytes.NewReader(header)
					reader := bufio.NewReader(source)
					meta, err := synthetic.Part10(reader)
					if err != nil {
						return nil, err
					}
					offset := int64(len(header) - reader.Buffered() - source.Len())
					digest := sha256.New()
					_, _ = digest.Write(header[offset:])
					_, _ = digest.Write(pixels)
					fileDigest := sha256.New()
					_, _ = fileDigest.Write(header)
					_, _ = fileDigest.Write(pixels)
					f := fixture{Index: len(fixtures), File: fmt.Sprintf("fixture-%05d.dcm", len(fixtures)), Instance: uid, Class: meta.Class, Syntax: meta.Syntax, Offset: offset, DatasetBytes: int64(len(header)) + int64(len(pixels)) - offset, Digest: hex.EncodeToString(digest.Sum(nil)), FileDigest: hex.EncodeToString(fileDigest.Sum(nil)), Modality: m.Modality, Size: v.Name, Template: template, Pixels: int64(len(pixels)), Frames: g.Frames}
					file, err := os.Create(filepath.Join(r.work, f.File))
					if err != nil {
						return nil, err
					}
					_, err = io.Copy(file, io.MultiReader(bytes.NewReader(header), bytes.NewReader(pixels)))
					closeErr := file.Close()
					if err != nil {
						return nil, err
					}
					if closeErr != nil {
						return nil, closeErr
					}
					fixtures = append(fixtures, f)
					if j == 0 {
						data := append(append([]byte(nil), header...), pixels...)
						response, err := request("POST", "/instances", data)
						if err != nil {
							return nil, err
						}
						var imported struct{ ID string }
						if json.Unmarshal(response, &imported) != nil || imported.ID == "" {
							return nil, errors.New("Orthanc rejected modality fixture")
						}
						stored, err := request("GET", "/instances/"+imported.ID+"/file", nil)
						if err != nil || hash(stored) != f.FileDigest {
							return nil, errors.New("Orthanc changed modality fixture bytes")
						}
						if err := r.validateMixedFixture(ctx, f, g.ImageSpec); err != nil {
							return nil, fmt.Errorf("%s/%s: %w", m.Modality, v.Name, err)
						}
					}
				}
			}
			template++
		}
	}
	return fixtures, nil
}

// This is an explicit test contract, not a claim of exhaustive IOD conformance.
// Orthanc parses each distinct pixel layout, and dcm4che independently checks
// required attributes and decodes the first/last frame with exact pixel checks.
func modalityIOD(s synthetic.ImageSpec) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\"?><IOD>\n")
	add := func(tag, vr, typ, vm, value string) {
		fmt.Fprintf(&b, "<DataElement tag=\"%s\" vr=\"%s\" type=\"%s\" vm=\"%s\">", tag, vr, typ, vm)
		if value != "" {
			fmt.Fprintf(&b, "<Value>%s</Value>", value)
		}
		b.WriteString("</DataElement>\n")
	}
	for _, v := range []struct{ tag, vr, typ, vm string }{
		{"00100010", "PN", "2", "1"}, {"00100020", "LO", "2", "1"}, {"00100030", "DA", "2", "1"}, {"00100040", "CS", "2", "1"},
		{"0020000D", "UI", "1", "1"}, {"0020000E", "UI", "1", "1"}, {"00080018", "UI", "1", "1"}, {"00080020", "DA", "2", "1"}, {"00080030", "TM", "2", "1"}, {"00080050", "SH", "2", "1"}, {"00080090", "PN", "2", "1"}, {"00200010", "SH", "2", "1"}, {"00200011", "IS", "2", "1"}, {"00200013", "IS", "2", "1"}, {"00080008", "CS", "1", "3-n"},
	} {
		add(v.tag, v.vr, v.typ, v.vm, "")
	}
	add("00080016", "UI", "1", "1", s.Class())
	add("00080060", "CS", "1", "1", s.Modality)
	add("0008001C", "CS", "1", "1", "YES")
	for tag, n := range map[string]int{"00280002": s.Samples, "00280010": s.Rows, "00280011": s.Columns, "00280100": s.Bits, "00280101": s.Bits, "00280102": s.Bits - 1, "00280103": 0} {
		add(tag, "US", "1", "1", fmt.Sprint(n))
	}
	photo := "MONOCHROME2"
	if s.Samples == 3 {
		photo = "RGB"
		add("00280006", "US", "1", "1", "0")
	}
	add("00280004", "CS", "1", "1", photo)
	if s.Modality == "CT" || s.Modality == "MR" {
		for _, v := range []struct{ tag, vr, vm string }{{"00200052", "UI", "1"}, {"00201040", "LO", "1"}, {"00200032", "DS", "3"}, {"00200037", "DS", "6"}, {"00280030", "DS", "2"}, {"00180050", "DS", "1"}} {
			typ := "1"
			if v.tag == "00201040" {
				typ = "2"
			}
			add(v.tag, v.vr, typ, v.vm, "")
		}
	}
	if s.Modality == "CT" {
		add("00281052", "DS", "1", "1", "0")
		add("00281053", "DS", "1", "1", "1")
		add("00180060", "DS", "2", "1", "")
	}
	if s.Modality == "MR" {
		add("00180020", "CS", "1", "1-n", "SE")
		add("00180021", "CS", "1", "1-n", "SK")
		add("00180022", "CS", "2", "1-n", "")
		add("00180023", "CS", "2", "1", "2D")
		for _, tag := range []string{"00180080", "00180081"} {
			add(tag, "DS", "2", "1", "")
		}
		add("00180091", "IS", "2", "1", "")
	}
	if s.Modality == "DX" {
		add("00080068", "CS", "1", "1", "FOR PRESENTATION")
		add("00082218", "SQ", "2", "1", "")
		add("00187004", "CS", "2", "1", "DIRECT")
		add("00200020", "CS", "1", "2", "")
		add("00200062", "CS", "1", "1", "U")
		add("00181164", "DS", "1", "2", "")
		add("00280301", "CS", "1", "1", "NO")
		add("00281040", "CS", "1", "1", "LIN")
		add("00281041", "SS", "1", "1", "1")
		add("00281050", "DS", "1", "1", "32768")
		add("00281051", "DS", "1", "1", "65536")
		add("00281052", "DS", "1", "1", "0")
		add("00281053", "DS", "1", "1", "1")
		add("00281054", "LO", "1", "1", "US")
		add("00282110", "CS", "1", "1", "00")
		add("00400555", "SQ", "2", "1", "")
		add("20500020", "CS", "1", "1", "IDENTITY")
	}
	if s.Frames > 1 {
		add("00280008", "IS", "1", "1", fmt.Sprint(s.Frames))
		add("00280009", "AT", "1", "1", "")
		add("00181063", "DS", "1", "1", "")
	}
	b.WriteString("</IOD>\n")
	return b.String()
}

func (r *runner) validateMixedFixture(ctx context.Context, f fixture, s synthetic.ImageSpec) error {
	if err := os.WriteFile(filepath.Join(r.work, "mixed-iod.xml"), []byte(modalityIOD(s)), 0644); err != nil {
		return err
	}
	options := []string{"--mount", "type=bind,src=" + r.work + ",dst=/work"}
	name, err := r.start(ctx, "validate", options, dcm4cheImage, []string{"dcmvalidate", "--iod", "/work/mixed-iod.xml", "/work/" + f.File})
	if err != nil {
		return err
	}
	if err := r.waitContainer(ctx, name); err != nil {
		return err
	}
	logs, err := r.docker(ctx, "logs", name)
	if err != nil {
		return err
	}
	if !strings.Contains(string(logs), " ... OK") || strings.Contains(string(logs), "FAILED:") {
		return errors.New("dcm4che modality contract validation failed")
	}
	frames := []int{1}
	if s.Frames > 1 {
		frames = append(frames, s.Frames)
	}
	for _, frame := range frames {
		name, err = r.start(ctx, "decode", options, dcm4cheImage, []string{"dcm2jpg", "--noauto", "--noshape", "--frame", fmt.Sprint(frame), "-F", "PNG", "/work/" + f.File, "/work/mixed-pixels.png"})
		if err != nil {
			return err
		}
		if err := r.waitContainer(ctx, name); err != nil {
			return errors.New("dcm4che modality pixel decoding failed")
		}
		file, err := os.Open(filepath.Join(r.work, "mixed-pixels.png"))
		if err != nil {
			return err
		}
		decoded, err := png.Decode(file)
		_ = file.Close()
		if err != nil {
			return err
		}
		if decoded.Bounds().Dx() != s.Columns || decoded.Bounds().Dy() != s.Rows {
			return errors.New("decoded modality dimensions changed")
		}
		for row := 0; row < s.Rows; row++ {
			for col := 0; col < s.Columns; col++ {
				r, g, b, _ := decoded.At(col, row).RGBA()
				channels := []uint32{r, g, b}
				for channel, got := range channels {
					c := channel
					if s.Samples == 1 {
						c = 0
					}
					want := uint32(byte((row*17+col*31+(row*col)%251+(frame-1)*13+c*47)%256)) * 257
					if got != want {
						return fmt.Errorf("decoded modality pixels changed at frame %d", frame)
					}
				}
			}
		}
	}
	return nil
}
