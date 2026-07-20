package nlscmodelset

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

func TestDecodeRawNodeAndMeshes(t *testing.T) {
	response := syntheticRawNode(t)

	node, err := DecodeRawNode(response, RawFormat{Version: 14, VertexSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if node.Level != 9 || node.Row != 164 || node.Col != 238 {
		t.Fatalf("unexpected node address: %d/%d/%d", node.Level, node.Row, node.Col)
	}
	if node.Offset != (Vec3{X: 1, Y: 2, Z: 3}) {
		t.Fatalf("unexpected offset: %+v", node.Offset)
	}
	if len(node.ChildrenIDs) != 2 || node.ChildrenIDs[0] != "1" || node.ChildrenIDs[1] != "4" {
		t.Fatalf("unexpected children: %#v", node.ChildrenIDs)
	}
	if len(node.EntityIDs) != 1 || node.EntityIDs[0] != 218004 {
		t.Fatalf("unexpected entities: %#v", node.EntityIDs)
	}
	if node.Resolution != 2.5 || len(node.DrawGroups) != 1 || len(node.MaskDrawGroups) != 0 {
		t.Fatalf("unexpected node payload: resolution=%g groups=%d masks=%d", node.Resolution, len(node.DrawGroups), len(node.MaskDrawGroups))
	}
	if string(node.AttributeTable) != "attrs" {
		t.Fatalf("unexpected attribute table: %q", node.AttributeTable)
	}

	meshes, err := node.DecodeMeshes()
	if err != nil {
		t.Fatal(err)
	}
	if len(meshes) != 1 {
		t.Fatalf("mesh count=%d, want 1", len(meshes))
	}
	mesh := meshes[0]
	if len(mesh.Positions) != 9 || mesh.Positions[3] != 1 || mesh.Positions[7] != 1 {
		t.Fatalf("unexpected positions: %#v", mesh.Positions)
	}
	if len(mesh.Indices) != 3 || mesh.Indices[2] != 2 {
		t.Fatalf("unexpected indices: %#v", mesh.Indices)
	}
	if len(mesh.FeatureIDs) != 3 || mesh.FeatureIDs[0] != 0 || len(mesh.EntityIDs) != 1 || mesh.EntityIDs[0] != 218004 {
		t.Fatalf("unexpected feature mapping: features=%#v entities=%#v", mesh.FeatureIDs, mesh.EntityIDs)
	}
	if !bytes.HasPrefix(mesh.Texture, pngSignature) || mesh.TextureMIMEType != "image/png" {
		t.Fatalf("unexpected texture: %x %q", mesh.Texture[:8], mesh.TextureMIMEType)
	}
}

func TestDecodeRawNodeRejectsTruncatedPayload(t *testing.T) {
	response := syntheticRawNode(t)
	response = response[:len(response)-3]
	if _, err := DecodeRawNode(response, RawFormat{Version: 14, VertexSize: 64}); err == nil {
		t.Fatal("expected truncated gzip error")
	}
}

func syntheticRawNode(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	write := func(value interface{}) {
		t.Helper()
		if err := binary.Write(&raw, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	write(int32(9))
	write(int32(164))
	write(int32(238))
	write(float64(1))
	write(float64(2))
	write(float64(3))
	for point := 0; point < 8; point++ {
		write(float64(point))
		write(float64(point + 10))
		write(float64(point + 20))
	}
	write(int32(3))
	write(int32(1))
	write(int32(0))
	write(int32(4))
	write(uint16(1))
	write(int32(218004))
	write(float64(2.5))
	for i := 0; i < 24; i++ { // boundary and frame-boundary points
		write(float64(i))
	}
	for i := 0; i < 12; i++ { // AABox and frame box
		write(float64(i))
	}

	write(int32(1)) // draw groups
	write(int32(1)) // textures
	write(uint8(1))
	write(uint16(1))
	write(uint16(1))
	textureImage := image.NewRGBA(image.Rect(0, 0, 1, 1))
	textureImage.SetRGBA(0, 0, color.RGBA{R: 255, G: 128, B: 64, A: 255})
	var texture bytes.Buffer
	if err := png.Encode(&texture, textureImage); err != nil {
		t.Fatal(err)
	}
	write(int32(texture.Len()))
	raw.Write(texture.Bytes())
	write(int32(1)) // draws
	write(int32(3)) // vertex count
	write(int32(3 * 64))
	positions := [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}}
	for _, position := range positions {
		vertex := make([]byte, 64)
		putFloat32(vertex[0:], position[0])
		putFloat32(vertex[4:], position[1])
		putFloat32(vertex[8:], position[2])
		putFloat32(vertex[20:], 1)
		copy(vertex[24:28], []byte{255, 128, 64, 255})
		putFloat32(vertex[28:], position[0])
		putFloat32(vertex[32:], position[1])
		putFloat32(vertex[36:], 0)
		putFloat32(vertex[40:], 0)
		putFloat32(vertex[44:], 1)
		putFloat32(vertex[48:], 1)
		binary.LittleEndian.PutUint16(vertex[54:], 0)
		raw.Write(vertex)
	}
	write(int32(3))
	write(uint16(0))
	write(uint16(1))
	write(uint16(2))
	write(int32(0)) // mask draw groups (version >= 9)
	write(uint32(4 + len("attrs")))
	raw.WriteString("attrs")

	var response bytes.Buffer
	zw := gzip.NewWriter(&response)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return response.Bytes()
}

func putFloat32(dst []byte, value float32) {
	binary.LittleEndian.PutUint32(dst, math.Float32bits(value))
}
