package retrieval

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

var ErrIdentity = errors.New("identity_mismatch")
var ErrPartial = errors.New("partial_transfer")

// DICOMStream qualifies native little-endian datasets, including
// encapsulated pixel data. It buffers only the identity prefix (at most 1 MiB),
// then validates/copies the remainder with cloud backpressure. No transcoding.
// Other transfer syntaxes require their own qualified parser, not a guess.
type DICOMStream struct {
	reader      *bufio.Reader
	prefix      bytes.Buffer
	writer      io.Writer
	values      map[uint32]string
	last        uint32
	bytes       int64
	limit       int64
	SOP         string
	metaSOP     string
	metaClass   string
	implicit    bool
	localIssuer bool
}

func OpenDICOM(r io.Reader, p Permit, study string, limit int64) (*DICOMStream, error) {
	return openDICOM(r, p, study, limit, false)
}

// OpenDIMSEDICOM uses the locally approved accession namespace when basic PACS
// omit the optional issuer sequence. A supplied conflicting issuer still fails.
func OpenDIMSEDICOM(r io.Reader, p Permit, study string, limit int64) (*DICOMStream, error) {
	return openDICOM(r, p, study, limit, true)
}
func openDICOM(r io.Reader, p Permit, study string, limit int64, localIssuer bool) (*DICOMStream, error) {
	d := &DICOMStream{reader: bufio.NewReaderSize(r, 32768), values: map[uint32]string{}, limit: limit, localIssuer: localIssuer}
	d.writer = &d.prefix
	header := make([]byte, 132)
	if d.read(header) != nil || string(header[128:]) != "DICM" {
		return nil, ErrPartial
	}
	var metaLast uint32
	var metaEnd int64
	for {
		peek, e := d.reader.Peek(4)
		if e != nil {
			return nil, ErrPartial
		}
		if binary.LittleEndian.Uint16(peek) != 2 {
			break
		}
		tag, vr, n, e := d.element()
		if e != nil || tag <= metaLast || n > 1024 || n == 0xffffffff {
			return nil, ErrPartial
		}
		metaLast = tag
		b := make([]byte, n)
		if d.read(b) != nil {
			return nil, ErrPartial
		}
		value := strings.TrimRight(string(b), "\x00 ")
		switch tag {
		case 0x00020000:
			if vr != "UL" || len(b) != 4 {
				return nil, ErrPartial
			}
			metaEnd = d.bytes + int64(binary.LittleEndian.Uint32(b))
		case 0x00020002:
			if vr != "UI" || !UID(value) {
				return nil, ErrPartial
			}
			d.metaClass = value
		case 0x00020003:
			if vr != "UI" || !UID(value) {
				return nil, ErrPartial
			}
			d.metaSOP = value
		case 0x00020010:
			// Native explicit little endian or registered encapsulated syntaxes.
			if value != "1.2.840.10008.1.2" && value != "1.2.840.10008.1.2.1" && value != "1.2.840.10008.1.2.5" && !supportedEncapsulated(value) {
				return nil, ErrPolicy
			}
			d.values[tag] = value
		}
	}
	if metaEnd != d.bytes || d.metaSOP == "" || !strings.HasPrefix(d.metaClass, "1.2.840.10008.5.1.4.1.1.") || d.values[0x00020010] == "" {
		return nil, ErrPartial
	}
	d.implicit = d.values[0x00020010] == "1.2.840.10008.1.2"
	for d.last < 0x0020000d {
		if e := d.topElement(); e != nil {
			return nil, e
		}
		if d.prefix.Len() > 1024*1024 {
			return nil, ErrPolicy
		}
	}
	if d.last != 0x0020000d || d.values[0x0020000d] != study || d.values[0x00080050] != p.Examination.Accession || !d.matchesIssuer(p.Examination.Issuer) {
		return nil, ErrIdentity
	}
	if d.values[0x00080016] != d.metaClass || d.values[0x00080018] != d.metaSOP {
		return nil, ErrIdentity
	}
	if charset := d.values[0x00080005]; charset != "" && charset != "ISO_IR 6" && charset != "ISO_IR 192" && !(charset == "ISO_IR 100" && asciiIdentity(d.values)) {
		return nil, ErrPolicy
	}
	d.SOP = d.metaSOP
	return d, nil
}
func supportedEncapsulated(s string) bool {
	for _, suffix := range []string{"50", "51", "57", "70", "80", "81", "90", "91", "92", "93", "201", "202", "203"} {
		if s == "1.2.840.10008.1.2.4."+suffix {
			return true
		}
	}
	return false
}
func (d *DICOMStream) read(b []byte) error {
	if int64(len(b)) > d.limit-d.bytes || (d.writer == &d.prefix && len(b) > 1024*1024-d.prefix.Len()) {
		return ErrPolicy
	}
	if _, e := io.ReadFull(d.reader, b); e != nil {
		return ErrPartial
	}
	d.bytes += int64(len(b))
	if _, e := d.writer.Write(b); e != nil {
		return e
	}
	return nil
}
func (d *DICOMStream) copy(n uint32) error {
	var b [32768]byte
	for n > 0 {
		size := min(n, uint32(len(b)))
		if e := d.read(b[:size]); e != nil {
			return e
		}
		n -= size
	}
	return nil
}
func (d *DICOMStream) element() (uint32, string, uint32, error) {
	var h [8]byte
	if e := d.read(h[:]); e != nil {
		return 0, "", 0, e
	}
	tag := uint32(binary.LittleEndian.Uint16(h[:2]))<<16 | uint32(binary.LittleEndian.Uint16(h[2:4]))
	if tag>>16 == 0xfffe {
		return tag, "", binary.LittleEndian.Uint32(h[4:]), nil
	}
	if d.implicit {
		n := binary.LittleEndian.Uint32(h[4:])
		if n != 0xffffffff && n%2 != 0 {
			return 0, "", 0, ErrPartial
		}
		vr := map[uint32]string{0x00080005: "CS", 0x00080016: "UI", 0x00080018: "UI", 0x00080050: "SH", 0x00080051: "SQ", 0x00080052: "CS", 0x00100020: "LO", 0x00100021: "LO", 0x0020000d: "UI", 0x00400031: "UT"}[tag]
		// Unknown defined-length values can be copied unchanged. Undefined-length
		// implicit values must be item-encoded sequences (pixel data is separate).
		if vr == "" && n == 0xffffffff && tag != 0x7fe00010 {
			vr = "SQ"
		}
		return tag, vr, n, nil
	}
	vr := string(h[4:6])
	var n uint32
	switch vr {
	case "OB", "OD", "OF", "OL", "OV", "OW", "SQ", "UC", "UR", "UT", "UN":
		if h[6] != 0 || h[7] != 0 {
			return 0, "", 0, ErrPartial
		}
		var more [4]byte
		if e := d.read(more[:]); e != nil {
			return 0, "", 0, e
		}
		n = binary.LittleEndian.Uint32(more[:])
	case "AE", "AS", "AT", "CS", "DA", "DS", "DT", "FD", "FL", "IS", "LO", "LT", "PN", "SH", "SL", "SS", "ST", "SV", "TM", "UI", "UL", "US", "UV":
		n = uint32(binary.LittleEndian.Uint16(h[6:]))
	default:
		return 0, "", 0, ErrPartial
	}
	if n != 0xffffffff && n%2 != 0 {
		return 0, "", 0, ErrPartial
	}
	return tag, vr, n, nil
}
func (d *DICOMStream) topElement() error {
	tag, vr, n, e := d.element()
	if e != nil {
		return e
	}
	if tag <= d.last || tag>>16 == 0xfffe {
		return ErrPartial
	}
	d.last = tag
	if tag == 0x00080051 {
		if vr != "SQ" {
			return ErrIdentity
		}
		return d.sequence(n, 0, true)
	}
	if vr == "SQ" {
		return d.sequence(n, 0, false)
	}
	if n == 0xffffffff {
		if tag != 0x7fe00010 || (vr != "OB" && vr != "OW") || d.values[0x00020010] == "1.2.840.10008.1.2.1" {
			return ErrPartial
		}
		return d.fragments()
	}
	switch tag {
	case 0x00080005, 0x00080016, 0x00080018, 0x00080050, 0x00080052, 0x00100020, 0x00100021, 0x0020000d:

		expectedVR := map[uint32]string{0x00080005: "CS", 0x00080016: "UI", 0x00080018: "UI", 0x00080050: "SH", 0x00080052: "CS", 0x00100020: "LO", 0x00100021: "LO", 0x0020000d: "UI"}
		if vr != expectedVR[tag] {
			return ErrPartial
		}
		if n > 256 {
			return ErrIdentity
		}
		b := make([]byte, n)
		if e := d.read(b); e != nil {
			return e
		}
		d.values[tag] = strings.TrimRight(string(b), "\x00 ")
	default:
		return d.copy(n)
	}
	return nil
}
func (d *DICOMStream) sequence(n uint32, depth int, issuer bool) error {
	if depth > 12 {
		return ErrPolicy
	}
	end := d.bytes + int64(n)
	items := 0
	for n == 0xffffffff || d.bytes < end {
		tag, _, length, e := d.element()
		if e != nil {
			return e
		}
		if tag == 0xfffee0dd {
			if n != 0xffffffff || length != 0 {
				return ErrPartial
			}
			if issuer && items != 1 && !(d.localIssuer && items == 0) {
				return ErrIdentity
			}
			return nil
		}
		if tag != 0xfffee000 {
			return ErrPartial
		}
		items++
		if issuer && items > 1 {
			return ErrIdentity
		}
		itemEnd := d.bytes + int64(length)
		var last uint32
		for length == 0xffffffff || d.bytes < itemEnd {
			t, vr, size, e := d.element()
			if e != nil {
				return e
			}
			if t == 0xfffee00d {
				if length != 0xffffffff || size != 0 {
					return ErrPartial
				}
				break
			}
			if t <= last || t>>16 == 0xfffe {
				return ErrPartial
			}
			last = t
			if issuer && t == 0x00400031 {
				if vr != "UT" || size > 256 {
					return ErrIdentity
				}
				b := make([]byte, size)
				if e := d.read(b); e != nil {
					return e
				}
				d.values[t] = strings.TrimRight(string(b), "\x00 ")
			} else if vr == "SQ" {
				if e := d.sequence(size, depth+1, false); e != nil {
					return e
				}
			} else {
				if size == 0xffffffff {
					return ErrPartial
				}
				if e := d.copy(size); e != nil {
					return e
				}
			}
		}
		if length != 0xffffffff && d.bytes != itemEnd {
			return ErrPartial
		}
	}
	if d.bytes != end {
		return ErrPartial
	}
	if issuer && items != 1 && !(d.localIssuer && items == 0) {
		return ErrIdentity
	}
	return nil
}
func (d *DICOMStream) fragments() error {
	for {
		tag, _, n, e := d.element()
		if e != nil {
			return e
		}
		if tag == 0xfffee0dd {
			if n != 0 {
				return ErrPartial
			}
			return nil
		}
		if tag != 0xfffee000 || n == 0xffffffff || n%2 != 0 {
			return ErrPartial
		}
		if e := d.copy(n); e != nil {
			return e
		}
	}
}
func (d *DICOMStream) WriteTo(w io.Writer) (int64, error) {
	d.writer = w
	if _, e := w.Write(d.prefix.Bytes()); e != nil {
		return d.bytes, e
	}
	d.prefix.Reset()
	for {
		_, e := d.reader.Peek(1)
		if e == io.EOF {
			return d.bytes, nil
		}
		if e != nil {
			return d.bytes, ErrPartial
		}
		if e := d.topElement(); e != nil {
			return d.bytes, e
		}
	}
}

func (d *DICOMStream) TransferSyntax() string { return d.values[0x00020010] }

func (d *DICOMStream) matchesIssuer(expected string) bool {
	return d.values[0x00400031] == expected || (d.localIssuer && d.values[0x00400031] == "")
}

// StudyIdentifier validates a bounded C-FIND response in its negotiated syntax.
func StudyIdentifier(data []byte, syntax string, p Permit) (string, error) {
	if syntax != "1.2.840.10008.1.2" && syntax != "1.2.840.10008.1.2.1" {
		return "", ErrPolicy
	}
	d := &DICOMStream{reader: bufio.NewReader(bytes.NewReader(data)), writer: io.Discard, values: map[uint32]string{}, limit: 1024 * 1024, implicit: syntax == "1.2.840.10008.1.2", localIssuer: true}
	for {
		_, e := d.reader.Peek(1)
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", ErrPartial
		}
		if e = d.topElement(); e != nil {
			return "", e
		}
	}
	if !UID(d.values[0x0020000d]) || d.values[0x00080050] != p.Examination.Accession || !d.matchesIssuer(p.Examination.Issuer) {
		return "", ErrIdentity
	}
	if level := d.values[0x00080052]; level != "" && level != "STUDY" {
		return "", ErrIdentity
	}
	if cs := d.values[0x00080005]; cs != "" && cs != "ISO_IR 6" && cs != "ISO_IR 192" && !(cs == "ISO_IR 100" && asciiIdentity(d.values)) {
		return "", ErrPolicy
	}
	return d.values[0x0020000d], nil
}

func asciiIdentity(values map[uint32]string) bool {
	for _, tag := range []uint32{0x00080050, 0x00400031, 0x0020000d, 0x00080016, 0x00080018} {
		for _, c := range values[tag] {
			if c > 127 {
				return false
			}
		}
	}
	return true
}
