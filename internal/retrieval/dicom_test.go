package retrieval

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func dicomElement(tag uint32, vr string, b []byte) []byte {
	b = bytes.Clone(b)
	if len(b)%2 != 0 {
		pad := byte(' ')
		if vr == "UI" || vr == "OB" {
			pad = 0
		}
		b = append(b, pad)
	}
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(tag>>16))
	binary.Write(&out, binary.LittleEndian, uint16(tag))
	out.WriteString(vr)
	switch vr {
	case "SQ", "UT", "OB", "OW":
		out.Write([]byte{0, 0})
		binary.Write(&out, binary.LittleEndian, uint32(len(b)))
	default:
		binary.Write(&out, binary.LittleEndian, uint16(len(b)))
	}
	out.Write(b)
	return out.Bytes()
}
func dicomItem(tag uint32, b []byte) []byte {
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(tag>>16))
	binary.Write(&out, binary.LittleEndian, uint16(tag))
	binary.Write(&out, binary.LittleEndian, uint32(len(b)))
	out.Write(b)
	return out.Bytes()
}
func syntheticDICOM(ts string, issuer []byte, pixelSize int) []byte {
	sop := "1.2.3.4"
	class := "1.2.840.10008.5.1.4.1.1.7"
	var meta []byte
	for _, e := range []struct {
		tag   uint32
		value string
	}{{0x00020002, class}, {0x00020003, sop}, {0x00020010, ts}} {
		meta = append(meta, dicomElement(e.tag, "UI", []byte(e.value))...)
	}
	b := make([]byte, 132)
	copy(b[128:], "DICM")
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(meta)))
	b = append(b, dicomElement(0x00020000, "UL", length)...)
	b = append(b, meta...)
	for _, e := range []struct {
		tag       uint32
		vr, value string
	}{{0x00080016, "UI", class}, {0x00080018, "UI", sop}, {0x00080050, "SH", "ACC"}} {
		b = append(b, dicomElement(e.tag, e.vr, []byte(e.value))...)
	}
	b = append(b, dicomElement(0x00080051, "SQ", issuer)...)
	for _, e := range []struct {
		tag       uint32
		vr, value string
	}{{0x00100020, "LO", "PATIENT"}, {0x00100021, "LO", "CLINIC"}, {0x0020000d, "UI", "1.2.3"}} {
		b = append(b, dicomElement(e.tag, e.vr, []byte(e.value))...)
	}
	return append(b, dicomElement(0x7fe00010, "OB", make([]byte, pixelSize))...)
}
func syntheticIssuer() []byte {
	return dicomItem(0xfffee000, dicomElement(0x00400031, "UT", []byte("CLINIC")))
}
func parseSynthetic(b []byte, limit int64) error {
	d, e := OpenDICOM(bytes.NewReader(b), fixturePermit(), "1.2.3", limit)
	if e != nil {
		return e
	}
	_, e = d.WriteTo(io.Discard)
	return e
}
func TestDICOMStreamingIntegrity(t *testing.T) {
	for _, ts := range []string{"1.2.840.10008.1.2.1", "1.2.840.10008.1.2.4.70", "1.2.840.10008.1.2.5"} {
		t.Run(ts, func(t *testing.T) {
			b := syntheticDICOM(ts, syntheticIssuer(), 2*1024*1024)
			d, e := OpenDICOM(bytes.NewReader(b), fixturePermit(), "1.2.3", int64(len(b)))
			if e != nil {
				t.Fatal(e)
			}
			if d.prefix.Len() > 1024*1024 || d.prefix.Len() >= len(b) || d.TransferSyntax() != ts || d.SOP != "1.2.3.4" {
				t.Fatal("unbounded or incorrect identity prefix")
			}
			var out bytes.Buffer
			n, e := d.WriteTo(&out)
			if e != nil || n != int64(len(b)) || !bytes.Equal(out.Bytes(), b) {
				t.Fatal("DICOM bytes changed")
			}
		})
	}
}
func TestDICOMRejectsMalformedAndForeignObjects(t *testing.T) {
	good := syntheticDICOM("1.2.840.10008.1.2.1", syntheticIssuer(), 16)
	mutate := func(find, replacement []byte) []byte { return bytes.Replace(good, find, replacement, 1) }
	cases := map[string][]byte{
		"missing Part10":        good[1:],
		"empty":                 {},
		"truncated header":      good[:150],
		"truncated pixels":      good[:len(good)-1],
		"unsupported syntax":    syntheticDICOM("1.2.840.10008.1.2", syntheticIssuer(), 16),
		"missing issuer":        syntheticDICOM("1.2.840.10008.1.2.1", nil, 16),
		"multiple issuers":      syntheticDICOM("1.2.840.10008.1.2.1", append(syntheticIssuer(), syntheticIssuer()...), 16),
		"conflicting issuer":    mutate([]byte("CLINIC"), []byte("OTHERX")),
		"conflicting accession": mutate([]byte("ACC"), []byte("BAD")),
		"meta SOP mismatch":     mutate([]byte("1.2.3.4"), []byte("1.2.3.5")),
		"duplicate identity":    append(bytes.Clone(good), dicomElement(0x00100020, "LO", []byte("PATIENT"))...),
		"wrong identity VR":     mutate(dicomElement(0x00100020, "LO", []byte("PATIENT")), dicomElement(0x00100020, "SH", []byte("PATIENT"))),
	}
	badGroup := bytes.Clone(good)
	badGroup[140]++
	cases["meta group length"] = badGroup
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if parseSynthetic(b, int64(len(b))) == nil {
				t.Fatal("malformed object accepted")
			}
		})
	}
	if parseSynthetic(good, int64(len(good)-1)) == nil {
		t.Fatal("byte limit not enforced")
	}
}
func TestDICOMUndefinedSequencesAndFragments(t *testing.T) {
	issuer := dicomItem(0xfffee000, dicomElement(0x00400031, "UT", []byte("CLINIC")))
	binary.LittleEndian.PutUint32(issuer[4:8], 0xffffffff)
	issuer = append(issuer, dicomItem(0xfffee00d, nil)...)
	good := syntheticDICOM("1.2.840.10008.1.2.4.70", issuer, 16)
	// Undefined sequence terminated after its single undefined-length item.
	start := bytes.Index(good, []byte{0x08, 0, 0x51, 0, 'S', 'Q', 0, 0})
	size := int(binary.LittleEndian.Uint32(good[start+8 : start+12]))
	end := start + 12 + size
	good = append(append(bytes.Clone(good[:end]), dicomItem(0xfffee0dd, nil)...), good[end:]...)
	binary.LittleEndian.PutUint32(good[start+8:start+12], 0xffffffff)
	pixel := bytes.Index(good, []byte{0xe0, 0x7f, 0x10, 0, 'O', 'B', 0, 0})
	good = good[:pixel+12]
	binary.LittleEndian.PutUint32(good[pixel+8:pixel+12], 0xffffffff)
	good = append(good, dicomItem(0xfffee000, nil)...)
	good = append(good, dicomItem(0xfffee000, []byte{1, 2, 3, 4})...)
	good = append(good, dicomItem(0xfffee0dd, nil)...)
	if e := parseSynthetic(good, int64(len(good))); e != nil {
		t.Fatal(e)
	}
	for _, b := range [][]byte{good[:len(good)-8], append(bytes.Clone(good[:len(good)-8]), dicomItem(0xfffee00d, nil)...)} {
		if parseSynthetic(b, int64(len(b))) == nil {
			t.Fatal("invalid fragment termination accepted")
		}
	}
}

type failingDICOMWriter struct{}

func (failingDICOMWriter) Write([]byte) (int, error) { return 0, errors.New("cancelled") }
func TestDICOMStopsOnBackpressureFailure(t *testing.T) {
	b := syntheticDICOM("1.2.840.10008.1.2.1", syntheticIssuer(), 16)
	d, e := OpenDICOM(bytes.NewReader(b), fixturePermit(), "1.2.3", int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = d.WriteTo(failingDICOMWriter{}); e == nil {
		t.Fatal("writer failure lost")
	}
}

func TestDIMSEIssuerAndCharacterSet(t *testing.T) {
	for _, tc := range []struct {
		name, charset, accession string
		issuer                   []byte
		want                     error
	}{
		{"empty issuer", "", "ACC", nil, nil},
		{"matching issuer", "ISO_IR 100", "ACC", syntheticIssuer(), nil},
		{"foreign issuer", "", "ACC", dicomItem(0xfffee000, dicomElement(0x00400031, "UT", []byte("OTHER"))), ErrIdentity},
		{"latin1 non-ascii identity", "ISO_IR 100", "Aé", nil, ErrPolicy},
		{"unsupported charset", "ISO 2022 IR 87", "ACC", nil, ErrPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixturePermit()
			p.Examination.Accession = tc.accession
			data := dicomElement(0x00080005, "CS", []byte(tc.charset))
			data = append(data, dicomElement(0x00080050, "SH", []byte(tc.accession))...)
			data = append(data, dicomElement(0x00080051, "SQ", tc.issuer)...)
			data = append(data, dicomElement(0x00080052, "CS", []byte("STUDY"))...)
			data = append(data, dicomElement(0x0020000d, "UI", []byte("1.2.3"))...)
			uid, err := StudyIdentifier(data, "1.2.840.10008.1.2.1", p)
			if !errors.Is(err, tc.want) || (err == nil && uid != "1.2.3") {
				t.Fatalf("discovery identity: %v", err)
			}
			object := syntheticDICOM("1.2.840.10008.1.2.1", tc.issuer, 4)
			if tc.accession == "ACC" && tc.charset == "" {
				_, err = OpenDIMSEDICOM(bytes.NewReader(object), p, "1.2.3", int64(len(object)))
				if !errors.Is(err, tc.want) {
					t.Fatalf("object identity: %v", err)
				}
			}
		})
	}
}
