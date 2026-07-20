package nlscmodelset

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// RawFormat describes the versioned MODELSET wire format advertised by LAYER.
// Taoyuan layer 113_H currently advertises version 14 and 64-byte vertices.
type RawFormat struct {
	Version    int
	VertexSize int
}

var Taoyuan113HFormat = RawFormat{Version: 14, VertexSize: 64}

type RawTexture struct {
	Type     uint8
	Width    uint16
	Height   uint16
	Data     []byte
	MIMEType string
}

type RawDraw struct {
	VertexCount int
	IndexCount  int
	VertexData  []byte
	IndexData   []byte
}

type RawDrawGroup struct {
	Textures []RawTexture
	Draws    []RawDraw
}

// RawNode is one decoded MODELSET NODE response before glTF conversion.
type RawNode struct {
	Level               int
	Row                 int
	Col                 int
	Offset              Vec3
	AABox8Points        [8]Vec3
	ChildrenIDs         []string
	EntityIDs           []uint32
	Resolution          float64
	BoundaryPoints      [4]Vec3
	FrameBoundaryPoints [4]Vec3
	AABox               Box
	FrameBox            Box
	DrawGroups          []RawDrawGroup
	MaskDrawGroups      []RawDrawGroup
	AttributeTable      []byte
	format              RawFormat
}

// DecodeRawNode decodes an HTTP response body saved by nlsc-modelset-download.
// Both gzip-compressed and already-decoded NODE bodies are accepted.
func DecodeRawNode(response []byte, format RawFormat) (RawNode, error) {
	var node RawNode
	if format.Version < 1 {
		return node, errors.New("MODELSET version must be positive")
	}
	if format.VertexSize < 56 {
		return node, fmt.Errorf("unsupported raw vertex size %d", format.VertexSize)
	}
	body, err := decodeGzipResponse(response)
	if err != nil {
		return node, err
	}
	reader := rawReader{data: body}
	level, err := reader.int32("level")
	if err != nil {
		return node, err
	}
	if level == -999 {
		return node, errors.New("NODE contains the -999 empty sentinel")
	}
	node.Level = int(level)
	if node.Row, err = reader.int("row"); err != nil {
		return node, err
	}
	if node.Col, err = reader.int("col"); err != nil {
		return node, err
	}
	if node.Offset, err = reader.vec3("offset"); err != nil {
		return node, err
	}
	for i := range node.AABox8Points {
		if node.AABox8Points[i], err = reader.vec3("AABox point"); err != nil {
			return node, err
		}
	}
	childCount, err := reader.count("child", 4)
	if err != nil {
		return node, err
	}
	for i := 0; i < childCount; i++ {
		childID, err := reader.int32("child ID")
		if err != nil {
			return node, err
		}
		if childID != 0 {
			node.ChildrenIDs = append(node.ChildrenIDs, fmt.Sprint(childID))
		}
	}
	entityCount, err := reader.uint16("entity count")
	if err != nil {
		return node, err
	}
	node.EntityIDs = make([]uint32, int(entityCount))
	for i := range node.EntityIDs {
		entityID, err := reader.int32("entity ID")
		if err != nil {
			return node, err
		}
		node.EntityIDs[i] = uint32(entityID)
	}
	if node.Resolution, err = reader.float64("resolution"); err != nil {
		return node, err
	}
	for i := range node.BoundaryPoints {
		if node.BoundaryPoints[i], err = reader.vec3("boundary point"); err != nil {
			return node, err
		}
	}
	if format.Version >= 3 {
		for i := range node.FrameBoundaryPoints {
			if node.FrameBoundaryPoints[i], err = reader.vec3("frame boundary point"); err != nil {
				return node, err
			}
		}
	}
	if node.AABox, err = reader.box("AABox"); err != nil {
		return node, err
	}
	if node.FrameBox, err = reader.box("frame box"); err != nil {
		return node, err
	}
	if node.DrawGroups, err = reader.drawGroups("draw group", format.VertexSize); err != nil {
		return node, err
	}
	if format.Version >= 9 {
		if node.MaskDrawGroups, err = reader.drawGroups("mask draw group", format.VertexSize); err != nil {
			return node, err
		}
	}
	if format.Version >= 5 {
		attributeLength, err := reader.uint32("attribute table length")
		if err != nil {
			return node, err
		}
		if attributeLength < 4 {
			return node, fmt.Errorf("attribute table length %d is smaller than its 4-byte prefix", attributeLength)
		}
		if node.AttributeTable, err = reader.bytes(int(attributeLength-4), "attribute table"); err != nil {
			return node, err
		}
	}
	if reader.remaining() != 0 {
		return node, fmt.Errorf("NODE has %d unparsed trailing bytes", reader.remaining())
	}
	node.format = format
	return node, nil
}

// DecodeMeshes converts each normal draw into an independent glTF-ready mesh.
// Keeping draws separate preserves their uint16 index domains.
func (node RawNode) DecodeMeshes() ([]Mesh, error) {
	meshes := make([]Mesh, 0)
	for collectionIndex, groups := range [][]RawDrawGroup{node.DrawGroups, node.MaskDrawGroups} {
		for groupIndex, group := range groups {
			texture, mimeType, err := groupTexture(group)
			if err != nil {
				return nil, fmt.Errorf("draw collection %d group %d: %w", collectionIndex, groupIndex, err)
			}
			for drawIndex, draw := range group.Draws {
				if draw.VertexCount == 0 || draw.IndexCount == 0 {
					continue
				}
				mesh, err := decodeRawDraw(draw, node.EntityIDs, node.format.VertexSize)
				if err != nil {
					return nil, fmt.Errorf("draw collection %d group %d draw %d: %w", collectionIndex, groupIndex, drawIndex, err)
				}
				mesh.Texture = texture
				mesh.TextureMIMEType = mimeType
				meshes = append(meshes, mesh)
			}
		}
	}
	return meshes, nil
}

func groupTexture(group RawDrawGroup) ([]byte, string, error) {
	var selected *RawTexture
	for i := range group.Textures {
		if len(group.Textures[i].Data) == 0 {
			continue
		}
		if selected != nil {
			return nil, "", errors.New("multiple embedded textures require atlas assembly")
		}
		selected = &group.Textures[i]
	}
	if selected == nil {
		return nil, "", nil
	}
	return selected.Data, selected.MIMEType, nil
}

func decodeRawDraw(draw RawDraw, entityIDs []uint32, stride int) (Mesh, error) {
	var mesh Mesh
	if len(draw.VertexData) != draw.VertexCount*stride {
		return mesh, fmt.Errorf("vertex byte length %d, want %d", len(draw.VertexData), draw.VertexCount*stride)
	}
	if len(draw.IndexData) != draw.IndexCount*2 {
		return mesh, fmt.Errorf("index byte length %d, want %d", len(draw.IndexData), draw.IndexCount*2)
	}
	mesh.Positions = make([]float32, 0, draw.VertexCount*3)
	mesh.Normals = make([]float32, 0, draw.VertexCount*3)
	mesh.Colors = make([]byte, 0, draw.VertexCount*4)
	mesh.TexCoords = make([]float32, 0, draw.VertexCount*2)
	mesh.RawTexCoords = make([]float32, 0, draw.VertexCount*2)
	mesh.UVRegions = make([]float32, 0, draw.VertexCount*4)
	mesh.EntityIDs = append(mesh.EntityIDs, entityIDs...)
	if len(entityIDs) > 0 {
		mesh.FeatureIDs = make([]uint16, 0, draw.VertexCount)
	}
	mesh.Min = [3]float32{float32(math.Inf(1)), float32(math.Inf(1)), float32(math.Inf(1))}
	mesh.Max = [3]float32{float32(math.Inf(-1)), float32(math.Inf(-1)), float32(math.Inf(-1))}
	for vertex := 0; vertex < draw.VertexCount; vertex++ {
		base := vertex * stride
		for component := 0; component < 3; component++ {
			value := readFloat32(draw.VertexData[base+component*4:])
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
			mesh.Normals = append(mesh.Normals, readFloat32(draw.VertexData[base+12+component*4:]))
		}
		mesh.Colors = append(mesh.Colors, draw.VertexData[base+24:base+28]...)
		u := readFloat32(draw.VertexData[base+28:])
		v := readFloat32(draw.VertexData[base+32:])
		minU := readFloat32(draw.VertexData[base+36:])
		minV := readFloat32(draw.VertexData[base+40:])
		maxU := readFloat32(draw.VertexData[base+44:])
		maxV := readFloat32(draw.VertexData[base+48:])
		mesh.RawTexCoords = append(mesh.RawTexCoords, u, v)
		mesh.UVRegions = append(mesh.UVRegions, minU, minV, maxU, maxV)
		mesh.TexCoords = append(mesh.TexCoords,
			(u-float32(math.Floor(float64(u))))*(maxU-minU)+minU,
			(v-float32(math.Floor(float64(v))))*(maxV-minV)+minV,
		)
		if len(entityIDs) > 0 {
			featureID := binary.LittleEndian.Uint16(draw.VertexData[base+54:])
			if int(featureID) >= len(entityIDs) {
				return Mesh{}, fmt.Errorf("vertex %d feature ID %d exceeds entity count %d", vertex, featureID, len(entityIDs))
			}
			mesh.FeatureIDs = append(mesh.FeatureIDs, featureID)
		}
	}
	mesh.Indices = make([]uint16, draw.IndexCount)
	for i := range mesh.Indices {
		mesh.Indices[i] = binary.LittleEndian.Uint16(draw.IndexData[i*2:])
		if int(mesh.Indices[i]) >= draw.VertexCount {
			return Mesh{}, fmt.Errorf("index %d references vertex %d, vertex count is %d", i, mesh.Indices[i], draw.VertexCount)
		}
	}
	return mesh, nil
}

func readFloat32(data []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(data))
}

type rawReader struct {
	data []byte
	pos  int
}

func (r *rawReader) remaining() int { return len(r.data) - r.pos }

func (r *rawReader) bytes(length int, field string) ([]byte, error) {
	if length < 0 || length > r.remaining() {
		return nil, fmt.Errorf("truncated %s at byte %d: need %d, have %d", field, r.pos, length, r.remaining())
	}
	result := r.data[r.pos : r.pos+length]
	r.pos += length
	return result, nil
}

func (r *rawReader) uint8(field string) (uint8, error) {
	data, err := r.bytes(1, field)
	if err != nil {
		return 0, err
	}
	return data[0], nil
}

func (r *rawReader) uint16(field string) (uint16, error) {
	data, err := r.bytes(2, field)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(data), nil
}

func (r *rawReader) uint32(field string) (uint32, error) {
	data, err := r.bytes(4, field)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(data), nil
}

func (r *rawReader) int32(field string) (int32, error) {
	value, err := r.uint32(field)
	return int32(value), err
}

func (r *rawReader) int(field string) (int, error) {
	value, err := r.int32(field)
	return int(value), err
}

func (r *rawReader) float64(field string) (float64, error) {
	data, err := r.bytes(8, field)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(data)), nil
}

func (r *rawReader) vec3(field string) (Vec3, error) {
	x, err := r.float64(field + " x")
	if err != nil {
		return Vec3{}, err
	}
	y, err := r.float64(field + " y")
	if err != nil {
		return Vec3{}, err
	}
	z, err := r.float64(field + " z")
	if err != nil {
		return Vec3{}, err
	}
	return Vec3{X: x, Y: y, Z: z}, nil
}

func (r *rawReader) box(field string) (Box, error) {
	min, err := r.vec3(field + " min")
	if err != nil {
		return Box{}, err
	}
	max, err := r.vec3(field + " max")
	if err != nil {
		return Box{}, err
	}
	return Box{Min: min, Max: max}, nil
}

func (r *rawReader) count(field string, max int) (int, error) {
	count, err := r.int(field + " count")
	if err != nil {
		return 0, err
	}
	if count < 0 || count > max || count > r.remaining() {
		return 0, fmt.Errorf("invalid %s count %d", field, count)
	}
	return count, nil
}

func (r *rawReader) drawGroups(field string, vertexSize int) ([]RawDrawGroup, error) {
	count, err := r.count(field, 4096)
	if err != nil {
		return nil, err
	}
	groups := make([]RawDrawGroup, count)
	for groupIndex := range groups {
		textureCount, err := r.count("texture", 4096)
		if err != nil {
			return nil, fmt.Errorf("%s %d: %w", field, groupIndex, err)
		}
		groups[groupIndex].Textures = make([]RawTexture, textureCount)
		for textureIndex := range groups[groupIndex].Textures {
			texture := &groups[groupIndex].Textures[textureIndex]
			if texture.Type, err = r.uint8("texture type"); err != nil {
				return nil, err
			}
			if texture.Width, err = r.uint16("texture width"); err != nil {
				return nil, err
			}
			if texture.Height, err = r.uint16("texture height"); err != nil {
				return nil, err
			}
			if texture.Type == 1 {
				length, err := r.count("texture byte", 1<<30)
				if err != nil {
					return nil, err
				}
				if texture.Data, err = r.bytes(length, "texture data"); err != nil {
					return nil, err
				}
				texture.MIMEType = detectTextureMIME(texture.Data)
			}
		}
		drawCount, err := r.count("draw", 4096)
		if err != nil {
			return nil, fmt.Errorf("%s %d: %w", field, groupIndex, err)
		}
		groups[groupIndex].Draws = make([]RawDraw, drawCount)
		for drawIndex := range groups[groupIndex].Draws {
			draw := &groups[groupIndex].Draws[drawIndex]
			if draw.VertexCount, err = r.count("vertex", 1<<16); err != nil {
				return nil, err
			}
			if draw.VertexCount > 0 {
				length, err := r.count("vertex byte", 1<<30)
				if err != nil {
					return nil, err
				}
				if length != draw.VertexCount*vertexSize {
					return nil, fmt.Errorf("draw %d vertex byte length %d, want %d", drawIndex, length, draw.VertexCount*vertexSize)
				}
				if draw.VertexData, err = r.bytes(length, "vertex data"); err != nil {
					return nil, err
				}
			}
			if draw.IndexCount, err = r.count("index", 1<<24); err != nil {
				return nil, err
			}
			if draw.IndexCount > 0 {
				if draw.IndexData, err = r.bytes(draw.IndexCount*2, "index data"); err != nil {
					return nil, err
				}
			}
		}
	}
	return groups, nil
}

func detectTextureMIME(data []byte) string {
	if bytes.HasPrefix(data, pngSignature) {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg"
	}
	// PilotGaea type 1 is an embedded image. PNG is the format used by 113_H;
	// retaining this default also lets callers report malformed image payloads
	// at GLB construction time with useful context.
	return "image/png"
}
