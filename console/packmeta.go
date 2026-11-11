package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
)

// packMetadata 生成打包用的瓦片元数据，字段与 tiler 写的 metadata.json 一致（task.go MetaItems）。
// 旧版 tiler 下载的目录里没有 metadata.json，打出的 pmtiles 就缺名称、格式和范围；
// 打包 Job 拿到它后，sync.sh 在目录缺 metadata.json 时把它写进 mbtiles 再转 pmtiles。
// geo 是 geojson ConfigMap 的数据，范围取所有 lrs 区域的外接矩形；读不到的区域跳过。
func packMetadata(conf *TilerConf, geo map[string]string) string {
	m := map[string]string{
		"name":        conf.TM.Name,
		"basename":    conf.TM.Name,
		"format":      conf.TM.Format,
		"type":        conf.TM.Schema,
		"minzoom":     strconv.Itoa(conf.TM.Min),
		"maxzoom":     strconv.Itoa(conf.TM.Max),
		"pixel_scale": "256",
	}
	for k, v := range m {
		if v == "" {
			delete(m, k)
		}
	}
	union := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	found := false
	for _, x := range conf.Lrs {
		b, ok := geojsonBounds([]byte(geo[geojsonBase(x.Geojson)]))
		if !ok {
			continue
		}
		found = true
		union = [4]float64{min(union[0], b[0]), min(union[1], b[1]), max(union[2], b[2]), max(union[3], b[3])}
	}
	if found {
		const maxLat = 85.051129
		union = [4]float64{max(union[0], -180), max(union[1], -maxLat), min(union[2], 180), min(union[3], maxLat)}
		m["bounds"] = fmt.Sprintf("%f,%f,%f,%f", union[0], union[1], union[2], union[3])
		m["center"] = fmt.Sprintf("%f,%f,%d", (union[0]+union[2])/2, (union[1]+union[3])/2, (conf.TM.Min+conf.TM.Max)/2)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// geojsonBounds 返回 GeoJSON（FeatureCollection / Feature / Geometry）所有坐标的外接矩形 [minLng, minLat, maxLng, maxLat]。
func geojsonBounds(data []byte) ([4]float64, bool) {
	b := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	var v any
	if len(data) == 0 || json.Unmarshal(data, &v) != nil {
		return b, false
	}
	found := false
	var walk func(x any, inCoords bool)
	walk = func(x any, inCoords bool) {
		switch t := x.(type) {
		case map[string]any:
			for k, c := range t {
				if k != "properties" { // 属性里可能有同名字段，不是几何
					walk(c, k == "coordinates")
				}
			}
		case []any:
			if inCoords && len(t) >= 2 {
				lng, ok1 := t[0].(float64)
				lat, ok2 := t[1].(float64)
				if ok1 && ok2 {
					b = [4]float64{min(b[0], lng), min(b[1], lat), max(b[2], lng), max(b[3], lat)}
					found = true
					return
				}
			}
			for _, c := range t {
				walk(c, inCoords)
			}
		}
	}
	walk(v, false)
	return b, found
}

// packMetaFor 读取图层的 geojson ConfigMap 生成打包元数据；读失败时仍给出不含范围的元数据。
func (s *Server) packMetaFor(l *LayerEntry, conf *TilerConf) string {
	gcm, err := s.kube.GetConfigMap(l.GeojsonConfigMap)
	if err != nil {
		log.Printf("读取 %s 失败，打包元数据不含范围: %v", l.GeojsonConfigMap, err)
		return packMetadata(conf, nil)
	}
	return packMetadata(conf, gcm.Data)
}
