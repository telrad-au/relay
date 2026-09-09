// Package synthetic provides PHI-free fixtures for tests and performance tools only.
package synthetic

import "encoding/binary"

const SecondaryCapture = "1.2.840.10008.5.1.4.1.1.7"
const ExplicitVRLittleEndian = "1.2.840.10008.1.2.1"
const JPEGLosslessSV1 = "1.2.840.10008.1.2.4.70"

func DICOM(instanceUID, seriesUID string, rows, columns int) []byte {
	if rows < 1 || columns < 1 || rows > 8192 || columns > 8192 {
		panic("invalid synthetic pixel dimensions")
	}
	text := func(value string, padding byte) []byte {
		data := []byte(value)
		if len(data)%2 != 0 {
			data = append(data, padding)
		}
		return data
	}
	ui := func(value string) []byte { return text(value, 0) }
	us := func(value uint16) []byte {
		data := make([]byte, 2)
		binary.LittleEndian.PutUint16(data, value)
		return data
	}

	var metadata []byte
	metadata = Element(metadata, 0x0002, 0x0001, "OB", []byte{0, 1})
	metadata = Element(metadata, 0x0002, 0x0002, "UI", ui(SecondaryCapture))
	metadata = Element(metadata, 0x0002, 0x0003, "UI", ui(instanceUID))
	metadata = Element(metadata, 0x0002, 0x0010, "UI", ui(ExplicitVRLittleEndian))
	metadata = Element(metadata, 0x0002, 0x0012, "UI", ui("1.2.826.0.1.3680043.10.543.89"))

	groupLength := make([]byte, 4)
	binary.LittleEndian.PutUint32(groupLength, uint32(len(metadata)))
	var data []byte
	data = append(data, make([]byte, 128)...)
	data = append(data, "DICM"...)
	data = Element(data, 0x0002, 0x0000, "UL", groupLength)
	data = append(data, metadata...)

	data = Element(data, 0x0008, 0x0008, "CS", text("DERIVED\\SECONDARY", ' '))
	data = Element(data, 0x0008, 0x0016, "UI", ui(SecondaryCapture))
	data = Element(data, 0x0008, 0x0018, "UI", ui(instanceUID))
	data = Element(data, 0x0008, 0x001c, "CS", text("YES", ' '))
	data = Element(data, 0x0008, 0x0020, "DA", text("20260101", ' '))
	data = Element(data, 0x0008, 0x0023, "DA", text("20260101", ' '))
	data = Element(data, 0x0008, 0x0030, "TM", text("120000", ' '))
	data = Element(data, 0x0008, 0x0033, "TM", text("120000", ' '))
	data = Element(data, 0x0008, 0x0050, "SH", text("SYNTH0001", ' '))
	data = Element(data, 0x0008, 0x0060, "CS", text("OT", ' '))
	data = Element(data, 0x0008, 0x0064, "CS", text("SYN", ' '))
	data = Element(data, 0x0008, 0x0070, "LO", text("Telrad test fixture", ' '))
	data = Element(data, 0x0008, 0x0090, "PN", nil)
	data = Element(data, 0x0008, 0x1030, "LO", text("Synthetic Relay interoperability", ' '))
	data = Element(data, 0x0008, 0x103e, "LO", text("Deterministic payload integrity", ' '))
	data = Element(data, 0x0010, 0x0010, "PN", text("SYNTHETIC^RELAY", ' '))
	data = Element(data, 0x0010, 0x0020, "LO", text("RELAY-INTEROP", ' '))
	data = Element(data, 0x0010, 0x0030, "DA", text("19800101", ' '))
	data = Element(data, 0x0010, 0x0040, "CS", text("O", ' '))
	data = Element(data, 0x0020, 0x000d, "UI", ui("1.2.826.0.1.3680043.10.543.82.1"))
	data = Element(data, 0x0020, 0x000e, "UI", ui(seriesUID))
	data = Element(data, 0x0020, 0x0010, "SH", text("SYNTHETIC", ' '))
	data = Element(data, 0x0020, 0x0011, "IS", text("1", ' '))
	data = Element(data, 0x0020, 0x0013, "IS", text("1", ' '))
	data = Element(data, 0x0020, 0x0020, "CS", nil)
	data = Element(data, 0x0028, 0x0002, "US", us(1))
	data = Element(data, 0x0028, 0x0004, "CS", text("MONOCHROME2", ' '))
	data = Element(data, 0x0028, 0x0010, "US", us(uint16(rows)))
	data = Element(data, 0x0028, 0x0011, "US", us(uint16(columns)))
	data = Element(data, 0x0028, 0x0100, "US", us(8))
	data = Element(data, 0x0028, 0x0101, "US", us(8))
	data = Element(data, 0x0028, 0x0102, "US", us(7))
	data = Element(data, 0x0028, 0x0103, "US", us(0))

	return Element(data, 0x7fe0, 0x0010, "OB", Pixels(rows, columns))
}

func Pixels(rows, columns int) []byte {
	pixels := make([]byte, rows*columns)
	for row := 0; row < rows; row++ {
		for column := 0; column < columns; column++ {
			pixels[row*columns+column] = byte((row*17 + column*31 + (row*column)%251) % 256)
		}
	}
	return pixels
}

func Element(destination []byte, group, element uint16, vr string, value []byte) []byte {
	headerBytes := 8
	if LongVR(vr) {
		headerBytes = 12
	}
	header := make([]byte, headerBytes)
	binary.LittleEndian.PutUint16(header[0:2], group)
	binary.LittleEndian.PutUint16(header[2:4], element)
	copy(header[4:6], vr)
	if headerBytes == 12 {
		binary.LittleEndian.PutUint32(header[8:12], uint32(len(value)))
	} else {
		binary.LittleEndian.PutUint16(header[6:8], uint16(len(value)))
	}
	destination = append(destination, header...)
	return append(destination, value...)
}

func LongVR(vr string) bool {
	switch vr {
	case "OB", "OD", "OF", "OL", "OV", "OW", "SQ", "SV", "UC", "UN", "UR", "UT", "UV":
		return true
	default:
		return false
	}
}
