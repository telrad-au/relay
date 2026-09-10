package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

func retrievalElement(tag uint32, vr, value, syntax string) []byte {
	padding := byte(' ')
	if vr == "UI" {
		padding = 0
	}
	v := paddedDICOMString(value, padding)
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint16(tag>>16))
	binary.Write(&b, binary.LittleEndian, uint16(tag))
	if syntax == implicitLittleEndian {
		binary.Write(&b, binary.LittleEndian, uint32(len(v)))
	} else {
		b.WriteString(vr)
		if vr == "SQ" {
			b.Write([]byte{0, 0})
			binary.Write(&b, binary.LittleEndian, uint32(len(v)))
		} else {
			binary.Write(&b, binary.LittleEndian, uint16(len(v)))
		}
	}
	b.Write(v)
	return b.Bytes()
}
func retrievalIdentifier(p retrieval.Permit, study, syntax string) []byte {
	var b []byte
	if study == "" {
		b = append(b, retrievalElement(0x00080050, "SH", p.Examination.Accession, syntax)...)
		b = append(b, retrievalElement(0x00080051, "SQ", "", syntax)...)
	}
	b = append(b, retrievalElement(0x00080052, "CS", "STUDY", syntax)...)
	b = append(b, retrievalElement(0x0020000d, "UI", study, syntax)...)
	return b
}
func retrievalResponse(fields map[uint16][]byte, class string, expected uint16) (uint16, bool, error) {
	field, ok := commandUS(fields, 0x0100)
	if !ok || field != expected {
		return 0, false, retrieval.ErrPartial
	}
	message, ok := commandUS(fields, 0x0120)
	if !ok || message != 1 {
		return 0, false, retrieval.ErrPartial
	}
	dataset, ok := commandUS(fields, 0x0800)
	if !ok {
		return 0, false, retrieval.ErrPartial
	}
	status, ok := commandUS(fields, 0x0900)
	if !ok || commandText(fields, 2) != class {
		return 0, false, retrieval.ErrPartial
	}
	return status, dataset != 0x0101, nil
}
func queryDIMSEAccession(parent context.Context, pacs retrievalPACS, p retrieval.Permit) ([]string, error) {
	// Basic study-root matching uses the accession, never a patient identifier.
	// Keep this profile to the default DICOM repertoire and SH length.
	if len(p.Examination.Accession) > 16 {
		return nil, retrieval.ErrPolicy
	}
	for _, c := range p.Examination.Accession {
		if c < 32 || c > 126 {
			return nil, retrieval.ErrPolicy
		}
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(pacs.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	a, err := openRetrievalAssociation(ctx, pacs, false)
	if err != nil {
		return nil, err
	}
	defer a.close()
	if err = a.send(1, true, retrievalRequest(studyRootFind, 0x0020)); err != nil {
		return nil, err
	}
	if err = a.send(1, false, retrievalIdentifier(p, "", a.contexts[1].TransferSyntax)); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	uids := []string{}
	for {
		id, fields, e := a.command()
		if e != nil {
			return nil, e
		}
		if id != 1 {
			return nil, retrieval.ErrPartial
		}
		status, dataset, e := retrievalResponse(fields, studyRootFind, 0x8020)
		if e != nil {
			return nil, e
		}
		switch status {
		case 0xff00, 0xff01:
			if !dataset {
				return nil, retrieval.ErrPartial
			}
			data, e := io.ReadAll(&retrievalDatasetReader{a: a, id: 1, limit: 1024 * 1024})
			if e != nil {
				return nil, e
			}
			uid, e := retrieval.StudyIdentifier(data, a.contexts[1].TransferSyntax, p)
			if e != nil {
				return nil, e
			}
			if seen[uid] {
				return nil, errors.New("ambiguous_identity")
			}
			seen[uid] = true
			uids = append(uids, uid)
			if len(uids) > 64 {
				return nil, retrieval.ErrPolicy
			}
		case 0:
			if dataset {
				return nil, retrieval.ErrPartial
			}
			if e := a.release(); e != nil {
				return nil, e
			}
			if len(uids) == 0 {
				return nil, errors.New("not_found")
			}
			slices.Sort(uids)
			return uids, nil
		default:
			return nil, errors.New("pacs_rejected")
		}
	}
}

func retrieveCGET(parent context.Context, cloud *http.Client, cfg *config, provider *credentialProvider, status *runtimeStatusManager, pacs retrievalPACS, p retrieval.Permit, study, attempt string, recheck func() error, progress *retrievalProgress) (retrievalStudyResult, error) {
	result := retrievalStudyResult{StudyInstanceUID: study, RetrieveStatus: "success", Completion: retrievalCompletion{Status: "not_provided"}}
	if e := recheck(); e != nil {
		return result, e
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(pacs.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	a, e := openRetrievalAssociation(ctx, pacs, true)
	if e != nil {
		return result, e
	}
	defer a.close()
	if e = a.send(1, true, retrievalRequest(studyRootGet, 0x0010)); e != nil {
		return result, e
	}
	if e = a.send(1, false, retrievalIdentifier(p, study, a.contexts[1].TransferSyntax)); e != nil {
		return result, e
	}
	inventory := map[string]bool{}
	arrivals := 0
	var totalBytes int64
	for {
		id, fields, e := a.command()
		if e != nil {
			return result, e
		}
		field, ok := commandUS(fields, 0x0100)
		if !ok {
			return result, retrieval.ErrPartial
		}
		if field == 0x0001 {
			message, haveMessage := commandUS(fields, 0x0110)
			dataset, haveDataset := commandUS(fields, 0x0800)
			class, sop := commandText(fields, 2), commandText(fields, 0x1000)
			pc := a.contexts[id]
			if id == 1 || !haveMessage || !haveDataset || dataset == 0x0101 || !retrieval.UID(sop) || class != pc.AbstractSyntax {
				return result, retrieval.ErrPartial
			}
			command := dicomCommand{Field: field, MessageID: message, SOPClassUID: class, SOPInstance: sop, HasDataset: true}
			arrivals++
			progress.received.Add(1)
			if arrivals > pacs.MaxInstances || arrivals > 65535 {
				return result, retrieval.ErrPolicy
			}
			if e = recheck(); e != nil {
				return result, e
			}
			header := buildPart10Header(class, sop, pc.TransferSyntax)
			limit := min(pacs.MaxInstanceBytes, pacs.MaxStudyBytes-totalBytes) - int64(len(header))
			if limit <= 0 {
				return result, retrieval.ErrPolicy
			}
			reader := &retrievalDatasetReader{a: a, id: id, limit: limit}
			// Part 10 metadata records the negotiated syntax; dataset bytes are copied
			// verbatim. Identity is checked before opening the cloud upload.
			stream, e := retrieval.OpenDIMSEDICOM(io.MultiReader(bytes.NewReader(header), reader), p, study, pacs.MaxInstanceBytes)
			if e == nil {
				e = uploadRetrievalStream(ctx, cloud, cfg, provider, status, stream, attempt)
			}
			totalBytes += int64(len(header)) + reader.bytes
			if e == nil {
				e = recheck()
			}
			if e != nil {
				_ = a.send(id, true, buildDIMSEStoreResponse(command, 0x8001, 0xA900))
				return result, e
			}
			progress.uploaded.Add(1)
			inventory[sop] = true
			if e = a.send(id, true, buildDIMSEStoreResponse(command, 0x8001, 0)); e != nil {
				return result, e
			}
			continue
		}
		if id != 1 {
			return result, retrieval.ErrPartial
		}
		response, hasDataset, e := retrievalResponse(fields, studyRootGet, 0x8010)
		if e != nil {
			return result, e
		}
		if response != 0 && response != 0xff00 {
			return result, retrieval.ErrPartial
		}
		if hasDataset {
			return result, retrieval.ErrPartial
		}
		for _, tag := range []uint16{0x1020, 0x1021, 0x1022, 0x1023} {
			v, present := commandUS(fields, tag)
			if _, exists := fields[tag]; exists && !present {
				return result, retrieval.ErrPartial
			}
			if response == 0xff00 && !present {
				return result, retrieval.ErrPartial
			}
			if present && ((tag == 0x1021 && int(v) != arrivals) || (tag == 0x1022 && v != 0) || (tag == 0x1023 && v != 0) || (tag == 0x1020 && response == 0 && v != 0)) {
				return result, retrieval.ErrPartial
			}
		}
		if response == 0xff00 {
			continue
		}
		if len(inventory) == 0 {
			return result, retrieval.ErrPartial
		}
		if e = recheck(); e != nil {
			return result, e
		}
		if e = a.release(); e != nil {
			return result, e
		}
		result.UniqueInstanceCount = len(inventory)
		result.InventorySHA256 = inventoryDigest(inventory)
		return result, nil
	}
}
func uploadRetrievalStream(ctx context.Context, cloud *http.Client, cfg *config, provider *credentialProvider, status *runtimeStatusManager, stream *retrieval.DICOMStream, attempt string) error {
	reader, writer := io.Pipe()
	response := make(chan dicomIngestResult, 1)
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(child, func() { reader.CloseWithError(context.Canceled); writer.CloseWithError(context.Canceled) })
	defer stop()
	go ingestDICOMWithAttempt(child, cfg.DicomURL, cloud, provider, status, reader, response, attempt)
	_, err := stream.WriteTo(writer)
	writer.CloseWithError(err)
	receipt := <-response
	if err != nil {
		return err
	}
	if receipt.status != 0 || ctx.Err() != nil {
		return retrieval.ErrPartial
	}
	return nil
}
