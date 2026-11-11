package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/minio/minio-go/v7"
)

// PMMeta 是从 pmtiles v3 文件头和 metadata 里读到的、控制台关心的信息。
type PMMeta struct {
	Format    string         `json:"format,omitempty"` // png / jpg / webp / avif / pbf；unknown 时为空
	MinZoom   int            `json:"minZoom"`
	MaxZoom   int            `json:"maxZoom"`
	Bounds    []float64      `json:"bounds,omitempty"` // 默认全球范围时为空
	Name      string         `json:"name,omitempty"`
	TileCount uint64         `json:"tileCount"`
	Metadata  int            `json:"metadataKeys"` // metadata JSON 的键数量
	Err       string         `json:"error,omitempty"`
	Extra     map[string]any `json:"-"`
}

var pmTileTypes = map[byte]string{1: "pbf", 2: "png", 3: "jpg", 4: "webp", 5: "avif"}

// parsePMHeader 解析 127 字节的 pmtiles v3 文件头。
func parsePMHeader(h []byte) (meta PMMeta, metaOff, metaLen int64, compression byte, err error) {
	if len(h) < 127 || string(h[:7]) != "PMTiles" {
		return meta, 0, 0, 0, errors.New("不是 pmtiles 文件")
	}
	if h[7] != 3 {
		return meta, 0, 0, 0, fmt.Errorf("不支持的 pmtiles 版本 %d", h[7])
	}
	le := binary.LittleEndian
	metaOff, metaLen = int64(le.Uint64(h[24:])), int64(le.Uint64(h[32:]))
	meta.TileCount = le.Uint64(h[72:])
	compression = h[97]
	meta.Format = pmTileTypes[h[99]]
	meta.MinZoom, meta.MaxZoom = int(h[100]), int(h[101])
	e7 := func(o int) float64 { return math.Round(float64(int32(le.Uint32(h[o:])))/1e7*1e6) / 1e6 }
	b := []float64{e7(102), e7(106), e7(110), e7(114)}
	// pmtiles convert 在 mbtiles 没有 bounds 时填的默认全球范围，视为缺失
	if !(b[0] == -180 && b[1] == -85 && b[2] == 180 && b[3] == 85) {
		meta.Bounds = b
	}
	return meta, metaOff, metaLen, compression, nil
}

// applyPMMetadata 解析 metadata JSON（只支持未压缩和 gzip，pmtiles convert 默认 gzip）。
func applyPMMetadata(meta *PMMeta, raw []byte, compression byte) error {
	switch compression {
	case 0, 1:
	case 2:
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("metadata 解压失败: %w", err)
		}
		if raw, err = io.ReadAll(zr); err != nil {
			return fmt.Errorf("metadata 解压失败: %w", err)
		}
	default:
		return fmt.Errorf("metadata 压缩方式 %d 暂不支持", compression)
	}
	var m map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("metadata 不是合法 JSON: %w", err)
		}
	}
	meta.Metadata = len(m)
	meta.Extra = m
	if v, ok := m["name"].(string); ok {
		meta.Name = v
	}
	if meta.Format == "" {
		if v, ok := m["format"].(string); ok {
			meta.Format = v
		}
	}
	return nil
}

func readRange(ctx context.Context, mc *minio.Client, bucket, key string, off, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, off+n-1); err != nil {
		return nil, err
	}
	obj, err := mc.GetObject(ctx, bucket, key, opts)
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(io.LimitReader(obj, n))
}

// readPMMeta 读 pmtiles 文件头和 metadata；按 ETag 缓存，文件不变就不重复读。
func (s *Server) readPMMeta(ctx context.Context, key, etag string) *PMMeta {
	s.metaMu.Lock()
	if c, ok := s.metaCache[key]; ok && c.etag == etag {
		s.metaMu.Unlock()
		return c.meta
	}
	s.metaMu.Unlock()

	meta := &PMMeta{}
	h, err := readRange(ctx, s.store.minio, s.store.bucket, key, 0, 127)
	if err == nil {
		var off, n int64
		var comp byte
		*meta, off, n, comp, err = parsePMHeader(h)
		if err == nil && n > 0 {
			if n > 4<<20 {
				err = fmt.Errorf("metadata 过大（%d 字节）", n)
			} else if raw, rerr := readRange(ctx, s.store.minio, s.store.bucket, key, off, n); rerr != nil {
				err = rerr
			} else {
				err = applyPMMetadata(meta, raw, comp)
			}
		}
	}
	if err != nil {
		meta.Err = err.Error()
	}
	s.metaMu.Lock()
	s.metaCache[key] = cachedMeta{etag: etag, meta: meta}
	s.metaMu.Unlock()
	return meta
}

type cachedMeta struct {
	etag string
	meta *PMMeta
}

// resolveInfo 按「pmtiles 优先、ConfigMap 兜底」得出展示用的名称 / 格式 / 层级，
// 并列出 pmtiles 缺失的项和与配置不一致的项。
func resolveInfo(st *layerState) {
	conf := st.conf
	st.Name, st.NameFrom = conf.TM.Name, "configmap"
	st.Format, st.FormatFrom = conf.TM.Format, "configmap"
	st.Min, st.Max, st.ZoomFrom = conf.TM.Min, conf.TM.Max, "configmap"
	st.Missing, st.Mismatch = nil, nil

	m := st.Meta
	if m == nil {
		return // pmtiles 还没生成，页面已有「未生成」提示
	}
	if m.Err != "" {
		st.Missing = append(st.Missing, "文件头无法读取："+m.Err)
		return
	}
	if m.Name != "" {
		st.Name, st.NameFrom = m.Name, "pmtiles"
	} else {
		st.Missing = append(st.Missing, "名称")
	}
	if m.Format != "" {
		st.Format, st.FormatFrom = m.Format, "pmtiles"
		if !sameFormat(m.Format, conf.TM.Format) {
			st.Mismatch = append(st.Mismatch, fmt.Sprintf("格式：pmtiles 为 %s，配置为 %s", m.Format, conf.TM.Format))
		}
	} else {
		st.Missing = append(st.Missing, "格式")
	}
	if m.TileCount > 0 {
		st.Min, st.Max, st.ZoomFrom = m.MinZoom, m.MaxZoom, "pmtiles"
		if m.MinZoom != conf.TM.Min || m.MaxZoom != conf.TM.Max {
			st.Mismatch = append(st.Mismatch, fmt.Sprintf("层级：pmtiles 为 z%d-%d，配置为 z%d-%d（配置改动后尚未重新打包？）", m.MinZoom, m.MaxZoom, conf.TM.Min, conf.TM.Max))
		}
	} else {
		st.Missing = append(st.Missing, "层级")
	}
	if m.Bounds == nil {
		st.Missing = append(st.Missing, "范围（bounds）")
	}
	if m.Name != "" && m.Name != conf.TM.Name {
		st.Mismatch = append(st.Mismatch, fmt.Sprintf("名称：pmtiles 为 %s，配置目录为 %s", m.Name, conf.TM.Name))
	}
}

func sameFormat(a, b string) bool {
	norm := func(s string) string {
		switch s {
		case "jpeg":
			return "jpg"
		case "mvt":
			return "pbf"
		}
		return s
	}
	return norm(a) == norm(b)
}
