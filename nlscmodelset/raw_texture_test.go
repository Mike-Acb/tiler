package nlscmodelset

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"testing"
)

func TestExtractTextureFromResponseFindsPNGInGzipNode(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	writer.Write(append(append([]byte("node-prefix"), png...), []byte("node-suffix")...))
	writer.Close()

	texture, mimeType, err := ExtractTextureFromResponse(compressed.Bytes())
	if err != nil {
		t.Fatalf("ExtractTextureFromResponse() error = %v", err)
	}
	if mimeType != "image/png" {
		t.Fatalf("MIME type = %q", mimeType)
	}
	if !bytes.Equal(texture, png) {
		t.Fatalf("extracted PNG has %d bytes, want %d", len(texture), len(png))
	}
}

func TestExtractTextureFromResponseFindsJPEGInDecodedNode(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x04, 0x11, 0x22, 0xff, 0xd9}
	response := append(append([]byte("node-prefix"), jpeg...), []byte("node-suffix")...)

	texture, mimeType, err := ExtractTextureFromResponse(response)
	if err != nil {
		t.Fatalf("ExtractTextureFromResponse() error = %v", err)
	}
	if mimeType != "image/jpeg" {
		t.Fatalf("MIME type = %q", mimeType)
	}
	if !bytes.Equal(texture, jpeg) {
		t.Fatalf("extracted JPEG = %v, want %v", texture, jpeg)
	}
}
