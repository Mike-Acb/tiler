package nlscmodelset

import (
	"encoding/json"
	"errors"
	"fmt"
)

// TileNode describes one converted MODELSET node and its descendants.
type TileNode struct {
	Capture        Capture
	ContentURI     string
	GeometricError float64
	Children       []TileNode
}

// BuildHierarchyTileset creates a 3D Tiles 1.1 hierarchy. Child transforms are
// relative to their parent transform, as required by the 3D Tiles transform
// composition rules.
func BuildHierarchyTileset(root TileNode) ([]byte, error) {
	rootJSON, err := buildHierarchyTile(root, nil)
	if err != nil {
		return nil, err
	}
	doc := map[string]interface{}{
		"asset":          map[string]interface{}{"version": "1.1"},
		"geometricError": root.GeometricError,
		"root":           rootJSON,
	}
	result, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode hierarchy tileset: %w", err)
	}
	return append(result, '\n'), nil
}

func buildHierarchyTile(node TileNode, parentOffset *Vec3) (map[string]interface{}, error) {
	if node.ContentURI == "" {
		return nil, errors.New("tile content URI is required")
	}
	if node.GeometricError < 0 {
		return nil, errors.New("tile geometric error cannot be negative")
	}
	if len(node.Children) > 0 && node.GeometricError == 0 {
		return nil, errors.New("tile with children must have a positive geometric error")
	}
	box, err := localBoundingBox(node.Capture)
	if err != nil {
		return nil, err
	}
	translation := node.Capture.Offset
	if parentOffset != nil {
		translation.X -= parentOffset.X
		translation.Y -= parentOffset.Y
		translation.Z -= parentOffset.Z
	}
	tile := map[string]interface{}{
		"boundingVolume": map[string]interface{}{"box": box},
		"geometricError": node.GeometricError,
		"refine":         "REPLACE",
		"transform": []float64{
			1, 0, 0, 0,
			0, 1, 0, 0,
			0, 0, 1, 0,
			translation.X, translation.Y, translation.Z, 1,
		},
		"content": map[string]interface{}{"uri": node.ContentURI},
	}
	if len(node.Children) > 0 {
		children := make([]interface{}, 0, len(node.Children))
		for _, child := range node.Children {
			if child.GeometricError > node.GeometricError {
				return nil, fmt.Errorf("child geometric error %g exceeds parent geometric error %g", child.GeometricError, node.GeometricError)
			}
			childJSON, err := buildHierarchyTile(child, &node.Capture.Offset)
			if err != nil {
				return nil, err
			}
			children = append(children, childJSON)
		}
		tile["children"] = children
	}
	return tile, nil
}

func localBoundingBox(c Capture) ([]float64, error) {
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
	return []float64{center.X, center.Y, center.Z, halfX, 0, 0, 0, halfY, 0, 0, 0, halfZ}, nil
}
