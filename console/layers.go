package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// Registry 描述控制台管理的图层，以及生成 Job 用到的集群资源名。
type Registry struct {
	Namespace      string `json:"namespace"`
	Images         Images `json:"images"`
	ProxyConfigMap string `json:"proxyConfigMap"`
	// ProxySecret 可选：代理账号（键 PROXY_AUTH，格式 用户名:密码），按图层默认和代理池模式下交给代理 sidecar
	ProxySecret      string `json:"proxySecret,omitempty"`
	GeojsonMountPath string `json:"geojsonMountPath"`

	// ---- 存储与预览（环境变量优先，见 storage.go）----
	OutputPVC string `json:"outputPVC"`           // 下载目录 PVC：下载 Job 写入
	OutputDir string `json:"outputDir,omitempty"` // OUTPUT_DIR：控制台里的只读挂载路径，默认 /data/output
	// 上传目标（可选）。打包 Job 通过这两个资源拿 MinIO 地址和凭证。
	MinioConfigMap string `json:"minioConfigMap,omitempty"`
	MinioSecret    string `json:"minioSecret,omitempty"`
	MinioEndpoint  string `json:"minioEndpoint,omitempty"` // MINIO_ENDPOINT：控制台自己读 MinIO 用
	MinioBucket    string `json:"minioBucket,omitempty"`   // MINIO_BUCKET
	// PMTILES_PUBLIC_URL：浏览器可直读的 pmtiles 基础地址（含桶），预览 B 直连；为空则经控制台中转。
	PMTilesPublicURL string `json:"pmtilesPublicUrl,omitempty"`
	// TILE_SERVER_URL / TILE_SERVER_PREFIX：go-pmtiles serve 地址与桶内前缀，预览 C 经反向代理读取。
	TileServer       string            `json:"tileServer,omitempty"`
	TileServerPrefix string            `json:"tileServerPrefix,omitempty"`
	JobTTLSeconds    int               `json:"jobTTLSeconds"`
	NodeSelector     map[string]string `json:"nodeSelector,omitempty"`
	Layers           []LayerEntry      `json:"layers"`
}

type Images struct {
	Tiler string `json:"tiler"`
	Sync  string `json:"sync"`
	Proxy string `json:"proxy"`
}

type LayerEntry struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Group            string `json:"group"`
	ConfigMap        string `json:"configMap"`
	GeojsonConfigMap string `json:"geojsonConfigMap"`
	MinioPath        string `json:"minioPath"`
	Proxy            bool   `json:"proxy"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func loadRegistry(file string) (*Registry, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("读取图层清单失败: %w", err)
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("解析图层清单失败: %w", err)
	}
	if r.Namespace == "" {
		r.Namespace = "default"
	}
	if r.GeojsonMountPath == "" {
		r.GeojsonMountPath = "/app/geojson"
	}
	if r.JobTTLSeconds == 0 {
		r.JobTTLSeconds = 7 * 24 * 3600
	}
	if r.Images.Tiler == "" || r.Images.Sync == "" {
		return nil, fmt.Errorf("图层清单缺少 images.tiler / images.sync")
	}
	seen := map[string]bool{}
	for _, l := range r.Layers {
		if !dnsLabel.MatchString(l.ID) || len(l.ID) > 40 {
			return nil, fmt.Errorf("图层 id %q 需为小写字母数字和连字符，且不超过 40 字符", l.ID)
		}
		if seen[l.ID] {
			return nil, fmt.Errorf("图层 id %q 重复", l.ID)
		}
		seen[l.ID] = true
		if l.ConfigMap == "" || l.GeojsonConfigMap == "" {
			return nil, fmt.Errorf("图层 %q 缺少 configMap / geojsonConfigMap", l.ID)
		}
		if l.Proxy && (r.Images.Proxy == "" || r.ProxyConfigMap == "") {
			return nil, fmt.Errorf("图层 %q 需要代理，但清单缺少 images.proxy / proxyConfigMap", l.ID)
		}
	}
	return &r, nil
}

func (r *Registry) layer(id string) (*LayerEntry, bool) {
	for i := range r.Layers {
		if r.Layers[i].ID == id {
			return &r.Layers[i], true
		}
	}
	return nil, false
}

// pmtilesKey 与 pmtiles-sync/sync.sh 的上传路径保持一致：<MINIO_PATH>/<tm.name 空格换下划线>.pmtiles
func pmtilesKey(minioPath, tileName string) string {
	name := strings.ReplaceAll(tileName, " ", "_") + ".pmtiles"
	if minioPath == "" {
		return name
	}
	return strings.Trim(minioPath, "/") + "/" + name
}

// ---- 区域 geojson ----

var keyChars = regexp.MustCompile(`[^a-z0-9_-]+`)

// regionKey 把用户输入的名称转成 ConfigMap 可用的 key（[-._a-zA-Z0-9]）。
// regionRef 指向一个已配置区域：图层（复制来源时用）+ geojson 键 + 层级。
type regionRef struct {
	Layer string `json:"layer,omitempty"`
	Key   string `json:"key"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
}

// pickRegionKey 决定新区域在目标 geojson ConfigMap 里用哪个键、要不要写入。
//   - 新画的区域：按名称加时间生成新键，写入；
//   - 复制已配置区域：目标里没有这个键就原样写入；已有且内容相同（多个图层共用一个 ConfigMap）就直接引用不写；
//     同名但内容不同则另起新键，避免覆盖别的图层在用的区域。
func pickRegionKey(existing map[string]string, from *regionRef, geo []byte, name string, now time.Time) (key string, write bool) {
	if from == nil {
		return regionKey(name, now), true
	}
	old, ok := existing[from.Key]
	switch {
	case !ok:
		return from.Key, true
	case old == string(geo):
		return from.Key, false
	}
	return regionKey(name, now), true
}

// hasRegion 图层配置里是否已有同一区域、同样层级的 [[lrs]]。
func hasRegion(conf *TilerConf, key string, min, max int) bool {
	for _, x := range conf.Lrs {
		if geojsonBase(x.Geojson) == key && x.Min == min && x.Max == max {
			return true
		}
	}
	return false
}

func regionKey(name string, now time.Time) string {
	k := keyChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	k = strings.Trim(k, "-_")
	if len(k) > 40 {
		k = k[:40]
	}
	stamp := now.Format("20060102-150405")
	if k == "" {
		return "region-" + stamp + ".geojson"
	}
	return k + "-" + stamp + ".geojson"
}

type geoFeature struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	Geometry   struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	} `json:"geometry"`
}

type geoCollection struct {
	Type     string       `json:"type"`
	Name     string       `json:"name,omitempty"`
	Features []geoFeature `json:"features"`
}

// normalizeRegion 校验区域并输出 tiler 能读的 FeatureCollection（只允许 Polygon / MultiPolygon）。
func normalizeRegion(raw json.RawMessage, name string) ([]byte, error) {
	var fc geoCollection
	if err := json.Unmarshal(raw, &fc); err != nil {
		return nil, fmt.Errorf("geojson 无法解析: %w", err)
	}
	if fc.Type != "FeatureCollection" || len(fc.Features) == 0 {
		return nil, errors.New("geojson 必须是非空 FeatureCollection")
	}
	for i, f := range fc.Features {
		switch f.Geometry.Type {
		case "Polygon":
			var rings [][][]float64
			if err := json.Unmarshal(f.Geometry.Coordinates, &rings); err != nil {
				return nil, fmt.Errorf("第 %d 个要素坐标无效: %w", i+1, err)
			}
			if err := checkRings(rings); err != nil {
				return nil, fmt.Errorf("第 %d 个要素: %w", i+1, err)
			}
		case "MultiPolygon":
			var polys [][][][]float64
			if err := json.Unmarshal(f.Geometry.Coordinates, &polys); err != nil {
				return nil, fmt.Errorf("第 %d 个要素坐标无效: %w", i+1, err)
			}
			for _, rings := range polys {
				if err := checkRings(rings); err != nil {
					return nil, fmt.Errorf("第 %d 个要素: %w", i+1, err)
				}
			}
		default:
			return nil, fmt.Errorf("第 %d 个要素类型为 %q，只支持 Polygon / MultiPolygon", i+1, f.Geometry.Type)
		}
		if fc.Features[i].Properties == nil {
			fc.Features[i].Properties = map[string]any{}
		}
		fc.Features[i].Type = "Feature"
		// tiler 的 loadFeatureCollection 会丢弃 name=original 的要素，这里避免误伤
		if v, _ := fc.Features[i].Properties["name"].(string); v == "original" {
			delete(fc.Features[i].Properties, "name")
		}
	}
	fc.Name = name
	return json.Marshal(fc)
}

func checkRings(rings [][][]float64) error {
	if len(rings) == 0 {
		return errors.New("多边形没有环")
	}
	for _, ring := range rings {
		if len(ring) < 4 {
			return errors.New("多边形环至少需要 4 个点（首尾闭合）")
		}
		for _, p := range ring {
			if len(p) < 2 || math.IsNaN(p[0]) || math.IsNaN(p[1]) || p[0] < -180 || p[0] > 180 || p[1] < -85.06 || p[1] > 85.06 {
				return fmt.Errorf("坐标 %v 超出经纬度范围", p)
			}
		}
		first, last := ring[0], ring[len(ring)-1]
		if first[0] != last[0] || first[1] != last[1] {
			return errors.New("多边形环未闭合")
		}
	}
	return nil
}

// ---- Job ----

const (
	labelManaged  = "app.kubernetes.io/managed-by"
	managedValue  = "tiler-console"
	labelLayer    = "tiler-console/layer"
	labelTileName = "tiler-console/tile-name"
	annoReason    = "tiler-console/reason"
	annoProxy     = "tiler-console/proxy"
	labelKind     = "tiler-console/kind"
)

// tileNameLabel 把目录名转成合法 label 值，用来判断两个图层是否写同一个目录。
func tileNameLabel(name string) string {
	v := keyChars.ReplaceAllString(strings.ToLower(name), "-")
	v = strings.Trim(v, "-_")
	if len(v) > 63 {
		v = v[:63]
	}
	return v
}

func jobName(layerID, kind string, now time.Time) string {
	return "tc-" + layerID + "-" + kindShort[kind] + "-" + now.Format("0102-150405")
}

func envFromCM(name, cm, key string, optional bool) map[string]any {
	ref := map[string]any{"name": cm, "key": key}
	if optional {
		ref["optional"] = true
	}
	return map[string]any{"name": name, "valueFrom": map[string]any{"configMapKeyRef": ref}}
}

// buildJob 生成「下载 → 打包上传」Job：
// 代理是原生 sidecar（initContainer + restartPolicy: Always），下载是普通 initContainer，
// 下载结束后 sync 才启动，因此不依赖输出目录里残留的 .done。
// ProxyOpt 是单次派发的下载代理设置。
type ProxyOpt struct {
	Mode string `json:"mode"` // default / direct / pool / upstream
	URL  string `json:"url,omitempty"`
}

var proxySchemes = map[string]bool{"http": true, "https": true, "socks5": true, "socks5h": true}

// resolve 校验并把 default 落到图层清单的设置上，返回最终模式。
func (p ProxyOpt) resolve(l *LayerEntry, r *Registry) (ProxyOpt, error) {
	switch p.Mode {
	case "", "default":
		if l.Proxy {
			return ProxyOpt{Mode: "default"}, nil
		}
		return ProxyOpt{Mode: "direct"}, nil
	case "direct":
		return ProxyOpt{Mode: "direct"}, nil
	case "pool", "upstream":
		if r.Images.Proxy == "" {
			return p, errors.New("图层清单未配置 images.proxy，无法使用代理")
		}
		if p.Mode == "pool" && strings.TrimSpace(p.URL) == "" {
			return ProxyOpt{Mode: "pool"}, nil // 用 ConfigMap 里的 PROXY_POOL_API
		}
		u, err := url.Parse(strings.TrimSpace(p.URL))
		if err != nil || u.Host == "" {
			return p, fmt.Errorf("代理地址无效: %q", p.URL)
		}
		if p.Mode == "upstream" && !proxySchemes[u.Scheme] {
			return p, fmt.Errorf("上游代理只支持 http / https / socks5 / socks5h，收到 %q", u.Scheme)
		}
		if p.Mode == "pool" && u.Scheme != "http" && u.Scheme != "https" {
			return p, fmt.Errorf("代理池 API 需为 http(s) 地址，收到 %q", u.Scheme)
		}
		return ProxyOpt{Mode: p.Mode, URL: u.String()}, nil
	}
	return p, fmt.Errorf("未知代理模式 %q", p.Mode)
}

// describe 给页面展示用，隐去代理地址里的账号密码。
func (p ProxyOpt) describe() string {
	shown := p.URL
	if u, err := url.Parse(p.URL); err == nil {
		if u.User != nil {
			u.User = url.User("***")
		}
		if u.RawQuery != "" { // 提取链接的查询参数里有 key
			u.RawQuery = "***"
		}
		shown = u.String()
	}
	switch p.Mode {
	case "default":
		return "按图层默认（tiler-proxy-config）"
	case "direct":
		return "直连"
	case "pool":
		if shown == "" {
			return "代理池（tiler-proxy-config）"
		}
		return "代理池 " + shown
	case "upstream":
		return "固定上游 " + shown
	}
	return p.Mode
}

// Job 种类：下载与打包拆开，打包需人工确认后再派发；all 为下载完自动打包。
const (
	KindDownload = "download"
	KindPack     = "pack"
	KindAll      = "all"
)

var kindShort = map[string]string{KindDownload: "dl", KindPack: "pk", KindAll: "all"}

// buildJob 生成控制台 Job：
//   - download：代理为原生 sidecar（initContainer + restartPolicy: Always），download 为主容器；
//   - pack：只有 sync 主容器，把整个目录打包成 pmtiles 上传；
//   - all：download 为普通 initContainer，结束后 sync 才启动。
//
// sync 一律设 WAIT_FOR_DONE=0：顺序由容器编排保证，不依赖输出目录里残留的 .done。
// packMeta 是 packMetadata 的结果，交给 sync 在目录缺 metadata.json 时补进 pmtiles；为空则不设。
func buildJob(r *Registry, l *LayerEntry, conf *TilerConf, name, reason, kind string, proxy ProxyOpt, packMeta string) map[string]any {
	outputDir := conf.Output.Directory
	if outputDir == "" {
		outputDir = "/data/output"
	}
	volumes := []any{
		map[string]any{"name": "output", "persistentVolumeClaim": map[string]any{"claimName": r.OutputPVC}},
	}
	var initContainers, containers []any
	annotations := map[string]string{annoReason: reason}

	if kind == KindDownload || kind == KindAll {
		volumes = append(volumes,
			map[string]any{"name": "config", "configMap": map[string]any{"name": l.ConfigMap}},
			map[string]any{"name": "geojson", "configMap": map[string]any{"name": l.GeojsonConfigMap}},
		)
		downloadMounts := []any{
			map[string]any{"name": "config", "mountPath": "/etc/tiler"},
			map[string]any{"name": "geojson", "mountPath": r.GeojsonMountPath},
			map[string]any{"name": "output", "mountPath": outputDir},
		}
		var downloadEnv []any
		if proxy.Mode != "direct" {
			// 每个 Job 独立的 /tmp/proxy：ready 标记和切换触发文件不与其他 Job 共享
			volumes = append(volumes, map[string]any{"name": "proxy-data", "emptyDir": map[string]any{}})
			downloadMounts = append(downloadMounts, map[string]any{"name": "proxy-data", "mountPath": "/tmp/proxy"})
			var proxyEnv []any
			switch proxy.Mode {
			case "upstream":
				proxyEnv = []any{map[string]any{"name": "UPSTREAM_PROXY", "value": proxy.URL}}
			case "pool":
				if proxy.URL != "" {
					proxyEnv = []any{map[string]any{"name": "PROXY_POOL_API", "value": proxy.URL}}
				} else {
					proxyEnv = []any{envFromCM("PROXY_POOL_API", r.ProxyConfigMap, "PROXY_POOL_API", true)}
				}
			default:
				proxyEnv = []any{
					envFromCM("UPSTREAM_PROXY", r.ProxyConfigMap, "UPSTREAM_PROXY", true),
					envFromCM("PROXY_POOL_API", r.ProxyConfigMap, "PROXY_POOL_API", true),
				}
			}
			// 固定上游的账号直接写在地址里；代理池 / 提取链接拿到的代理用 Secret 里的账号认证
			if proxy.Mode != "upstream" && r.ProxySecret != "" {
				proxyEnv = append(proxyEnv, map[string]any{"name": "PROXY_AUTH", "valueFrom": map[string]any{
					"secretKeyRef": map[string]any{"name": r.ProxySecret, "key": "PROXY_AUTH", "optional": true}}})
			}
			initContainers = append(initContainers, map[string]any{
				"name":            "proxy",
				"image":           r.Images.Proxy,
				"imagePullPolicy": "Always",
				"restartPolicy":   "Always",
				"env":             proxyEnv,
				"volumeMounts":    []any{map[string]any{"name": "proxy-data", "mountPath": "/tmp/proxy"}},
				"resources":       resources("100m", "64Mi", "500m", "128Mi"),
			})
			downloadEnv = []any{
				map[string]any{"name": "HTTP_PROXY", "value": "http://localhost:8080"},
				map[string]any{"name": "HTTPS_PROXY", "value": "http://localhost:8080"},
			}
		}
		download := map[string]any{
			"name":            "download",
			"image":           r.Images.Tiler,
			"imagePullPolicy": "Always",
			"command":         []any{"./tiler", "-c", "/etc/tiler/conf.toml"},
			"volumeMounts":    downloadMounts,
			"resources":       resources("500m", "256Mi", "2", "512Mi"),
		}
		if downloadEnv != nil {
			download["env"] = downloadEnv
		}
		if kind == KindAll {
			initContainers = append(initContainers, download)
		} else {
			containers = append(containers, download)
		}
		annotations[annoProxy] = proxy.describe()
	}

	if kind == KindPack || kind == KindAll {
		syncEnv := []any{
			map[string]any{"name": "WAIT_FOR_DONE", "value": "0"},
			map[string]any{"name": "INPUT_DIR", "value": outputDir},
			map[string]any{"name": "TILE_NAME", "value": conf.TM.Name},
			map[string]any{"name": "MINIO_PATH", "value": l.MinioPath},
			envFromCM("MINIO_ENDPOINT", r.MinioConfigMap, "MINIO_ENDPOINT", false),
			envFromCM("MINIO_BUCKET", r.MinioConfigMap, "MINIO_BUCKET", false),
			envFromCM("MINIO_ACCESS_KEY", r.MinioConfigMap, "MINIO_ACCESS_KEY", false),
			map[string]any{"name": "MINIO_SECRET_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": r.MinioSecret, "key": "MINIO_SECRET_KEY"}}},
		}
		if packMeta != "" {
			syncEnv = append(syncEnv, map[string]any{"name": "TILE_METADATA", "value": packMeta})
		}
		containers = append(containers, map[string]any{
			"name":            "sync",
			"image":           r.Images.Sync,
			"imagePullPolicy": "Always",
			"env":             syncEnv,
			"volumeMounts":    []any{map[string]any{"name": "output", "mountPath": outputDir, "readOnly": true}},
			"resources":       resources("500m", "512Mi", "2", "2Gi"),
		})
	}

	labels := map[string]string{
		labelManaged:  managedValue,
		labelLayer:    l.ID,
		labelTileName: tileNameLabel(conf.TM.Name),
		labelKind:     kind,
	}
	podSpec := map[string]any{
		"restartPolicy": "Never",
		"containers":    containers,
		"volumes":       volumes,
	}
	if len(initContainers) > 0 {
		podSpec["initContainers"] = initContainers
	}
	if len(r.NodeSelector) > 0 {
		podSpec["nodeSelector"] = r.NodeSelector
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   r.Namespace,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"backoffLimit":            1,
			"ttlSecondsAfterFinished": r.JobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}
}

func resources(reqCPU, reqMem, limCPU, limMem string) map[string]any {
	return map[string]any{
		"requests": map[string]any{"cpu": reqCPU, "memory": reqMem},
		"limits":   map[string]any{"cpu": limCPU, "memory": limMem},
	}
}

func geojsonBase(p string) string { return path.Base(p) }
