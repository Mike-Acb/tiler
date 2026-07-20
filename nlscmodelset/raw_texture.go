package nlscmodelset

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// ExtractTextureFromResponse extracts the original compressed image embedded
// in an NLSC MODELSET NODE response. Both HTTP gzip bodies and decoded bodies
// are accepted.
func ExtractTextureFromResponse(response []byte) ([]byte, string, error) {
	body, err := decodeGzipResponse(response)
	if err != nil {
		return nil, "", err
	}
	start := bytes.Index(body, pngSignature)
	if start < 0 {
		return extractJPEG(body)
	}
	position := start + len(pngSignature)
	for position+12 <= len(body) {
		length := int(binary.BigEndian.Uint32(body[position : position+4]))
		end := position + 12 + length
		if length < 0 || end > len(body) {
			return nil, "", errors.New("truncated PNG texture")
		}
		chunkType := string(body[position+4 : position+8])
		position = end
		if chunkType == "IEND" {
			return append([]byte(nil), body[start:end]...), "image/png", nil
		}
	}
	return nil, "", errors.New("PNG IEND chunk not found")
}

func extractJPEG(body []byte) ([]byte, string, error) {
	start := bytes.Index(body, []byte{0xff, 0xd8, 0xff})
	if start < 0 {
		return nil, "", errors.New("embedded PNG or JPEG texture not found")
	}
	relativeEnd := bytes.Index(body[start+3:], []byte{0xff, 0xd9})
	if relativeEnd < 0 {
		return nil, "", errors.New("JPEG end marker not found")
	}
	end := start + 3 + relativeEnd + 2
	return append([]byte(nil), body[start:end]...), "image/jpeg", nil
}

func decodeGzipResponse(response []byte) ([]byte, error) {
	if len(response) < 2 || response[0] != 0x1f || response[1] != 0x8b {
		return response, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(response))
	if err != nil {
		return nil, fmt.Errorf("open gzip response: %w", err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("decompress response: %w", err)
	}
	return body, nil
}
