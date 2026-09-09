package synthetic

import (
	"encoding/binary"
	"fmt"
	"sort"
)

const CTStorage = "1.2.840.10008.5.1.4.1.1.2"
const MRStorage = "1.2.840.10008.5.1.4.1.1.4"
const DXStorage = "1.2.840.10008.5.1.4.1.1.1.1"
const USStorage = "1.2.840.10008.5.1.4.1.1.6.1"
const USMultiFrameStorage = "1.2.840.10008.5.1.4.1.1.3.1"

// ImageSpec describes pixels, independently of the number of SOP instances in
// a study. All values and pixels are synthetic, not derived from patient data.
type ImageSpec struct {
	Modality string `json:"modality"`
	Rows     int    `json:"rows"`
	Columns  int    `json:"columns"`
	Bits     int    `json:"bitsAllocated"`
	Samples  int    `json:"samplesPerPixel"`
	Frames   int    `json:"frames"`
}

func (s ImageSpec) PixelBytes() int64 {
	return int64(s.Rows) * int64(s.Columns) * int64(s.Samples) * int64(s.Bits/8) * int64(s.Frames)
}

func (s ImageSpec) Validate() error {
	if s.Rows < 1 || s.Rows > 4096 || s.Columns < 1 || s.Columns > 4096 || s.Frames < 1 || s.Frames > 400 || (s.Bits != 8 && s.Bits != 16) || (s.Samples != 1 && s.Samples != 3) || s.PixelBytes() > 128<<20 {
		return fmt.Errorf("invalid synthetic image dimensions")
	}
	if s.Modality == "US" && s.Bits == 8 {
		return nil
	}
	if (s.Modality == "CT" || s.Modality == "MR" || s.Modality == "DX") && s.Bits == 16 && s.Samples == 1 && s.Frames == 1 {
		return nil
	}
	return fmt.Errorf("unsupported synthetic modality representation")
}

func (s ImageSpec) Class() string {
	switch s.Modality {
	case "CT":
		return CTStorage
	case "MR":
		return MRStorage
	case "DX":
		return DXStorage
	case "US":
		if s.Frames > 1 {
			return USMultiFrameStorage
		}
		return USStorage
	}
	return ""
}

// ModalityHeader returns Part 10 metadata and the dataset up to Pixel Data.
// Keeping pixels separate lets fixtures share a bounded pixel buffer during
// construction and avoids retaining whole studies in the generator.
func ModalityHeader(s ImageSpec, study, series, instance string, seriesNumber, instanceNumber int) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	type value struct {
		vr   string
		data []byte
	}
	tags := map[uint32]value{}
	put := func(tag uint32, vr string, data []byte) { tags[tag] = value{vr, data} }
	str := func(tag uint32, vr, text string) {
		if len(text)%2 != 0 {
			if vr == "UI" {
				text += "\x00"
			} else {
				text += " "
			}
		}
		put(tag, vr, []byte(text))
	}
	us := func(tag uint32, n int) {
		b := make([]byte, 2)
		binary.LittleEndian.PutUint16(b, uint16(n))
		put(tag, "US", b)
	}
	for tag, text := range map[uint32]string{0x00080016: s.Class(), 0x00080018: instance, 0x0020000d: study, 0x0020000e: series} {
		str(tag, "UI", text)
	}
	for tag, text := range map[uint32]string{0x00080008: "ORIGINAL\\PRIMARY\\OTHER", 0x0008001c: "YES", 0x00080060: s.Modality, 0x00100040: "O", 0x00200020: "", 0x00280004: "MONOCHROME2", 0x00280301: "NO"} {
		str(tag, "CS", text)
	}
	if s.Samples == 3 {
		str(0x00280004, "CS", "RGB")
		us(0x00280006, 0)
	}
	for _, tag := range []uint32{0x00080020, 0x00080021, 0x00080023, 0x00100030} {
		str(tag, "DA", "20260101")
	}
	for _, tag := range []uint32{0x00080030, 0x00080031, 0x00080033} {
		str(tag, "TM", "120000")
	}
	str(0x00080050, "SH", "SYNTHETIC")
	str(0x00080070, "LO", "Telrad synthetic fixture")
	str(0x00080090, "PN", "")
	str(0x00100010, "PN", "SYNTHETIC^RELAY")
	str(0x00100020, "LO", "RELAY-PERF")
	str(0x00200010, "SH", "SYNTHETIC")
	str(0x00200011, "IS", fmt.Sprint(seriesNumber))
	str(0x00200012, "IS", "1")
	str(0x00200013, "IS", fmt.Sprint(instanceNumber))
	for tag, n := range map[uint32]int{0x00280002: s.Samples, 0x00280010: s.Rows, 0x00280011: s.Columns, 0x00280100: s.Bits, 0x00280101: s.Bits, 0x00280102: s.Bits - 1, 0x00280103: 0} {
		us(tag, n)
	}
	if s.Modality == "CT" || s.Modality == "MR" {
		str(0x00200052, "UI", study+".1")
		str(0x00201040, "LO", "")
		str(0x00200032, "DS", fmt.Sprintf("0\\0\\%d", instanceNumber))
		str(0x00200037, "DS", "1\\0\\0\\0\\1\\0")
		str(0x00280030, "DS", "1\\1")
		str(0x00180050, "DS", "1")
		str(0x00185100, "CS", "HFS")
		str(0x00281050, "DS", "32768")
		str(0x00281051, "DS", "65536")
	}
	switch s.Modality {
	case "CT":
		str(0x00080008, "CS", "ORIGINAL\\PRIMARY\\AXIAL")
		str(0x00180060, "DS", "120")
		str(0x00281052, "DS", "0")
		str(0x00281053, "DS", "1")
		str(0x00281054, "LO", "HU")
	case "MR":
		str(0x00080008, "CS", "ORIGINAL\\PRIMARY\\M")
		str(0x00180020, "CS", "SE")
		str(0x00180021, "CS", "SK")
		str(0x00180022, "CS", "")
		str(0x00180023, "CS", "2D")
		str(0x00180080, "DS", "500")
		str(0x00180081, "DS", "10")
		str(0x00180091, "IS", "1")
	case "DX":
		str(0x00080008, "CS", "ORIGINAL\\PRIMARY\\")
		str(0x00080068, "CS", "FOR PRESENTATION")
		put(0x00082218, "SQ", nil)
		str(0x00180015, "CS", "CHEST")
		str(0x00181164, "DS", "0.2\\0.2")
		str(0x00181508, "CS", "NONE")
		str(0x00187004, "CS", "DIRECT")
		str(0x00200020, "CS", "L\\F")
		str(0x00200062, "CS", "U")
		str(0x00281040, "CS", "LIN")
		put(0x00281041, "SS", []byte{1, 0})
		str(0x00281050, "DS", "32768")
		str(0x00281051, "DS", "65536")
		str(0x00281052, "DS", "0")
		str(0x00281053, "DS", "1")
		str(0x00281054, "LO", "US")
		str(0x00282110, "CS", "00")
		str(0x20500020, "CS", "IDENTITY")
		put(0x00400555, "SQ", nil)
	case "US":
		str(0x00080008, "CS", "ORIGINAL\\PRIMARY\\ABDOMINAL")
		if s.Frames > 1 {
			str(0x00280008, "IS", fmt.Sprint(s.Frames))
			put(0x00280009, "AT", []byte{0x18, 0, 0x63, 0x10})
			str(0x00181063, "DS", "33.333333")
		}
	}
	var metadata []byte
	metadata = Element(metadata, 2, 1, "OB", []byte{0, 1})
	for _, tag := range []uint32{0x00080016, 0x00080018} {
		v := tags[tag]
		element := uint16(2)
		if tag == 0x00080018 {
			element = 3
		}
		metadata = Element(metadata, 2, element, "UI", v.data)
	}
	ui := func(text string) []byte {
		if len(text)%2 != 0 {
			text += "\x00"
		}
		return []byte(text)
	}
	metadata = Element(metadata, 2, 0x10, "UI", ui(ExplicitVRLittleEndian))
	metadata = Element(metadata, 2, 0x12, "UI", ui("1.2.826.0.1.3680043.10.543.89"))
	b := append(make([]byte, 128), []byte("DICM")...)
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(metadata)))
	b = Element(b, 2, 0, "UL", length)
	b = append(b, metadata...)
	keys := make([]uint32, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, tag := range keys {
		v := tags[tag]
		b = Element(b, uint16(tag>>16), uint16(tag), v.vr, v.data)
	}
	vr := "OB"
	if s.Bits == 16 {
		vr = "OW"
	}
	pixelHeader := Element(nil, 0x7fe0, 0x0010, vr, nil)
	binary.LittleEndian.PutUint32(pixelHeader[8:], uint32(s.PixelBytes()))
	return append(b, pixelHeader...), nil
}

func ModalityPixels(s ImageSpec) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, int(s.PixelBytes()))
	n := 0
	for frame := 0; frame < s.Frames; frame++ {
		for row := 0; row < s.Rows; row++ {
			for col := 0; col < s.Columns; col++ {
				for channel := 0; channel < s.Samples; channel++ {
					v := byte((row*17 + col*31 + (row*col)%251 + frame*13 + channel*47) % 256)
					b[n] = v
					n++
					if s.Bits == 16 {
						b[n] = v
						n++
					}
				}
			}
		}
	}
	return b, nil
}
