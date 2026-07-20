package nlscmodelset

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeCaptureReadsPilotGaeaInterleavedGeometry(t *testing.T) {
	capture := syntheticCapture(t)

	mesh, err := DecodeCapture(capture)
	if err != nil {
		t.Fatalf("DecodeCapture() error = %v", err)
	}

	if got, want := len(mesh.Positions)/3, 3; got != want {
		t.Fatalf("vertex count = %d, want %d", got, want)
	}
	if got, want := len(mesh.Indices), 3; got != want {
		t.Fatalf("index count = %d, want %d", got, want)
	}
	if got, want := mesh.Positions, []float32{0, 0, 0, 10, 0, 0, 0, 20, 5}; !equalFloat32s(got, want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	if got, want := mesh.Colors, []byte{255, 0, 0, 255, 0, 255, 0, 255, 0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("colors = %v, want %v", got, want)
	}
	if got, want := mesh.Indices, []uint16{0, 1, 2}; !equalUint16s(got, want) {
		t.Fatalf("indices = %v, want %v", got, want)
	}
	if mesh.Min != ([3]float32{0, 0, 0}) || mesh.Max != ([3]float32{10, 20, 5}) {
		t.Fatalf("bounds = %v..%v", mesh.Min, mesh.Max)
	}
}

func TestCapturePreservesHierarchyCoordinates(t *testing.T) {
	capture := syntheticCapture(t)
	capture.Level = 8
	capture.Row = 82
	capture.Col = 121
	capture.ChildrenIDs = []string{"9_164_242", "9_164_243", "9_165_242", "9_165_243"}

	encoded, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Capture
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Level != 8 || decoded.Row != 82 || decoded.Col != 121 {
		t.Fatalf("coordinates = %d/%d/%d", decoded.Level, decoded.Row, decoded.Col)
	}
	if got, want := decoded.ChildrenIDs, capture.ChildrenIDs; len(got) != len(want) || got[0] != want[0] || got[3] != want[3] {
		t.Fatalf("children = %v, want %v", got, want)
	}
}

func TestDecodeCaptureReconstructsAtlasTextureCoordinates(t *testing.T) {
	capture := syntheticCapture(t)
	vertices, err := base64.StdEncoding.DecodeString(capture.VertexBase64)
	if err != nil {
		t.Fatal(err)
	}
	uvs := [][2]float32{{4, -1}, {0.25, -0.25}, {1, 0}}
	region := [4]float32{0.1, 0.2, 0.5, 0.8}
	for vertex := range uvs {
		base := vertex * capture.VertexSize
		for component, value := range uvs[vertex] {
			binary.LittleEndian.PutUint32(vertices[base+28+component*4:], math.Float32bits(value))
		}
		for component, value := range region {
			binary.LittleEndian.PutUint32(vertices[base+36+component*4:], math.Float32bits(value))
		}
	}
	capture.VertexBase64 = base64.StdEncoding.EncodeToString(vertices)

	mesh, err := DecodeCapture(capture)
	if err != nil {
		t.Fatalf("DecodeCapture() error = %v", err)
	}
	want := []float32{0.1, 0.2, 0.2, 0.65, 0.1, 0.2}
	if !closeFloat32s(mesh.TexCoords, want, 1e-6) {
		t.Fatalf("texture coordinates = %v, want %v", mesh.TexCoords, want)
	}
	if got, want := mesh.RawTexCoords, []float32{4, -1, 0.25, -0.25, 1, 0}; !closeFloat32s(got, want, 1e-6) {
		t.Fatalf("raw texture coordinates = %v, want %v", got, want)
	}
	wantRegions := []float32{
		0.1, 0.2, 0.5, 0.8,
		0.1, 0.2, 0.5, 0.8,
		0.1, 0.2, 0.5, 0.8,
	}
	if !closeFloat32s(mesh.UVRegions, wantRegions, 1e-6) {
		t.Fatalf("UV regions = %v, want %v", mesh.UVRegions, wantRegions)
	}
}

func TestDecodeCaptureMapsEntityIDsToFeatureIDs(t *testing.T) {
	capture := syntheticCapture(t)
	capture.EntityIDs = []uint32{218004, 218038}
	vertices, err := base64.StdEncoding.DecodeString(capture.VertexBase64)
	if err != nil {
		t.Fatal(err)
	}
	for vertex, entityID := range []uint32{218004, 218004, 218038} {
		binary.LittleEndian.PutUint32(vertices[vertex*capture.VertexSize+64:], entityID)
	}
	capture.VertexBase64 = base64.StdEncoding.EncodeToString(vertices)

	mesh, err := DecodeCapture(capture)
	if err != nil {
		t.Fatalf("DecodeCapture() error = %v", err)
	}
	if got, want := mesh.FeatureIDs, []uint16{0, 0, 1}; !equalUint16s(got, want) {
		t.Fatalf("feature IDs = %v, want %v", got, want)
	}
	if got, want := mesh.EntityIDs, []uint32{218004, 218038}; !equalUint32s(got, want) {
		t.Fatalf("entity IDs = %v, want %v", got, want)
	}
}

func TestBuildGLBProducesValidGLTF2Container(t *testing.T) {
	mesh, err := DecodeCapture(syntheticCapture(t))
	if err != nil {
		t.Fatal(err)
	}

	glb, err := BuildGLB(mesh)
	if err != nil {
		t.Fatalf("BuildGLB() error = %v", err)
	}
	if string(glb[:4]) != "glTF" {
		t.Fatalf("magic = %q", glb[:4])
	}
	if got := binary.LittleEndian.Uint32(glb[4:8]); got != 2 {
		t.Fatalf("GLB version = %d, want 2", got)
	}
	if got := int(binary.LittleEndian.Uint32(glb[8:12])); got != len(glb) {
		t.Fatalf("declared length = %d, actual = %d", got, len(glb))
	}

	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	if string(glb[16:20]) != "JSON" {
		t.Fatalf("first chunk type = %q", glb[16:20])
	}
	var doc struct {
		Asset struct {
			Version string `json:"version"`
		} `json:"asset"`
		Accessors []struct {
			ComponentType int  `json:"componentType"`
			Count         int  `json:"count"`
			Normalized    bool `json:"normalized"`
		} `json:"accessors"`
		Buffers []struct {
			ByteLength int `json:"byteLength"`
		} `json:"buffers"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &doc); err != nil {
		t.Fatalf("decode GLB JSON: %v", err)
	}
	if doc.Asset.Version != "2.0" {
		t.Fatalf("asset version = %q", doc.Asset.Version)
	}
	if got, want := doc.Accessors[0].Count, 3; got != want {
		t.Fatalf("POSITION count = %d, want %d", got, want)
	}
	if got, want := doc.Accessors[3].ComponentType, 5123; got != want {
		t.Fatalf("index component type = %d, want uint16 (%d)", got, want)
	}
	if got, want := doc.Accessors[3].Count, 3; got != want {
		t.Fatalf("index count = %d, want %d", got, want)
	}
	if !doc.Accessors[2].Normalized {
		t.Fatal("COLOR_0 accessor is not normalized")
	}

	binHeader := 20 + jsonLength
	binLength := int(binary.LittleEndian.Uint32(glb[binHeader : binHeader+4]))
	if string(glb[binHeader+4:binHeader+8]) != "BIN\x00" {
		t.Fatalf("second chunk type = %q", glb[binHeader+4:binHeader+8])
	}
	if got := doc.Buffers[0].ByteLength; got != binLength {
		t.Fatalf("buffer byteLength = %d, BIN chunk = %d", got, binLength)
	}
}

func TestBuildGLBEmbedsAtlasTextureAndUsesTextureCoordinates(t *testing.T) {
	mesh, err := DecodeCapture(syntheticCapture(t))
	if err != nil {
		t.Fatal(err)
	}
	texture, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	mesh.Texture = texture
	mesh.TextureMIMEType = "image/png"

	glb, err := BuildGLB(mesh)
	if err != nil {
		t.Fatalf("BuildGLB() error = %v", err)
	}
	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	var doc struct {
		Meshes []struct {
			Primitives []struct {
				Attributes map[string]int `json:"attributes"`
			} `json:"primitives"`
		} `json:"meshes"`
		Materials []struct {
			PBR struct {
				BaseColorTexture struct {
					Index int `json:"index"`
				} `json:"baseColorTexture"`
			} `json:"pbrMetallicRoughness"`
		} `json:"materials"`
		Images []struct {
			BufferView int    `json:"bufferView"`
			MIMEType   string `json:"mimeType"`
		} `json:"images"`
		Textures []struct {
			Source int `json:"source"`
		} `json:"textures"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &doc); err != nil {
		t.Fatal(err)
	}
	attributes := doc.Meshes[0].Primitives[0].Attributes
	if got, ok := attributes["TEXCOORD_0"]; !ok || got != 4 {
		t.Fatalf("TEXCOORD_0 accessor = %d, present = %v", got, ok)
	}
	if _, ok := attributes["COLOR_0"]; ok {
		t.Fatal("textured primitive should not multiply the atlas by COLOR_0")
	}
	if len(doc.Images) != 1 || doc.Images[0].MIMEType != "image/png" {
		t.Fatalf("images = %+v", doc.Images)
	}
	if len(doc.Textures) != 1 || doc.Textures[0].Source != 0 {
		t.Fatalf("textures = %+v", doc.Textures)
	}
	if doc.Materials[0].PBR.BaseColorTexture.Index != 0 {
		t.Fatalf("base color texture index = %d", doc.Materials[0].PBR.BaseColorTexture.Index)
	}
}

func TestBuildGLBSplitsAtlasRegionsIntoRepeatingTextures(t *testing.T) {
	texture := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			texture.Set(x, y, color.RGBA{R: 255, A: 255})
		}
		for x := 2; x < 4; x++ {
			texture.Set(x, y, color.RGBA{B: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, texture); err != nil {
		t.Fatal(err)
	}
	mesh := Mesh{
		Positions:    []float32{0, 0, 0, 1, 0, 0, 0, 1, 0, 2, 0, 0, 3, 0, 0, 2, 1, 0},
		Normals:      []float32{0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1},
		Colors:       bytes.Repeat([]byte{255, 255, 255, 255}, 6),
		TexCoords:    []float32{0, 0, 1, 0, 0, 1, 0.5, 0, 1, 0, 0.5, 1},
		RawTexCoords: []float32{0, 0, 2, 0, 0, 1, 0, 0, 3, 0, 0, 1},
		UVRegions: []float32{
			0, 0, 0.5, 1, 0, 0, 0.5, 1, 0, 0, 0.5, 1,
			0.5, 0, 1, 1, 0.5, 0, 1, 1, 0.5, 0, 1, 1,
		},
		Indices:         []uint16{0, 1, 2, 3, 4, 5},
		Texture:         encoded.Bytes(),
		TextureMIMEType: "image/png",
		Min:             [3]float32{0, 0, 0},
		Max:             [3]float32{3, 1, 0},
	}

	glb, err := BuildGLB(mesh)
	if err != nil {
		t.Fatalf("BuildGLB() error = %v", err)
	}
	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	var doc struct {
		Meshes []struct {
			Primitives []struct {
				Material int `json:"material"`
			} `json:"primitives"`
		} `json:"meshes"`
		Images    []json.RawMessage `json:"images"`
		Materials []json.RawMessage `json:"materials"`
		Samplers  []struct {
			WrapS int `json:"wrapS"`
			WrapT int `json:"wrapT"`
		} `json:"samplers"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &doc); err != nil {
		t.Fatal(err)
	}
	if got := len(doc.Meshes[0].Primitives); got != 2 {
		t.Fatalf("primitive count = %d, want 2", got)
	}
	if len(doc.Images) != 2 || len(doc.Materials) != 2 {
		t.Fatalf("images/materials = %d/%d, want 2/2", len(doc.Images), len(doc.Materials))
	}
	if len(doc.Samplers) != 1 || doc.Samplers[0].WrapS != 10497 || doc.Samplers[0].WrapT != 10497 {
		t.Fatalf("samplers = %+v", doc.Samplers)
	}
}

func TestBuildGLBUsesVertexColorsForTrianglesWithoutAtlasRegion(t *testing.T) {
	texture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	draw.Draw(texture, texture.Bounds(), &image.Uniform{C: color.RGBA{R: 220, G: 80, B: 30, A: 255}}, image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, texture); err != nil {
		t.Fatal(err)
	}
	mesh := Mesh{
		Positions:    []float32{0, 0, 0, 1, 0, 0, 0, 1, 0, 2, 0, 0, 3, 0, 0, 2, 1, 0},
		Normals:      []float32{0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1},
		Colors:       bytes.Repeat([]byte{40, 180, 80, 255}, 6),
		TexCoords:    []float32{0, 0, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0},
		RawTexCoords: []float32{0, 0, 2, 0, 0, 1, 0, 0, 0, 0, 0, 0},
		UVRegions: []float32{
			0, 0, 1, 1, 0, 0, 1, 1, 0, 0, 1, 1,
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		},
		Indices:         []uint16{0, 1, 2, 3, 4, 5},
		Texture:         encoded.Bytes(),
		TextureMIMEType: "image/png",
		Min:             [3]float32{0, 0, 0},
		Max:             [3]float32{3, 1, 0},
	}

	glb, err := BuildGLB(mesh)
	if err != nil {
		t.Fatalf("BuildGLB() error = %v", err)
	}
	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	var doc struct {
		Meshes []struct {
			Primitives []struct {
				Attributes map[string]int `json:"attributes"`
				Material   int            `json:"material"`
			} `json:"primitives"`
		} `json:"meshes"`
		Images    []json.RawMessage `json:"images"`
		Materials []struct {
			PBR struct {
				BaseColorTexture *struct {
					Index int `json:"index"`
				} `json:"baseColorTexture"`
			} `json:"pbrMetallicRoughness"`
		} `json:"materials"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &doc); err != nil {
		t.Fatal(err)
	}
	if got := len(doc.Meshes[0].Primitives); got != 2 {
		t.Fatalf("primitive count = %d, want 2", got)
	}
	if got := len(doc.Images); got != 1 {
		t.Fatalf("image count = %d, want 1", got)
	}
	textured := doc.Meshes[0].Primitives[0]
	if _, ok := textured.Attributes["TEXCOORD_0"]; !ok {
		t.Fatal("textured primitive is missing TEXCOORD_0")
	}
	if _, ok := textured.Attributes["COLOR_0"]; ok {
		t.Fatal("textured primitive should not multiply its texture by COLOR_0")
	}
	colored := doc.Meshes[0].Primitives[1]
	if _, ok := colored.Attributes["COLOR_0"]; !ok {
		t.Fatal("untextured primitive is missing COLOR_0")
	}
	if _, ok := colored.Attributes["TEXCOORD_0"]; ok {
		t.Fatal("untextured primitive should not use TEXCOORD_0")
	}
	if doc.Materials[textured.Material].PBR.BaseColorTexture == nil {
		t.Fatal("textured material is missing baseColorTexture")
	}
	if doc.Materials[colored.Material].PBR.BaseColorTexture != nil {
		t.Fatal("vertex-colored material should not reference a texture")
	}
}

func TestBuildGLBUsesVertexColorsWhenAllAtlasRegionsAreInvalid(t *testing.T) {
	mesh := Mesh{
		Positions:       []float32{0, 0, 0, 1, 0, 0, 0, 1, 0},
		Normals:         []float32{0, 0, 1, 0, 0, 1, 0, 0, 1},
		Colors:          bytes.Repeat([]byte{40, 180, 80, 255}, 3),
		TexCoords:       make([]float32, 6),
		RawTexCoords:    make([]float32, 6),
		UVRegions:       make([]float32, 12),
		Indices:         []uint16{0, 1, 2},
		Texture:         []byte("not an image; all-invalid UVs must not decode it"),
		TextureMIMEType: "image/png",
		Min:             [3]float32{0, 0, 0},
		Max:             [3]float32{1, 1, 0},
	}

	glb, err := BuildGLB(mesh)
	if err != nil {
		t.Fatalf("BuildGLB() error = %v", err)
	}
	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	var doc struct {
		Meshes []struct {
			Primitives []struct {
				Attributes map[string]int `json:"attributes"`
			} `json:"primitives"`
		} `json:"meshes"`
		Images []json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &doc); err != nil {
		t.Fatal(err)
	}
	attributes := doc.Meshes[0].Primitives[0].Attributes
	if _, ok := attributes["COLOR_0"]; !ok {
		t.Fatal("all-invalid atlas should use COLOR_0")
	}
	if _, ok := attributes["TEXCOORD_0"]; ok {
		t.Fatal("all-invalid atlas should not use TEXCOORD_0")
	}
	if len(doc.Images) != 0 {
		t.Fatalf("image count = %d, want 0", len(doc.Images))
	}
}

func TestBuildGLBRejectsInvalidTopologyAndFeatureIDs(t *testing.T) {
	mesh, err := DecodeCapture(syntheticCapture(t))
	if err != nil {
		t.Fatal(err)
	}
	mesh.Indices = append(mesh.Indices, 0)
	if _, err := BuildGLB(mesh); err == nil {
		t.Fatal("BuildGLB() accepted a non-triangle index count")
	}

	mesh, err = DecodeCapture(syntheticCapture(t))
	if err != nil {
		t.Fatal(err)
	}
	mesh.Indices[0] = uint16(len(mesh.Positions) / 3)
	if _, err := BuildGLB(mesh); err == nil {
		t.Fatal("BuildGLB() accepted an out-of-range vertex index")
	}

	mesh, err = DecodeCapture(syntheticCapture(t))
	if err != nil {
		t.Fatal(err)
	}
	mesh.EntityIDs = []uint32{218004}
	mesh.FeatureIDs = []uint16{0, 1, 0}
	if _, err := BuildGLB(mesh); err == nil {
		t.Fatal("BuildGLB() accepted an out-of-range feature ID")
	}
}

func TestBuildGLBDeclaresInverseAxisConversionForCesium(t *testing.T) {
	mesh, err := DecodeCapture(syntheticCapture(t))
	if err != nil {
		t.Fatal(err)
	}
	glb, err := BuildGLB(mesh)
	if err != nil {
		t.Fatal(err)
	}
	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	var doc struct {
		Nodes []struct {
			Matrix []float64 `json:"matrix"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &doc); err != nil {
		t.Fatal(err)
	}
	want := []float64{
		1, 0, 0, 0,
		0, 0, -1, 0,
		0, 1, 0, 0,
		0, 0, 0, 1,
	}
	if len(doc.Nodes) != 1 || !equalFloat64s(doc.Nodes[0].Matrix, want) {
		t.Fatalf("root matrix = %v, want %v", doc.Nodes[0].Matrix, want)
	}
}

func TestBuildGLBPublishesEntityIDsAsStructuralMetadata(t *testing.T) {
	capture := syntheticCapture(t)
	capture.EntityIDs = []uint32{218004, 218038}
	vertices, err := base64.StdEncoding.DecodeString(capture.VertexBase64)
	if err != nil {
		t.Fatal(err)
	}
	for vertex, entityID := range []uint32{218004, 218004, 218038} {
		binary.LittleEndian.PutUint32(vertices[vertex*capture.VertexSize+64:], entityID)
	}
	capture.VertexBase64 = base64.StdEncoding.EncodeToString(vertices)
	mesh, err := DecodeCapture(capture)
	if err != nil {
		t.Fatal(err)
	}

	glb, err := BuildGLB(mesh)
	if err != nil {
		t.Fatalf("BuildGLB() error = %v", err)
	}
	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	var doc struct {
		ExtensionsUsed []string `json:"extensionsUsed"`
		Accessors      []struct {
			BufferView    int `json:"bufferView"`
			ComponentType int `json:"componentType"`
		} `json:"accessors"`
		BufferViews []struct {
			ByteStride int `json:"byteStride"`
		} `json:"bufferViews"`
		Meshes []struct {
			Primitives []struct {
				Attributes map[string]int `json:"attributes"`
				Extensions struct {
					MeshFeatures struct {
						FeatureIDs []struct {
							FeatureCount  int `json:"featureCount"`
							Attribute     int `json:"attribute"`
							PropertyTable int `json:"propertyTable"`
						} `json:"featureIds"`
					} `json:"EXT_mesh_features"`
				} `json:"extensions"`
			} `json:"primitives"`
		} `json:"meshes"`
		Extensions struct {
			Metadata struct {
				Schema struct {
					ID string `json:"id"`
				} `json:"schema"`
				PropertyTables []struct {
					Count int `json:"count"`
				} `json:"propertyTables"`
			} `json:"EXT_structural_metadata"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &doc); err != nil {
		t.Fatal(err)
	}
	if !containsString(doc.ExtensionsUsed, "EXT_mesh_features") || !containsString(doc.ExtensionsUsed, "EXT_structural_metadata") {
		t.Fatalf("extensionsUsed = %v", doc.ExtensionsUsed)
	}
	primitive := doc.Meshes[0].Primitives[0]
	if _, ok := primitive.Attributes["_FEATURE_ID_0"]; !ok {
		t.Fatalf("attributes = %v", primitive.Attributes)
	}
	featureAccessor := doc.Accessors[primitive.Attributes["_FEATURE_ID_0"]]
	if got := featureAccessor.ComponentType; got != 5123 {
		t.Fatalf("feature ID component type = %d, want uint16 (5123)", got)
	}
	if got := doc.BufferViews[featureAccessor.BufferView].ByteStride; got != 4 {
		t.Fatalf("feature ID byte stride = %d, want 4", got)
	}
	featureIDs := primitive.Extensions.MeshFeatures.FeatureIDs
	if len(featureIDs) != 1 || featureIDs[0].FeatureCount != 2 || featureIDs[0].Attribute != 0 || featureIDs[0].PropertyTable != 0 {
		t.Fatalf("feature IDs extension = %+v", featureIDs)
	}
	if got := doc.Extensions.Metadata.PropertyTables; len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("property tables = %+v", got)
	}
	if got := doc.Extensions.Metadata.Schema.ID; got != "nlsc_modelset_entity_1_0_0" {
		t.Fatalf("metadata schema ID = %q", got)
	}
}

func TestBuildTilesetUsesLocalBoundsAndECEFTranslation(t *testing.T) {
	capture := syntheticCapture(t)

	tileset, err := BuildTileset(capture, "sample.glb")
	if err != nil {
		t.Fatalf("BuildTileset() error = %v", err)
	}
	var doc struct {
		GeometricError float64 `json:"geometricError"`
		Asset          struct {
			Version string `json:"version"`
		} `json:"asset"`
		Root struct {
			GeometricError float64   `json:"geometricError"`
			Transform      []float64 `json:"transform"`
			BoundingVolume struct {
				Box []float64 `json:"box"`
			} `json:"boundingVolume"`
			Content struct {
				URI string `json:"uri"`
			} `json:"content"`
		} `json:"root"`
	}
	if err := json.Unmarshal(tileset, &doc); err != nil {
		t.Fatalf("decode tileset: %v", err)
	}
	if doc.Asset.Version != "1.1" {
		t.Fatalf("asset version = %q", doc.Asset.Version)
	}
	if doc.GeometricError != 1 || doc.Root.GeometricError != 0 {
		t.Fatalf("single-tile geometric errors = %v/%v, want 1/0", doc.GeometricError, doc.Root.GeometricError)
	}
	if doc.Root.Content.URI != "sample.glb" {
		t.Fatalf("content URI = %q", doc.Root.Content.URI)
	}
	if got, want := len(doc.Root.Transform), 16; got != want {
		t.Fatalf("transform length = %d, want %d", got, want)
	}
	if got := doc.Root.Transform[12:15]; !equalFloat64s(got, []float64{1000, 2000, 3000}) {
		t.Fatalf("translation = %v", got)
	}
	if got, want := doc.Root.BoundingVolume.Box, []float64{5, 10, 2.5, 5, 0, 0, 0, 10, 0, 0, 0, 2.5}; !equalFloat64s(got, want) {
		t.Fatalf("local bounding box = %v, want %v", got, want)
	}
}

func TestBuildHierarchyTilesetUsesRelativeChildTransforms(t *testing.T) {
	parent := syntheticCapture(t)
	parent.ID = "8_82_121"
	child := syntheticCapture(t)
	child.ID = "9_164_242"
	child.Offset = Vec3{X: 1010, Y: 2005, Z: 3000}
	child.AABox = Box{Min: Vec3{X: 1010, Y: 2005, Z: 3000}, Max: Vec3{X: 1020, Y: 2025, Z: 3005}}

	tileset, err := BuildHierarchyTileset(TileNode{
		Capture:        parent,
		ContentURI:     "8_82_121.glb",
		GeometricError: 64,
		Children: []TileNode{{
			Capture:        child,
			ContentURI:     "9_164_242.glb",
			GeometricError: 0,
		}},
	})
	if err != nil {
		t.Fatalf("BuildHierarchyTileset() error = %v", err)
	}
	var doc struct {
		GeometricError float64 `json:"geometricError"`
		Root           struct {
			Transform []float64 `json:"transform"`
			Children  []struct {
				Transform      []float64 `json:"transform"`
				GeometricError float64   `json:"geometricError"`
				Content        struct {
					URI string `json:"uri"`
				} `json:"content"`
			} `json:"children"`
		} `json:"root"`
	}
	if err := json.Unmarshal(tileset, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.GeometricError != 64 {
		t.Fatalf("tileset geometric error = %v", doc.GeometricError)
	}
	if got := doc.Root.Transform[12:15]; !equalFloat64s(got, []float64{1000, 2000, 3000}) {
		t.Fatalf("root translation = %v", got)
	}
	if len(doc.Root.Children) != 1 {
		t.Fatalf("children count = %d", len(doc.Root.Children))
	}
	childDoc := doc.Root.Children[0]
	if got := childDoc.Transform[12:15]; !equalFloat64s(got, []float64{10, 5, 0}) {
		t.Fatalf("child relative translation = %v", got)
	}
	if childDoc.Content.URI != "9_164_242.glb" || childDoc.GeometricError != 0 {
		t.Fatalf("child = %+v", childDoc)
	}
}

func TestBuildHierarchyTilesetRejectsInvalidErrorProgression(t *testing.T) {
	parent := syntheticCapture(t)
	child := syntheticCapture(t)
	child.Offset.X += 10

	_, err := BuildHierarchyTileset(TileNode{
		Capture:        parent,
		ContentURI:     "parent.glb",
		GeometricError: 0,
		Children: []TileNode{{
			Capture:        child,
			ContentURI:     "child.glb",
			GeometricError: 0,
		}},
	})
	if err == nil {
		t.Fatal("BuildHierarchyTileset() accepted a parent with children and zero geometric error")
	}

	_, err = BuildHierarchyTileset(TileNode{
		Capture:        parent,
		ContentURI:     "parent.glb",
		GeometricError: 10,
		Children: []TileNode{{
			Capture:        child,
			ContentURI:     "child.glb",
			GeometricError: 20,
		}},
	})
	if err == nil {
		t.Fatal("BuildHierarchyTileset() accepted a child geometric error above its parent")
	}
}

func TestWriteSampleWritesLoadableTilesetPair(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSample(syntheticCapture(t), dir); err != nil {
		t.Fatalf("WriteSample() error = %v", err)
	}
	for _, name := range []string{"sample.glb", "tileset.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
}

func TestConvertCaptureFileReadsJSONAndWritesTilesetPair(t *testing.T) {
	inputDir := t.TempDir()
	input := filepath.Join(inputDir, "capture.json")
	encoded, err := json.Marshal(syntheticCapture(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "converted")

	if err := ConvertCaptureFile(input, output); err != nil {
		t.Fatalf("ConvertCaptureFile() error = %v", err)
	}
	for _, name := range []string{"sample.glb", "tileset.json"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

func TestConvertCaptureFileEmbedsRelativeTexture(t *testing.T) {
	inputDir := t.TempDir()
	texture, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "atlas.png"), texture, 0o644); err != nil {
		t.Fatal(err)
	}
	capture := syntheticCapture(t)
	capture.TexturePath = "atlas.png"
	capture.TextureMIMEType = "image/png"
	encoded, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(inputDir, "capture.json")
	if err := os.WriteFile(input, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "converted")

	if err := ConvertCaptureFile(input, output); err != nil {
		t.Fatalf("ConvertCaptureFile() error = %v", err)
	}
	glb, err := os.ReadFile(filepath.Join(output, "sample.glb"))
	if err != nil {
		t.Fatal(err)
	}
	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	var doc struct {
		Images []struct {
			MIMEType string `json:"mimeType"`
		} `json:"images"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Images) != 1 || doc.Images[0].MIMEType != "image/png" {
		t.Fatalf("images = %+v", doc.Images)
	}
}

func syntheticCapture(t *testing.T) Capture {
	t.Helper()
	const stride = 68
	vertices := make([]byte, 3*stride)
	positions := [][3]float32{{0, 0, 0}, {10, 0, 0}, {0, 20, 5}}
	colors := [][4]byte{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}}
	for i := range positions {
		base := i * stride
		for component, value := range positions[i] {
			binary.LittleEndian.PutUint32(vertices[base+component*4:], math.Float32bits(value))
		}
		binary.LittleEndian.PutUint32(vertices[base+12:], math.Float32bits(0))
		binary.LittleEndian.PutUint32(vertices[base+16:], math.Float32bits(0))
		binary.LittleEndian.PutUint32(vertices[base+20:], math.Float32bits(1))
		copy(vertices[base+24:base+28], colors[i][:])
		binary.LittleEndian.PutUint32(vertices[base+44:], math.Float32bits(1))
		binary.LittleEndian.PutUint32(vertices[base+48:], math.Float32bits(1))
	}
	indices := make([]byte, 6)
	for i, index := range []uint16{0, 1, 2} {
		binary.LittleEndian.PutUint16(indices[i*2:], index)
	}
	return Capture{
		ID:           "synthetic",
		Layer:        "113_H",
		VertexCount:  3,
		IndexCount:   3,
		VertexSize:   stride,
		Offset:       Vec3{X: 1000, Y: 2000, Z: 3000},
		AABox:        Box{Min: Vec3{X: 1000, Y: 2000, Z: 3000}, Max: Vec3{X: 1010, Y: 2020, Z: 3005}},
		Resolution:   2.5,
		VertexBase64: base64.StdEncoding.EncodeToString(vertices),
		IndexBase64:  base64.StdEncoding.EncodeToString(indices),
	}
}

func equalFloat32s(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func closeFloat32s(a, b []float32, tolerance float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(float64(a[i]-b[i])) > tolerance {
			return false
		}
	}
	return true
}

func equalUint16s(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalUint32s(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalFloat64s(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}
