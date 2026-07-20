// Package nlscmodelset converts a captured NLSC PilotGaea MODELSET node into
// a minimal, standards-based 3D Tiles tileset containing a glTF binary mesh.
package nlscmodelset

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const pilotGaeaVertexSize = 68

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type Box struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

// Capture is the browser-extracted CPU representation of one MODELSET node.
type Capture struct {
	ID              string   `json:"id"`
	Layer           string   `json:"layer"`
	Level           int      `json:"level"`
	Row             int      `json:"row"`
	Col             int      `json:"col"`
	ChildrenIDs     []string `json:"childrenIds,omitempty"`
	SourceURL       string   `json:"sourceUrl,omitempty"`
	EntityIDs       []uint32 `json:"entityIds,omitempty"`
	VertexCount     int      `json:"vertexCount"`
	IndexCount      int      `json:"indexCount"`
	VertexSize      int      `json:"vertexSize"`
	Offset          Vec3     `json:"offset"`
	AABox           Box      `json:"aaBox"`
	Resolution      float64  `json:"resolution"`
	VertexBase64    string   `json:"vertexBase64"`
	IndexBase64     string   `json:"indexBase64"`
	TexturePath     string   `json:"texturePath,omitempty"`
	TextureMIMEType string   `json:"textureMIMEType,omitempty"`
	textureData     []byte
}

// Mesh contains the attributes needed by the generated glTF primitive.
type Mesh struct {
	Positions       []float32
	Normals         []float32
	Colors          []byte
	TexCoords       []float32
	RawTexCoords    []float32
	UVRegions       []float32
	Indices         []uint16
	FeatureIDs      []uint16
	EntityIDs       []uint32
	Texture         []byte
	TextureMIMEType string
	Min             [3]float32
	Max             [3]float32
}

// DecodeCapture reads PilotGaea's 68-byte interleaved vertex layout, including
// positions, normals, colors, raw/atlas UVs and per-vertex entity IDs. Floor
// attributes that are not needed by glTF remain intentionally omitted.
func DecodeCapture(c Capture) (Mesh, error) {
	var mesh Mesh
	if c.VertexSize != pilotGaeaVertexSize {
		return mesh, fmt.Errorf("unsupported vertex size %d, want %d", c.VertexSize, pilotGaeaVertexSize)
	}
	if c.VertexCount <= 0 || c.IndexCount <= 0 {
		return mesh, errors.New("vertex and index counts must be positive")
	}
	vertexData, err := base64.StdEncoding.DecodeString(c.VertexBase64)
	if err != nil {
		return mesh, fmt.Errorf("decode vertex data: %w", err)
	}
	indexData, err := base64.StdEncoding.DecodeString(c.IndexBase64)
	if err != nil {
		return mesh, fmt.Errorf("decode index data: %w", err)
	}
	if got, want := len(vertexData), c.VertexCount*c.VertexSize; got != want {
		return mesh, fmt.Errorf("vertex byte length %d, want %d", got, want)
	}
	if got, want := len(indexData), c.IndexCount*2; got != want {
		return mesh, fmt.Errorf("index byte length %d, want %d", got, want)
	}

	mesh.Positions = make([]float32, 0, c.VertexCount*3)
	mesh.Normals = make([]float32, 0, c.VertexCount*3)
	mesh.Colors = make([]byte, 0, c.VertexCount*4)
	mesh.TexCoords = make([]float32, 0, c.VertexCount*2)
	mesh.RawTexCoords = make([]float32, 0, c.VertexCount*2)
	mesh.UVRegions = make([]float32, 0, c.VertexCount*4)
	entityToFeature := make(map[uint32]uint16, len(c.EntityIDs))
	if len(c.EntityIDs) > math.MaxUint16 {
		return mesh, fmt.Errorf("entity count %d exceeds uint16 feature ID capacity", len(c.EntityIDs))
	}
	for i, entityID := range c.EntityIDs {
		entityToFeature[entityID] = uint16(i)
	}
	mesh.EntityIDs = append(mesh.EntityIDs, c.EntityIDs...)
	mesh.FeatureIDs = make([]uint16, 0, c.VertexCount)
	mesh.Min = [3]float32{float32(math.Inf(1)), float32(math.Inf(1)), float32(math.Inf(1))}
	mesh.Max = [3]float32{float32(math.Inf(-1)), float32(math.Inf(-1)), float32(math.Inf(-1))}
	for vertex := 0; vertex < c.VertexCount; vertex++ {
		base := vertex * c.VertexSize
		for component := 0; component < 3; component++ {
			value := math.Float32frombits(binary.LittleEndian.Uint32(vertexData[base+component*4:]))
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return Mesh{}, fmt.Errorf("vertex %d position component %d is not finite", vertex, component)
			}
			mesh.Positions = append(mesh.Positions, value)
			if value < mesh.Min[component] {
				mesh.Min[component] = value
			}
			if value > mesh.Max[component] {
				mesh.Max[component] = value
			}
		}
		for component := 0; component < 3; component++ {
			value := math.Float32frombits(binary.LittleEndian.Uint32(vertexData[base+12+component*4:]))
			mesh.Normals = append(mesh.Normals, value)
		}
		mesh.Colors = append(mesh.Colors, vertexData[base+24:base+28]...)
		u := math.Float32frombits(binary.LittleEndian.Uint32(vertexData[base+28:]))
		v := math.Float32frombits(binary.LittleEndian.Uint32(vertexData[base+32:]))
		regionMinU := math.Float32frombits(binary.LittleEndian.Uint32(vertexData[base+36:]))
		regionMinV := math.Float32frombits(binary.LittleEndian.Uint32(vertexData[base+40:]))
		regionMaxU := math.Float32frombits(binary.LittleEndian.Uint32(vertexData[base+44:]))
		regionMaxV := math.Float32frombits(binary.LittleEndian.Uint32(vertexData[base+48:]))
		atlasU := (u-float32(math.Floor(float64(u))))*(regionMaxU-regionMinU) + regionMinU
		atlasV := (v-float32(math.Floor(float64(v))))*(regionMaxV-regionMinV) + regionMinV
		mesh.TexCoords = append(mesh.TexCoords, atlasU, atlasV)
		mesh.RawTexCoords = append(mesh.RawTexCoords, u, v)
		mesh.UVRegions = append(mesh.UVRegions, regionMinU, regionMinV, regionMaxU, regionMaxV)
		if len(entityToFeature) > 0 {
			entityID := binary.LittleEndian.Uint32(vertexData[base+64:])
			featureID, ok := entityToFeature[entityID]
			if !ok {
				return Mesh{}, fmt.Errorf("vertex %d references unknown entity ID %d", vertex, entityID)
			}
			mesh.FeatureIDs = append(mesh.FeatureIDs, featureID)
		}
	}

	mesh.Indices = make([]uint16, c.IndexCount)
	for i := range mesh.Indices {
		mesh.Indices[i] = binary.LittleEndian.Uint16(indexData[i*2:])
		if int(mesh.Indices[i]) >= c.VertexCount {
			return Mesh{}, fmt.Errorf("index %d references vertex %d, vertex count is %d", i, mesh.Indices[i], c.VertexCount)
		}
	}
	return mesh, nil
}

type gltfBufferView struct {
	Buffer     int `json:"buffer"`
	ByteOffset int `json:"byteOffset,omitempty"`
	ByteLength int `json:"byteLength"`
	ByteStride int `json:"byteStride,omitempty"`
	Target     int `json:"target,omitempty"`
}

type gltfAccessor struct {
	BufferView    int       `json:"bufferView"`
	ComponentType int       `json:"componentType"`
	Count         int       `json:"count"`
	Type          string    `json:"type"`
	Normalized    bool      `json:"normalized,omitempty"`
	Min           []float32 `json:"min,omitempty"`
	Max           []float32 `json:"max,omitempty"`
}

type atlasGroup struct {
	indices  []uint16
	texture  []byte
	region   [4]float32
	textured bool
}

func splitAtlasTexture(mesh Mesh) ([]atlasGroup, bool, error) {
	vertexCount := len(mesh.Positions) / 3
	if len(mesh.Texture) == 0 || len(mesh.RawTexCoords) != vertexCount*2 || len(mesh.UVRegions) != vertexCount*4 || len(mesh.Indices)%3 != 0 {
		return nil, false, nil
	}
	type regionKey [4]uint32
	keyForVertex := func(vertex uint16) regionKey {
		base := int(vertex) * 4
		return regionKey{
			math.Float32bits(mesh.UVRegions[base]),
			math.Float32bits(mesh.UVRegions[base+1]),
			math.Float32bits(mesh.UVRegions[base+2]),
			math.Float32bits(mesh.UVRegions[base+3]),
		}
	}
	groupIndices := make(map[regionKey]int)
	groups := make([]atlasGroup, 0)
	vertexColorGroup := -1
	for triangle := 0; triangle < len(mesh.Indices); triangle += 3 {
		key := keyForVertex(mesh.Indices[triangle])
		if key != keyForVertex(mesh.Indices[triangle+1]) || key != keyForVertex(mesh.Indices[triangle+2]) {
			return nil, false, fmt.Errorf("triangle %d crosses atlas regions", triangle/3)
		}
		region := [4]float32{
			math.Float32frombits(key[0]), math.Float32frombits(key[1]),
			math.Float32frombits(key[2]), math.Float32frombits(key[3]),
		}
		if region[2] <= region[0] || region[3] <= region[1] {
			if vertexColorGroup < 0 {
				vertexColorGroup = len(groups)
				groups = append(groups, atlasGroup{})
			}
			groups[vertexColorGroup].indices = append(groups[vertexColorGroup].indices, mesh.Indices[triangle:triangle+3]...)
			continue
		}
		groupIndex, ok := groupIndices[key]
		if !ok {
			groupIndex = len(groups)
			groupIndices[key] = groupIndex
			groups = append(groups, atlasGroup{region: region, textured: true})
		}
		groups[groupIndex].indices = append(groups[groupIndex].indices, mesh.Indices[triangle:triangle+3]...)
	}

	var source image.Image
	if len(groupIndices) > 0 {
		var err error
		source, _, err = image.Decode(bytes.NewReader(mesh.Texture))
		if err != nil {
			return nil, false, fmt.Errorf("decode atlas texture: %w", err)
		}
	}
	bounds := image.Rectangle{}
	if source != nil {
		bounds = source.Bounds()
	}
	width, height := bounds.Dx(), bounds.Dy()
	for i := range groups {
		if !groups[i].textured {
			continue
		}
		region := groups[i].region
		x0 := clampInt(int(math.Round(float64(region[0])*float64(width))), 0, width)
		y0 := clampInt(int(math.Round(float64(region[1])*float64(height))), 0, height)
		x1 := clampInt(int(math.Round(float64(region[2])*float64(width))), 0, width)
		y1 := clampInt(int(math.Round(float64(region[3])*float64(height))), 0, height)
		if x1 <= x0 || y1 <= y0 {
			return nil, false, fmt.Errorf("atlas region %d has empty pixel bounds", i)
		}
		cropped := image.NewRGBA(image.Rect(0, 0, x1-x0, y1-y0))
		draw.Draw(cropped, cropped.Bounds(), source, image.Point{X: bounds.Min.X + x0, Y: bounds.Min.Y + y0}, draw.Src)
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, cropped); err != nil {
			return nil, false, fmt.Errorf("encode atlas region %d: %w", i, err)
		}
		groups[i].texture = encoded.Bytes()
	}
	return groups, true, nil
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// BuildGLB creates an embedded glTF 2.0 binary with positions, normals,
// normalized vertex colors and uint16 triangle indices.
func BuildGLB(mesh Mesh) ([]byte, error) {
	vertexCount := len(mesh.Positions) / 3
	if vertexCount == 0 || len(mesh.Positions)%3 != 0 {
		return nil, errors.New("positions must contain complete vertices")
	}
	if len(mesh.Normals) != vertexCount*3 || len(mesh.Colors) != vertexCount*4 || len(mesh.TexCoords) != vertexCount*2 || len(mesh.Indices) == 0 {
		return nil, errors.New("mesh attribute lengths do not match")
	}
	if len(mesh.Indices)%3 != 0 {
		return nil, errors.New("triangle index count must be a multiple of 3")
	}
	for i, index := range mesh.Indices {
		if int(index) >= vertexCount {
			return nil, fmt.Errorf("index %d references vertex %d, vertex count is %d", i, index, vertexCount)
		}
	}
	if len(mesh.Texture) > 0 && mesh.TextureMIMEType == "" {
		return nil, errors.New("texture MIME type is required")
	}
	atlasGroups, useAtlasGroups, err := splitAtlasTexture(mesh)
	if err != nil {
		return nil, err
	}
	indexSets := [][]uint16{mesh.Indices}
	textureCoordinates := mesh.TexCoords
	if useAtlasGroups {
		indexSets = make([][]uint16, len(atlasGroups))
		for i := range atlasGroups {
			indexSets[i] = atlasGroups[i].indices
		}
		textureCoordinates = mesh.RawTexCoords
	}

	var bin bytes.Buffer
	views := make([]gltfBufferView, 0, 4)
	appendView := func(data []byte, target int) {
		offset := bin.Len()
		bin.Write(data)
		for bin.Len()%4 != 0 {
			bin.WriteByte(0)
		}
		views = append(views, gltfBufferView{Buffer: 0, ByteOffset: offset, ByteLength: len(data), Target: target})
	}

	positionBytes := make([]byte, len(mesh.Positions)*4)
	for i, value := range mesh.Positions {
		binary.LittleEndian.PutUint32(positionBytes[i*4:], math.Float32bits(value))
	}
	appendView(positionBytes, 34962)
	normalBytes := make([]byte, len(mesh.Normals)*4)
	for i, value := range mesh.Normals {
		binary.LittleEndian.PutUint32(normalBytes[i*4:], math.Float32bits(value))
	}
	appendView(normalBytes, 34962)
	appendView(mesh.Colors, 34962)
	indexBytes := make([]byte, len(indexSets[0])*2)
	for i, value := range indexSets[0] {
		binary.LittleEndian.PutUint16(indexBytes[i*2:], value)
	}
	appendView(indexBytes, 34963)
	indexViews := []int{3}
	texCoordBytes := make([]byte, len(textureCoordinates)*4)
	for i, value := range textureCoordinates {
		binary.LittleEndian.PutUint32(texCoordBytes[i*4:], math.Float32bits(value))
	}
	appendView(texCoordBytes, 34962)
	featureIDAccessor := -1
	entityValuesView := -1
	if len(mesh.EntityIDs) > 0 {
		if len(mesh.FeatureIDs) != vertexCount {
			return nil, errors.New("feature ID count does not match vertex count")
		}
		for vertex, featureID := range mesh.FeatureIDs {
			if int(featureID) >= len(mesh.EntityIDs) {
				return nil, fmt.Errorf("vertex %d feature ID %d exceeds property table count %d", vertex, featureID, len(mesh.EntityIDs))
			}
		}
		featureBytes := make([]byte, len(mesh.FeatureIDs)*4)
		for i, value := range mesh.FeatureIDs {
			binary.LittleEndian.PutUint16(featureBytes[i*4:], value)
		}
		featureView := len(views)
		appendView(featureBytes, 34962)
		views[featureView].ByteStride = 4
		featureIDAccessor = 5
		entityBytes := make([]byte, len(mesh.EntityIDs)*4)
		for i, value := range mesh.EntityIDs {
			binary.LittleEndian.PutUint32(entityBytes[i*4:], value)
		}
		entityValuesView = len(views)
		appendView(entityBytes, 0)
	}
	for i := 1; i < len(indexSets); i++ {
		data := make([]byte, len(indexSets[i])*2)
		for j, value := range indexSets[i] {
			binary.LittleEndian.PutUint16(data[j*2:], value)
		}
		indexViews = append(indexViews, len(views))
		appendView(data, 34963)
	}
	imageBufferViews := make([]int, 0)
	imageMIMETypes := make([]string, 0)
	if useAtlasGroups {
		for _, group := range atlasGroups {
			if !group.textured {
				continue
			}
			imageBufferViews = append(imageBufferViews, len(views))
			imageMIMETypes = append(imageMIMETypes, "image/png")
			appendView(group.texture, 0)
		}
	} else if len(mesh.Texture) > 0 {
		imageBufferViews = append(imageBufferViews, len(views))
		imageMIMETypes = append(imageMIMETypes, mesh.TextureMIMEType)
		appendView(mesh.Texture, 0)
	}

	accessors := []gltfAccessor{
		{BufferView: 0, ComponentType: 5126, Count: vertexCount, Type: "VEC3", Min: mesh.Min[:], Max: mesh.Max[:]},
		{BufferView: 1, ComponentType: 5126, Count: vertexCount, Type: "VEC3"},
		{BufferView: 2, ComponentType: 5121, Count: vertexCount, Type: "VEC4", Normalized: true},
		{BufferView: 3, ComponentType: 5123, Count: len(indexSets[0]), Type: "SCALAR"},
		{BufferView: 4, ComponentType: 5126, Count: vertexCount, Type: "VEC2"},
	}
	if featureIDAccessor >= 0 {
		accessors = append(accessors, gltfAccessor{BufferView: 5, ComponentType: 5123, Count: vertexCount, Type: "SCALAR"})
	}
	indexAccessors := []int{3}
	for i := 1; i < len(indexSets); i++ {
		indexAccessors = append(indexAccessors, len(accessors))
		accessors = append(accessors, gltfAccessor{BufferView: indexViews[i], ComponentType: 5123, Count: len(indexSets[i]), Type: "SCALAR"})
	}
	makePrimitive := func(indexAccessor, material int, textured bool) map[string]interface{} {
		attributes := map[string]int{"POSITION": 0, "NORMAL": 1}
		if textured {
			attributes["TEXCOORD_0"] = 4
		} else {
			attributes["COLOR_0"] = 2
		}
		if featureIDAccessor >= 0 {
			attributes["_FEATURE_ID_0"] = featureIDAccessor
		}
		primitive := map[string]interface{}{
			"attributes": attributes,
			"indices":    indexAccessor,
			"material":   material,
		}
		if featureIDAccessor >= 0 {
			primitive["extensions"] = map[string]interface{}{
				"EXT_mesh_features": map[string]interface{}{
					"featureIds": []interface{}{map[string]interface{}{
						"featureCount":  len(mesh.EntityIDs),
						"attribute":     0,
						"propertyTable": 0,
					}},
				},
			}
		}
		return primitive
	}
	materialCount := 1
	if useAtlasGroups {
		materialCount = len(atlasGroups)
	}
	materials := make([]interface{}, 0, materialCount)
	primitives := make([]interface{}, 0, materialCount)
	textureIndex := 0
	for i := 0; i < materialCount; i++ {
		pbr := map[string]interface{}{
			"baseColorFactor": []float64{1, 1, 1, 1},
			"metallicFactor":  0,
			"roughnessFactor": 1,
		}
		textured := len(mesh.Texture) > 0
		if useAtlasGroups {
			textured = atlasGroups[i].textured
		}
		if textured {
			pbr["baseColorTexture"] = map[string]interface{}{"index": textureIndex}
			textureIndex++
		}
		materials = append(materials, map[string]interface{}{"doubleSided": true, "pbrMetallicRoughness": pbr})
		indexAccessor := indexAccessors[0]
		if useAtlasGroups {
			indexAccessor = indexAccessors[i]
		}
		primitives = append(primitives, makePrimitive(indexAccessor, i, textured))
	}
	doc := map[string]interface{}{
		"asset":  map[string]interface{}{"version": "2.0", "generator": "tiler/nlscmodelset"},
		"scene":  0,
		"scenes": []interface{}{map[string]interface{}{"nodes": []int{0}}},
		"nodes": []interface{}{map[string]interface{}{
			"mesh": 0,
			// 3D Tiles applies glTF Y-up to Z-up after the glTF node graph.
			// Source MODELSET vertices already use the tile's Z-up/ECEF axes,
			// so this inverse matrix cancels Cesium's standard conversion.
			"matrix": []float64{
				1, 0, 0, 0,
				0, 0, -1, 0,
				0, 1, 0, 0,
				0, 0, 0, 1,
			},
		}},
		"meshes": []interface{}{
			map[string]interface{}{
				"primitives": primitives,
			},
		},
		"materials":   materials,
		"buffers":     []interface{}{map[string]interface{}{"byteLength": bin.Len()}},
		"bufferViews": views,
		"accessors":   accessors,
	}
	if entityValuesView >= 0 {
		doc["extensionsUsed"] = []string{"EXT_mesh_features", "EXT_structural_metadata"}
		doc["extensionsRequired"] = []string{"EXT_mesh_features", "EXT_structural_metadata"}
		doc["extensions"] = map[string]interface{}{
			"EXT_structural_metadata": map[string]interface{}{
				"schema": map[string]interface{}{
					"id": "nlsc_modelset_entity_1_0_0",
					"classes": map[string]interface{}{
						"building": map[string]interface{}{
							"properties": map[string]interface{}{
								"entityId": map[string]interface{}{"type": "SCALAR", "componentType": "UINT32"},
							},
						},
					},
				},
				"propertyTables": []interface{}{map[string]interface{}{
					"class": "building",
					"count": len(mesh.EntityIDs),
					"properties": map[string]interface{}{
						"entityId": map[string]interface{}{"values": entityValuesView},
					},
				}},
			},
		}
	}
	if len(imageBufferViews) > 0 {
		images := make([]interface{}, 0, len(imageBufferViews))
		textures := make([]interface{}, 0, len(imageBufferViews))
		for i, view := range imageBufferViews {
			images = append(images, map[string]interface{}{"bufferView": view, "mimeType": imageMIMETypes[i]})
			textures = append(textures, map[string]interface{}{"sampler": 0, "source": i})
		}
		wrap := 33071
		if useAtlasGroups {
			wrap = 10497
		}
		doc["images"] = images
		doc["samplers"] = []interface{}{map[string]interface{}{"magFilter": 9729, "minFilter": 9729, "wrapS": wrap, "wrapT": wrap}}
		doc["textures"] = textures
	}
	jsonChunk, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode glTF JSON: %w", err)
	}
	for len(jsonChunk)%4 != 0 {
		jsonChunk = append(jsonChunk, ' ')
	}

	var glb bytes.Buffer
	glb.WriteString("glTF")
	binary.Write(&glb, binary.LittleEndian, uint32(2))
	totalLength := 12 + 8 + len(jsonChunk) + 8 + bin.Len()
	binary.Write(&glb, binary.LittleEndian, uint32(totalLength))
	binary.Write(&glb, binary.LittleEndian, uint32(len(jsonChunk)))
	glb.WriteString("JSON")
	glb.Write(jsonChunk)
	binary.Write(&glb, binary.LittleEndian, uint32(bin.Len()))
	glb.Write([]byte{'B', 'I', 'N', 0})
	glb.Write(bin.Bytes())
	return glb.Bytes(), nil
}

// BuildTileset creates a one-tile 3D Tiles 1.1 manifest. The bounding volume
// and mesh remain local to the node; the transform places them in ECEF.
func BuildTileset(c Capture, glbURI string) ([]byte, error) {
	if glbURI == "" {
		return nil, errors.New("glB URI is required")
	}
	if c.AABox.Max.X < c.AABox.Min.X || c.AABox.Max.Y < c.AABox.Min.Y || c.AABox.Max.Z < c.AABox.Min.Z {
		return nil, errors.New("invalid axis-aligned bounding box")
	}
	center := Vec3{
		X: (c.AABox.Min.X+c.AABox.Max.X)/2 - c.Offset.X,
		Y: (c.AABox.Min.Y+c.AABox.Max.Y)/2 - c.Offset.Y,
		Z: (c.AABox.Min.Z+c.AABox.Max.Z)/2 - c.Offset.Z,
	}
	halfX := (c.AABox.Max.X - c.AABox.Min.X) / 2
	halfY := (c.AABox.Max.Y - c.AABox.Min.Y) / 2
	halfZ := (c.AABox.Max.Z - c.AABox.Min.Z) / 2
	transform := []float64{
		1, 0, 0, 0,
		0, 1, 0, 0,
		0, 0, 1, 0,
		c.Offset.X, c.Offset.Y, c.Offset.Z, 1,
	}
	box := []float64{center.X, center.Y, center.Z, halfX, 0, 0, 0, halfY, 0, 0, 0, halfZ}
	doc := map[string]interface{}{
		"asset":          map[string]interface{}{"version": "1.1"},
		"geometricError": 1,
		"root": map[string]interface{}{
			"boundingVolume": map[string]interface{}{"box": box},
			"geometricError": 0,
			"refine":         "REPLACE",
			"transform":      transform,
			"content":        map[string]interface{}{"uri": glbURI},
		},
	}
	result, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode tileset: %w", err)
	}
	return append(result, '\n'), nil
}

// WriteSample writes sample.glb and tileset.json into dir.
func WriteSample(c Capture, dir string) error {
	mesh, err := DecodeCapture(c)
	if err != nil {
		return err
	}
	mesh.Texture = c.textureData
	mesh.TextureMIMEType = c.TextureMIMEType
	glb, err := BuildGLB(mesh)
	if err != nil {
		return err
	}
	tileset, err := BuildTileset(c, "sample.glb")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample.glb"), glb, 0o644); err != nil {
		return fmt.Errorf("write sample.glb: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tileset.json"), tileset, 0o644); err != nil {
		return fmt.Errorf("write tileset.json: %w", err)
	}
	return nil
}

// ConvertCaptureFile reads a browser capture JSON file and writes its 3D Tiles
// representation into outputDir.
func ConvertCaptureFile(inputPath, outputDir string) error {
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read capture: %w", err)
	}
	var capture Capture
	if err := json.Unmarshal(data, &capture); err != nil {
		return fmt.Errorf("decode capture JSON: %w", err)
	}
	if capture.TexturePath != "" {
		texturePath := capture.TexturePath
		if !filepath.IsAbs(texturePath) {
			texturePath = filepath.Join(filepath.Dir(inputPath), texturePath)
		}
		capture.textureData, err = os.ReadFile(texturePath)
		if err != nil {
			return fmt.Errorf("read texture: %w", err)
		}
	}
	if err := WriteSample(capture, outputDir); err != nil {
		return fmt.Errorf("convert capture: %w", err)
	}
	return nil
}
