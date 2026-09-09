package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

const studyRootFind = "1.2.840.10008.5.1.4.1.2.2.1"
const studyRootGet = "1.2.840.10008.5.1.4.1.2.2.3"
const implicitLittleEndian = "1.2.840.10008.1.2"
const explicitLittleEndian = "1.2.840.10008.1.2.1"

// Both DIMSE operations and their storage suboperations remain on locally
// initiated sockets. No C-MOVE destination or inbound listener is involved.
type retrievalAssociation struct {
	conn     net.Conn
	contexts map[byte]presentationContext
	pending  []presentationDataValue
	maxSend  int
	stop     func() bool
	released bool
}

type retrievalContextOffer struct {
	class    string
	syntaxes []string
	storage  bool
}

func dimseNetworkError(err error) error {
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return errors.New("network_timeout")
	}
	return errors.New("pacs_unavailable")
}

func openRetrievalAssociation(ctx context.Context, pacs retrievalPACS, get bool) (*retrievalAssociation, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(pacs.Host, strconv.Itoa(pacs.Port)))
	if err != nil {
		return nil, dimseNetworkError(err)
	}
	a := &retrievalAssociation{conn: conn, contexts: map[byte]presentationContext{}, maxSend: 65536}
	a.stop = context.AfterFunc(ctx, func() { conn.Close() })
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	offers := map[byte]retrievalContextOffer{1: {class: studyRootFind, syntaxes: []string{explicitLittleEndian, implicitLittleEndian}}}
	if get {
		offers[1] = retrievalContextOffer{class: studyRootGet, syntaxes: []string{explicitLittleEndian, implicitLittleEndian}}
		for i, class := range pacs.StorageSOPClasses {
			offers[byte(3+4*i)] = retrievalContextOffer{class: class, syntaxes: []string{explicitLittleEndian}, storage: true}
			offers[byte(5+4*i)] = retrievalContextOffer{class: class, syntaxes: []string{implicitLittleEndian}, storage: true}
		}
	}
	body := make([]byte, 68)
	binary.BigEndian.PutUint16(body, 1)
	copy(body[4:20], strings.Repeat(" ", 16))
	copy(body[20:36], strings.Repeat(" ", 16))
	copy(body[4:20], pacs.CalledAETitle)
	copy(body[20:36], pacs.CallingAETitle)
	body = append(body, associationItem(0x10, []byte("1.2.840.10008.3.1.1.1"))...)
	user := associationItem(0x51, []byte{0, 16, 0, 0}) // 1 MiB receive cap.
	user = append(user, associationItem(0x52, []byte(implementationUID))...)
	user = append(user, associationItem(0x55, []byte(implementationName))...)
	rolesOffered := map[string]bool{}
	for index := 1; index <= 2*len(offers)-1; index += 2 {
		id := byte(index)
		offer := offers[id]
		value := append([]byte{id, 0, 0, 0}, associationItem(0x30, []byte(offer.class))...)
		for _, ts := range offer.syntaxes {
			value = append(value, associationItem(0x40, []byte(ts))...)
		}
		body = append(body, associationItem(0x20, value)...)
		if offer.storage && !rolesOffered[offer.class] {
			rolesOffered[offer.class] = true
			role := make([]byte, 2)
			binary.BigEndian.PutUint16(role, uint16(len(offer.class)))
			role = append(role, []byte(offer.class)...)
			role = append(role, 0, 1) // requestor is Storage SCP only.
			user = append(user, associationItem(0x54, role)...)
		}
	}
	body = append(body, associationItem(0x50, user)...)
	if err = writeDICOMPDU(conn, 0x01, body); err == nil {
		var kind byte
		var response []byte
		kind, response, err = readDICOMPDU(conn)
		if err == nil {
			if kind != 0x02 {
				err = errors.New("pacs_rejected")
			} else {
				err = a.accept(response, offers)
			}
		} else {
			err = dimseNetworkError(err)
		}
	}
	if err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

func associationItems(data []byte, visit func(byte, []byte) error) error {
	for len(data) > 0 {
		if len(data) < 4 {
			return retrieval.ErrPartial
		}
		n := int(binary.BigEndian.Uint16(data[2:4]))
		if n > len(data)-4 {
			return retrieval.ErrPartial
		}
		if err := visit(data[0], data[4:4+n]); err != nil {
			return err
		}
		data = data[4+n:]
	}
	return nil
}
func (a *retrievalAssociation) accept(body []byte, offers map[byte]retrievalContextOffer) error {
	if len(body) < 68 || binary.BigEndian.Uint16(body) != 1 {
		return retrieval.ErrPartial
	}
	app, userSeen, maxSeen := false, false, false
	seen := map[byte]bool{}
	roles := map[string]bool{}
	err := associationItems(body[68:], func(kind byte, value []byte) error {
		switch kind {
		case 0x10:
			if app || string(value) != "1.2.840.10008.3.1.1.1" {
				return retrieval.ErrPartial
			}
			app = true
		case 0x21:
			if len(value) < 4 {
				return retrieval.ErrPartial
			}
			id := value[0]
			offer, ok := offers[id]
			if !ok || seen[id] || value[2] > 4 {
				return retrieval.ErrPartial
			}
			seen[id] = true
			if value[2] != 0 {
				return nil
			}
			var syntax string
			if err := associationItems(value[4:], func(k byte, v []byte) error {
				if k != 0x40 || syntax != "" {
					return retrieval.ErrPartial
				}
				syntax = string(v)
				return nil
			}); err != nil {
				return err
			}
			matched := false
			for _, ts := range offer.syntaxes {
				if ts == syntax {
					matched = true
				}
			}
			if !matched {
				return retrieval.ErrPartial
			}
			a.contexts[id] = presentationContext{ID: id, AbstractSyntax: offer.class, TransferSyntax: syntax, Accepted: true}
		case 0x50:
			if userSeen {
				return retrieval.ErrPartial
			}
			userSeen = true
			return associationItems(value, func(k byte, v []byte) error {
				switch k {
				case 0x51:
					if maxSeen || len(v) != 4 {
						return retrieval.ErrPartial
					}
					maxSeen = true
					n := binary.BigEndian.Uint32(v)
					if n != 0 {
						if n < 1024 {
							return retrieval.ErrPolicy
						}
						a.maxSend = int(min(n, 65536))
					}
				case 0x54:
					if len(v) < 4 {
						return retrieval.ErrPartial
					}
					n := int(binary.BigEndian.Uint16(v))
					if len(v) != n+4 {
						return retrieval.ErrPartial
					}
					class := string(v[2 : 2+n])
					// Some PACS repeat identical role selections for the two storage
					// contexts. Conflicting selections still invalidate the association.
					if prior, exists := roles[class]; exists && prior != (v[n+3] == 1) {
						return retrieval.ErrPartial
					}
					known := false
					for _, o := range offers {
						if o.storage && o.class == class {
							known = true
						}
					}
					if !known || v[n+2] != 0 || v[n+3] > 1 {
						return retrieval.ErrPartial
					}
					roles[class] = v[n+3] == 1
				case 0x53:
					if len(v) != 4 || binary.BigEndian.Uint16(v) != 1 || binary.BigEndian.Uint16(v[2:]) != 1 {
						return retrieval.ErrPartial
					}
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !app || !maxSeen || len(seen) != len(offers) || !a.contexts[1].Accepted {
		return errors.New("pacs_rejected")
	}
	storage := 0
	for id, c := range a.contexts {
		if offers[id].storage {
			if !roles[c.AbstractSyntax] {
				delete(a.contexts, id)
			} else {
				storage++
			}
		}
	}
	if offers[1].class == studyRootGet && storage == 0 {
		return errors.New("pacs_rejected")
	}
	return nil
}
func (a *retrievalAssociation) close() {
	a.stop()
	if !a.released {
		a.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		_ = writeAbort(a.conn, 0, 0)
	}
	a.conn.Close()
}
func (a *retrievalAssociation) release() error {
	if len(a.pending) != 0 {
		return retrieval.ErrPartial
	}
	if err := writeDICOMPDU(a.conn, 0x05, make([]byte, 4)); err != nil {
		return dimseNetworkError(err)
	}
	kind, body, err := readDICOMPDU(a.conn)
	if err != nil {
		return dimseNetworkError(err)
	}
	if kind != 0x06 || len(body) != 4 {
		return retrieval.ErrPartial
	}
	a.released = true
	return nil
}
func (a *retrievalAssociation) send(id byte, command bool, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), a.maxSend-6)
		v := make([]byte, 6+n)
		binary.BigEndian.PutUint32(v, uint32(n+2))
		v[4] = id
		if command {
			v[5] = 1
		}
		if n == len(data) {
			v[5] |= 2
		}
		copy(v[6:], data[:n])
		if err := writeDICOMPDU(a.conn, 0x04, v); err != nil {
			return dimseNetworkError(err)
		}
		data = data[n:]
	}
	return nil
}
func (a *retrievalAssociation) next() (presentationDataValue, error) {
	if len(a.pending) == 0 {
		kind, body, err := readDICOMPDU(a.conn)
		if err != nil {
			return presentationDataValue{}, dimseNetworkError(err)
		}
		if kind != 0x04 {
			return presentationDataValue{}, retrieval.ErrPartial
		}
		a.pending, err = parsePDVs(body)
		if err != nil || len(a.pending) == 0 {
			return presentationDataValue{}, retrieval.ErrPartial
		}
	}
	v := a.pending[0]
	a.pending = a.pending[1:]
	if !a.contexts[v.contextID].Accepted {
		return v, retrieval.ErrPartial
	}
	return v, nil
}
func (a *retrievalAssociation) command() (byte, map[uint16][]byte, error) {
	var b bytes.Buffer
	var id byte
	for {
		v, err := a.next()
		if err != nil {
			return 0, nil, err
		}
		if !v.command || (id != 0 && id != v.contextID) || b.Len()+len(v.data) > maxDIMSECommandBytes {
			return 0, nil, retrieval.ErrPartial
		}
		id = v.contextID
		b.Write(v.data)
		if v.last {
			break
		}
	}
	fields := map[uint16][]byte{}
	data := b.Bytes()
	offset := 0
	for offset < len(data) {
		if offset+8 > len(data) || binary.LittleEndian.Uint16(data[offset:]) != 0 {
			return 0, nil, retrieval.ErrPartial
		}
		tag := binary.LittleEndian.Uint16(data[offset+2:])
		n := int(binary.LittleEndian.Uint32(data[offset+4:]))
		offset += 8
		if n > len(data)-offset || n%2 != 0 {
			return 0, nil, retrieval.ErrPartial
		}
		if _, ok := fields[tag]; ok {
			return 0, nil, retrieval.ErrPartial
		}
		fields[tag] = data[offset : offset+n]
		offset += n
	}
	group := fields[0]
	if len(group) != 4 || int(binary.LittleEndian.Uint32(group)) != len(data)-12 {
		return 0, nil, retrieval.ErrPartial
	}
	return id, fields, nil
}
func commandUS(fields map[uint16][]byte, tag uint16) (uint16, bool) {
	v := fields[tag]
	if len(v) != 2 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(v), true
}
func commandText(fields map[uint16][]byte, tag uint16) string {
	return strings.TrimRight(string(fields[tag]), "\x00 ")
}
func retrievalRequest(class string, field uint16) []byte {
	var b bytes.Buffer
	writeCommandUI(&b, 2, class)
	writeCommandUS(&b, 0x0100, field)
	writeCommandUS(&b, 0x0110, 1)
	writeCommandUS(&b, 0x0700, 0)
	writeCommandUS(&b, 0x0800, 0x0102)
	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[4:], 4)
	binary.LittleEndian.PutUint32(header[8:], uint32(b.Len()))
	return append(header, b.Bytes()...)
}

type retrievalDatasetReader struct {
	a     *retrievalAssociation
	id    byte
	data  []byte
	last  bool
	bytes int64
	limit int64
}

func (r *retrievalDatasetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.data) == 0 {
		if r.last {
			return 0, io.EOF
		}
		v, e := r.a.next()
		if e != nil {
			return 0, e
		}
		if v.command || v.contextID != r.id {
			return 0, retrieval.ErrPartial
		}
		r.bytes += int64(len(v.data))
		if r.bytes > r.limit {
			return 0, retrieval.ErrPolicy
		}
		r.data = v.data
		r.last = v.last
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
