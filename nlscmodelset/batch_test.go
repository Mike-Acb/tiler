package nlscmodelset

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildArchiveWritesSMBFriendlyResumableLayout(t *testing.T) {
	input := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(input, "download-config.json"), []byte(`{"version":1,"layer":"113_H","root":{"level":1,"row":0,"col":0}}`), 0644); err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(input, "raw", "9", "164", "238.response")
	if err := os.MkdirAll(filepath.Dir(rawPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawPath, syntheticRawNode(t), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "build-config.json.part"), []byte("interrupted"), 0644); err != nil {
		t.Fatal(err)
	}

	options := BuildOptions{
		InputDir: input, OutputDir: output, Workers: 2,
		MinLevel: 1, MaxLevel: 9, Format: RawFormat{Version: 14, VertexSize: 64},
	}
	stats, err := BuildArchive(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Converted != 1 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("unexpected first-run stats: %+v", stats)
	}
	for _, relative := range []string{
		filepath.Join("content", "9", "164", "238.glb"),
		filepath.Join("nodes", "9", "164", "238.json"),
		"build-config.json",
		"tileset.json",
	} {
		if info, err := os.Stat(filepath.Join(output, relative)); err != nil || info.Size() == 0 {
			t.Fatalf("missing output %s: info=%v err=%v", relative, info, err)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(output, "**", "*.part")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary files remain: %v, err=%v", matches, err)
	}

	manifestData, err := os.ReadFile(filepath.Join(output, "tileset.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]interface{}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("invalid tileset: %v", err)
	}
	if manifest["asset"].(map[string]interface{})["version"] != "1.1" {
		t.Fatalf("unexpected tileset asset: %#v", manifest["asset"])
	}
	if manifest["geometricError"].(float64) <= 0 {
		t.Fatalf("top-level geometricError must stay positive so Cesium traverses a single-leaf tileset: %#v", manifest["geometricError"])
	}

	stats, err = BuildArchive(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Converted != 0 || stats.Skipped != 1 || stats.Failed != 0 {
		t.Fatalf("unexpected resume stats: %+v", stats)
	}

	glbPath := filepath.Join(output, "content", "9", "164", "238.glb")
	if err := os.WriteFile(glbPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	stats, err = BuildArchive(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Converted != 1 || stats.Skipped != 0 {
		t.Fatalf("corrupt GLB was not rebuilt: %+v", stats)
	}

	mismatched := options
	mismatched.Format.VertexSize = 60
	if _, err := BuildArchive(context.Background(), mismatched); err == nil {
		t.Fatal("output accepted a mismatched raw format")
	}
}

func TestBuildArchiveFinalizeOnlySkipsSourceAndGLBValidation(t *testing.T) {
	input := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(input, "download-config.json"), []byte(`{"version":1,"layer":"113_H","root":{"level":1,"row":0,"col":0}}`), 0644); err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(input, "raw", "9", "164", "238.response")
	if err := os.MkdirAll(filepath.Dir(rawPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawPath, syntheticRawNode(t), 0644); err != nil {
		t.Fatal(err)
	}
	options := BuildOptions{
		InputDir: input, OutputDir: output, Workers: 2,
		MinLevel: 1, MaxLevel: 9, Format: RawFormat{Version: 14, VertexSize: 64},
	}
	if _, err := BuildArchive(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rawPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "content", "9", "164", "238.glb"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(output, "tileset.json")); err != nil {
		t.Fatal(err)
	}
	options.FinalizeOnly = true
	stats, err := BuildArchive(context.Background(), options)
	if err != nil {
		t.Fatalf("finalize existing markers: %v", err)
	}
	if stats.Discovered != 1 || stats.Skipped != 1 || stats.Converted != 0 {
		t.Fatalf("unexpected finalize stats: %+v", stats)
	}
	if _, err := os.Stat(filepath.Join(output, "tileset.json")); err != nil {
		t.Fatalf("tileset was not published: %v", err)
	}
}

func TestBuildArchiveTilesetRejectsMissingAdvertisedChild(t *testing.T) {
	input := t.TempDir()
	parent := nodeRecord{
		Level: 8, Row: 82, Col: 119,
		Offset:      Vec3{X: 1, Y: 2, Z: 3},
		AABox:       Box{Min: Vec3{X: 0, Y: 1, Z: 2}, Max: Vec3{X: 2, Y: 3, Z: 4}},
		ChildrenIDs: []string{"1"}, Resolution: 1,
		ContentURIs: []string{"content/8/82/119.glb"},
	}
	options := BuildOptions{InputDir: input, MaxLevel: 9}
	if _, err := buildArchiveTileset([]nodeRecord{parent}, options); err == nil {
		t.Fatal("missing advertised child was silently omitted")
	}
	empty := filepath.Join(input, "empty", "9", "164", "238.empty")
	if err := os.MkdirAll(filepath.Dir(empty), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildArchiveTileset([]nodeRecord{parent}, options); err != nil {
		t.Fatalf("known empty child should be accepted: %v", err)
	}
}

func TestBuildArchiveTilesetAllowsMissingChildForPartialDeepCoverage(t *testing.T) {
	parent := nodeRecord{
		Level: 9, Row: 63, Col: 119,
		Offset:      Vec3{X: 1, Y: 2, Z: 3},
		AABox:       Box{Min: Vec3{X: 0, Y: 1, Z: 2}, Max: Vec3{X: 2, Y: 3, Z: 4}},
		ChildrenIDs: []string{"1"}, Resolution: 1,
		ContentURIs: []string{"content/9/63/119.glb"},
	}
	options := BuildOptions{InputDir: t.TempDir(), MaxLevel: 10}
	field := reflect.ValueOf(&options).Elem().FieldByName("AllowMissingChildren")
	if !field.IsValid() {
		t.Fatal("BuildOptions is missing AllowMissingChildren")
	}
	field.SetBool(true)
	if _, err := buildArchiveTileset([]nodeRecord{parent}, options); err != nil {
		t.Fatalf("partial deep coverage should keep the parent tile: %v", err)
	}
}

func TestBuildArchiveTilesetRejectsPartiallyDownloadedSiblingSet(t *testing.T) {
	parent := nodeRecord{
		Level: 9, Row: 63, Col: 119,
		Offset:      Vec3{X: 1, Y: 2, Z: 3},
		AABox:       Box{Min: Vec3{X: 0, Y: 1, Z: 2}, Max: Vec3{X: 2, Y: 3, Z: 4}},
		ChildrenIDs: []string{"1", "2"}, Resolution: 1,
		ContentURIs: []string{"content/9/63/119.glb"},
	}
	child := nodeRecord{
		Level: 10, Row: 126, Col: 238,
		Offset: parent.Offset, AABox: parent.AABox,
		ContentURIs: []string{"content/10/126/238.glb"},
	}
	options := BuildOptions{InputDir: t.TempDir(), MaxLevel: 10, AllowMissingChildren: true}
	if _, err := buildArchiveTileset([]nodeRecord{parent, child}, options); err == nil {
		t.Fatal("partially downloaded sibling set was published with unsafe REPLACE refinement")
	}
}

func TestBuildArchiveTilesetRejectsOrphanRecord(t *testing.T) {
	root := nodeRecord{
		Level: 8, Row: 82, Col: 119,
		Offset:      Vec3{X: 1, Y: 2, Z: 3},
		AABox:       Box{Min: Vec3{X: 0, Y: 1, Z: 2}, Max: Vec3{X: 2, Y: 3, Z: 4}},
		ContentURIs: []string{"content/8/82/119.glb"},
	}
	orphan := root
	orphan.Level, orphan.Row, orphan.Col = 9, 164, 238
	orphan.ContentURIs = []string{"content/9/164/238.glb"}
	if _, err := buildArchiveTileset([]nodeRecord{root, orphan}, BuildOptions{InputDir: t.TempDir(), MaxLevel: 9}); err == nil {
		t.Fatal("orphan converted record was silently omitted from tileset")
	}
}

func TestBuildArchiveTilesetIgnoresOrphanWithoutRenderableContent(t *testing.T) {
	root := nodeRecord{
		Level: 1, Row: 0, Col: 0,
		Offset:      Vec3{X: 1, Y: 2, Z: 3},
		AABox:       Box{Min: Vec3{X: 0, Y: 1, Z: 2}, Max: Vec3{X: 2, Y: 3, Z: 4}},
		ContentURIs: []string{"content/1/0/0.glb"},
	}
	orphan := nodeRecord{Level: 9, Row: 136, Col: 90}
	if _, err := buildArchiveTileset([]nodeRecord{root, orphan}, BuildOptions{InputDir: t.TempDir(), MaxLevel: 9}); err != nil {
		t.Fatalf("non-renderable orphan should not block tileset publication: %v", err)
	}
}

func TestBuildArchiveTilesetReplacesParentForPartialChildCoverage(t *testing.T) {
	parent := nodeRecord{
		Level: 8, Row: 21, Col: 127,
		Offset:      Vec3{X: 1, Y: 2, Z: 3},
		AABox:       Box{Min: Vec3{X: 0, Y: 1, Z: 2}, Max: Vec3{X: 2, Y: 3, Z: 4}},
		ChildrenIDs: []string{"3", "4"}, Resolution: 1,
		ContentURIs: []string{"content/8/21/127.glb"},
	}
	children := []nodeRecord{
		{Level: 9, Row: 43, Col: 254, Offset: parent.Offset, AABox: parent.AABox, ContentURIs: []string{"content/9/43/254.glb"}},
		{Level: 9, Row: 43, Col: 255, Offset: parent.Offset, AABox: parent.AABox, ContentURIs: []string{"content/9/43/255.glb"}},
	}
	data, err := buildArchiveTileset(append([]nodeRecord{parent}, children...), BuildOptions{InputDir: t.TempDir(), MaxLevel: 9})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Root struct {
			Refine string `json:"refine"`
		} `json:"root"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document.Root.Refine != "REPLACE" {
		t.Fatalf("partial child coverage refine = %q, want REPLACE to avoid overlapping coarse and detailed geometry", document.Root.Refine)
	}
}
