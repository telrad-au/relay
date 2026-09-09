package synthetic

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
)

func Item(kind byte, data []byte) []byte {
	b := []byte{kind, 0, byte(len(data) >> 8), byte(len(data))}
	return append(b, data...)
}

func Association(syntax string) []byte {
	return StorageAssociation(SecondaryCapture, syntax)
}

func StorageAssociation(class, syntax string) []byte {
	b := make([]byte, 68)
	binary.BigEndian.PutUint16(b, 1)
	copy(b[4:20], "CLINIC_ARCHIVE  ")
	copy(b[20:36], "SYNTHETIC       ")
	b = append(b, Item(0x10, []byte("1.2.840.10008.3.1.1.1"))...)
	pc := append([]byte{1, 0, 0, 0}, Item(0x30, []byte(class))...)
	pc = append(pc, Item(0x40, []byte(syntax))...)
	b = append(b, Item(0x20, pc)...)
	return append(b, Item(0x50, Item(0x51, []byte{0, 1, 0, 0}))...)
}

func Command(id uint16, uid string) []byte {
	return StorageCommand(SecondaryCapture, id, uid)
}

func StorageCommand(class string, id uint16, uid string) []byte {
	var b []byte
	add := func(element uint16, value []byte) {
		h := make([]byte, 8)
		binary.LittleEndian.PutUint16(h[2:], element)
		binary.LittleEndian.PutUint32(h[4:], uint32(len(value)))
		b = append(append(b, h...), value...)
	}
	ui := func(s string) []byte {
		if len(s)%2 != 0 {
			s += "\x00"
		}
		return []byte(s)
	}
	add(2, ui(class))
	add(0x100, []byte{1, 0})
	add(0x110, []byte{byte(id), byte(id >> 8)})
	add(0x700, []byte{0, 0}) // Priority: medium.
	add(0x800, []byte{1, 0}) // Dataset present; only 0x0101 means absent.
	add(0x1000, ui(uid))
	h := make([]byte, 12)
	binary.LittleEndian.PutUint32(h[4:], 4)
	binary.LittleEndian.PutUint32(h[8:], uint32(len(b)))
	return append(h, b...)
}

func PDV(data []byte, flags byte) []byte {
	b := make([]byte, 6, 6+len(data))
	binary.BigEndian.PutUint32(b, uint32(len(data)+2))
	b[4], b[5] = 1, flags
	return append(b, data...)
}

func WritePDU(w io.Writer, kind byte, body []byte) error {
	h := []byte{kind, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(h[2:], uint32(len(body)))
	_, err := io.Copy(w, io.MultiReader(bytes.NewReader(h), bytes.NewReader(body)))
	return err
}

func ReadPDU(r io.Reader) (byte, []byte, error) {
	var h [6]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[2:])
	if n > 1024*1024 {
		return 0, nil, errors.New("oversized PDU")
	}
	b := make([]byte, int(n))
	_, err := io.ReadFull(r, b)
	return h[0], b, err
}

func Associate(c net.Conn, syntax string) error {
	return AssociateStorage(c, SecondaryCapture, syntax)
}

func AssociateStorage(c net.Conn, class, syntax string) error {
	if err := WritePDU(c, 1, StorageAssociation(class, syntax)); err != nil {
		return err
	}
	kind, b, err := ReadPDU(c)
	if err != nil {
		return err
	}
	if kind != 2 || len(b) < 68 {
		return errors.New("association rejected")
	}
	for p := 68; p+4 <= len(b); {
		n := int(binary.BigEndian.Uint16(b[p+2:]))
		if p+4+n > len(b) {
			break
		}
		v := b[p+4 : p+4+n]
		if b[p] == 0x21 && len(v) >= 8 && v[0] == 1 && v[2] == 0 && v[4] == 0x40 && string(v[8:]) == syntax {
			return nil
		}
		p += 4 + n
	}
	return errors.New("storage presentation context not accepted")
}

// SendStore supports fragmented commands and multiple dataset PDVs per PDU.
func SendStore(c net.Conn, uid string, id uint16, data io.Reader, size int64, pdu int, fragmented, multiple bool) error {
	return SendStorage(c, SecondaryCapture, uid, id, data, size, pdu, fragmented, multiple)
}

func SendStorage(c net.Conn, class, uid string, id uint16, data io.Reader, size int64, pdu int, fragmented, multiple bool) error {
	if pdu < 64 || pdu > 64*1024 {
		return errors.New("invalid test PDU size")
	}
	cmd := StorageCommand(class, id, uid)
	if fragmented {
		if err := WritePDU(c, 4, PDV(cmd[:len(cmd)/2], 1)); err != nil {
			return err
		}
		cmd = cmd[len(cmd)/2:]
	}
	if err := WritePDU(c, 4, PDV(cmd, 3)); err != nil {
		return err
	}
	chunk := make([]byte, pdu-12)
	for left := size; left > 0; {
		n := int(min(int64(len(chunk)), left))
		if _, err := io.ReadFull(data, chunk[:n]); err != nil {
			return err
		}
		left -= int64(n)
		flags := byte(0)
		if left == 0 {
			flags = 2
		}
		body := PDV(chunk[:n], flags)
		if multiple && n > 1 {
			body = append(PDV(chunk[:n/2], 0), PDV(chunk[n/2:n], flags)...)
		}
		if err := WritePDU(c, 4, body); err != nil {
			return err
		}
	}
	return nil
}

func StoreResponse(r io.Reader, id uint16) (uint16, error) {
	var cmd []byte
	for {
		kind, body, err := ReadPDU(r)
		if err != nil {
			return 0, err
		}
		if kind != 4 {
			return 0, errors.New("missing C-STORE response")
		}
		last := false
		for p := 0; p < len(body); {
			if p+6 > len(body) {
				return 0, errors.New("truncated response PDV")
			}
			n := int(binary.BigEndian.Uint32(body[p:]))
			if n < 2 || p+4+n > len(body) || body[p+4] != 1 || body[p+5]&1 == 0 {
				return 0, errors.New("invalid response PDV")
			}
			cmd = append(cmd, body[p+6:p+4+n]...)
			last = body[p+5]&2 != 0
			p += n + 4
		}
		if len(cmd) > 64*1024 {
			return 0, errors.New("oversized response command")
		}
		if !last {
			continue
		}
		fields := map[uint16]uint16{}
		for p := 0; p+8 <= len(cmd); {
			n := int(binary.LittleEndian.Uint32(cmd[p+4:]))
			if n < 0 || p+8+n > len(cmd) {
				return 0, errors.New("invalid response command")
			}
			if n == 2 {
				fields[binary.LittleEndian.Uint16(cmd[p+2:])] = binary.LittleEndian.Uint16(cmd[p+8:])
			}
			p += 8 + n
		}
		status, ok := fields[0x900]
		if !ok || fields[0x100] != 0x8001 || fields[0x120] != id || fields[0x800] != 0x0101 {
			return 0, errors.New("uncorrelated C-STORE response")
		}
		return status, nil
	}
}

type Metadata struct{ Class, Instance, Syntax string }

// Part10 leaves the reader positioned at the unchanged dataset. Only bounded
// file metadata is retained; callers hash the remaining stream incrementally.
func Part10(r *bufio.Reader) (Metadata, error) {
	var m Metadata
	preamble := make([]byte, 132)
	if _, err := io.ReadFull(r, preamble); err != nil {
		return m, err
	}
	if string(preamble[128:]) != "DICM" {
		return m, errors.New("missing Part 10 preamble")
	}
	for total := 0; total < 64*1024; {
		h, err := r.Peek(8)
		if err != nil {
			return m, err
		}
		if binary.LittleEndian.Uint16(h) != 2 {
			if m.Class == "" || m.Instance == "" || m.Syntax == "" {
				return m, errors.New("incomplete file metadata")
			}
			return m, nil
		}
		element, vr := binary.LittleEndian.Uint16(h[2:]), string(h[4:6])
		n, header := int(binary.LittleEndian.Uint16(h[6:])), 8
		if LongVR(vr) {
			h, err = r.Peek(12)
			if err != nil {
				return m, err
			}
			n, header = int(binary.LittleEndian.Uint32(h[8:])), 12
		}
		if n < 0 || n > 64*1024-total-header {
			return m, errors.New("oversized file metadata")
		}
		_, _ = r.Discard(header)
		v := make([]byte, n)
		if _, err := io.ReadFull(r, v); err != nil {
			return m, err
		}
		s := strings.TrimRight(string(v), "\x00 ")
		switch element {
		case 2:
			m.Class = s
		case 3:
			m.Instance = s
		case 0x10:
			m.Syntax = s
		}
		total += header + n
	}
	return m, errors.New("oversized file metadata")
}

func Frame(b []byte) []byte { return append(append([]byte{11}, b...), 28, 13) }

func ReadFrame(r *bufio.Reader, limit int) ([]byte, error) {
	start, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if start != 11 {
		return nil, errors.New("invalid MLLP start")
	}
	var b []byte
	for {
		chunk, err := r.ReadSlice(28)
		if len(b)+len(chunk) > limit+1 {
			return nil, errors.New("oversized MLLP frame")
		}
		b = append(b, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		end, err := r.ReadByte()
		if err != nil || end != 13 {
			return nil, errors.New("invalid MLLP terminator")
		}
		return b[:len(b)-1], nil
	}
}
