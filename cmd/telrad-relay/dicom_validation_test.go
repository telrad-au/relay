package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const dcm4cheToolsImage = "dcm4che/dcm4che-tools:5.33.1@sha256:c8fbede4a6cf6047370ad21ce12fcc6be7ab013ff4996f1d032eb55239f870ed"

// secondaryCaptureIODProfile is a test-only validation profile derived from
// the mandatory modules in DICOM PS3.3 A.8.1.3.
//
//go:embed testdata/secondary-capture-iod.xml
var secondaryCaptureIODProfile []byte

func validateSecondaryCaptureWithDcm4che(t *testing.T, ctx context.Context, fixtureName string, dicom []byte) {
	t.Helper()

	mount, decodedPath := newDcm4cheWorkspace(t, dicom)
	validationOutput, err := exec.CommandContext(
		ctx,
		"docker", "run", "--rm", "--mount", mount,
		dcm4cheToolsImage,
		"dcmvalidate", "--iod", "/work/secondary-capture-iod.xml", "/work/input.dcm",
	).CombinedOutput()
	if err != nil || !dcm4cheValidationPassed(validationOutput) {
		t.Fatalf("dcm4che rejected %s Secondary Capture fixture: %v\n%s", fixtureName, err, validationOutput)
	}

	decodeOutput, err := exec.CommandContext(
		ctx,
		"docker", "run", "--rm", "--mount", mount,
		dcm4cheToolsImage,
		"dcm2jpg", "--noauto", "--noshape", "-F", "PNG", "/work/input.dcm", "/work/decoded.png",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("dcm4che could not decode %s Pixel Data: %v\n%s", fixtureName, err, decodeOutput)
	}
	decodedFile, err := os.Open(decodedPath)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(decodedFile)
	closeErr := decodedFile.Close()
	if err != nil {
		t.Fatalf("decode dcm4che PNG for %s: %v", fixtureName, err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	decodedPixels, err := grayscale8Pixels(decoded)
	if err != nil {
		t.Fatalf("read dcm4che pixels for %s: %v", fixtureName, err)
	}
	expectedPixels := syntheticPixelData()
	if !bytes.Equal(decodedPixels, expectedPixels) {
		t.Fatalf(
			"dcm4che decoded %s pixels sha256=%s, want deterministic source sha256=%s",
			fixtureName,
			sha256Hex(decodedPixels),
			sha256Hex(expectedPixels),
		)
	}
}

func assertDcm4cheRejectsMissingReferringPhysician(t *testing.T, ctx context.Context, dicom []byte) {
	t.Helper()
	marker := []byte{0x08, 0x00, 0x90, 0x00, 'P', 'N', 0x00, 0x00}
	offset := bytes.Index(dicom, marker)
	if offset < 0 {
		t.Fatal("could not locate empty Referring Physician's Name in valid fixture")
	}
	missingRequiredTag := bytes.Clone(dicom)
	missingRequiredTag[offset+2] = 0x91
	mount, _ := newDcm4cheWorkspace(t, missingRequiredTag)
	output, err := exec.CommandContext(
		ctx,
		"docker", "run", "--rm", "--mount", mount,
		dcm4cheToolsImage,
		"dcmvalidate", "--iod", "/work/secondary-capture-iod.xml", "/work/input.dcm",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("run dcm4che negative validation: %v\n%s", err, output)
	}
	if dcm4cheValidationPassed(output) || !bytes.Contains(output, []byte("(0008,0090) ReferringPhysicianName")) {
		t.Fatalf("dcm4che did not reject missing Referring Physician's Name:\n%s", output)
	}
}

func newDcm4cheWorkspace(t *testing.T, dicom []byte) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "input.dcm"), dicom, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "secondary-capture-iod.xml"), secondaryCaptureIODProfile, 0600); err != nil {
		t.Fatal(err)
	}
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return "type=bind,source=" + realWorkspace + ",target=/work", filepath.Join(workspace, "decoded.png")
}

func dcm4cheValidationPassed(output []byte) bool {
	return bytes.Contains(output, []byte(" ... OK")) && !bytes.Contains(output, []byte("FAILED:"))
}

func TestDcm4cheValidationVerdict(t *testing.T) {
	if !dcm4cheValidationPassed([]byte("Validate: /work/input.dcm ... OK\n")) {
		t.Fatal("dcm4che success verdict was rejected")
	}
	for _, output := range [][]byte{
		[]byte("Validate: /work/input.dcm ... FAILED:\nMissing Attributes:\n"),
		[]byte("unexpected output"),
	} {
		if dcm4cheValidationPassed(output) {
			t.Fatalf("dcm4che failure verdict was accepted: %q", output)
		}
	}
}

func grayscale8Pixels(decoded image.Image) ([]byte, error) {
	bounds := decoded.Bounds()
	if bounds.Dx() != 512 || bounds.Dy() != 512 {
		return nil, fmt.Errorf("dimensions=%dx%d, want 512x512", bounds.Dx(), bounds.Dy())
	}
	pixels := make([]byte, 0, bounds.Dx()*bounds.Dy())
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			red, green, blue, alpha := decoded.At(x, y).RGBA()
			if red != green || green != blue || alpha != 0xffff {
				return nil, fmt.Errorf("pixel (%d,%d) is rgba(%d,%d,%d,%d), want opaque grayscale", x, y, red, green, blue, alpha)
			}
			pixels = append(pixels, byte(red>>8))
		}
	}
	return pixels, nil
}
