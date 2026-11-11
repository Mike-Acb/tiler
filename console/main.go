package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
)

//go:embed web
var webFS embed.FS

type Server struct {
	reg      *Registry
	kube     *Kube
	store    *Storage
	password string
	dispatch sync.Mutex // 单副本内串行化派发，避免同一图层并发改 ConfigMap

	keyMu    sync.Mutex
	keyCache map[string]cachedKey // 图层 id -> 目录名 / 格式 / pmtiles key，避免每个瓦片请求都查 ConfigMap

	metaMu    sync.Mutex
	metaCache map[string]cachedMeta // pmtiles 对象 key -> 文件头/metadata（按 ETag 失效）
}

type cachedKey struct {
	key      string // MinIO 对象 key
	tileName string // 下载目录名（tm.name）
	format   string // tm.format，下载目录里的文件扩展名
	at       time.Time
}

func main() {
	reg, err := loadRegistry(envOr("LAYERS_FILE", "/etc/tiler-console/layers.json"))
	if err != nil {
		log.Fatal(err)
	}
	dryRun := os.Getenv("DRY_RUN") == "1"
	kube, err := newKube(reg.Namespace, dryRun)
	if err != nil {
		log.Fatal(err)
	}
	store, err := loadStorage(reg)
	if err != nil {
		log.Fatal(err)
	}
	store.logSummary()
	s := &Server{reg: reg, kube: kube, store: store, password: os.Getenv("CONSOLE_PASSWORD"), keyCache: map[string]cachedKey{}, metaCache: map[string]cachedMeta{}}
	if s.password == "" {
		log.Print("CONSOLE_PASSWORD 未设置：写操作（派发、删除 Job）全部禁用")
	}
	if dryRun {
		log.Print("DRY_RUN=1：所有写请求都带 dryRun=All，不会真正修改集群")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("POST /api/auth", s.auth(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) }))
	mux.HandleFunc("GET /api/layers", s.handleLayers)
	mux.HandleFunc("GET /api/layers/{id}", s.handleLayer)
	mux.HandleFunc("POST /api/layers/{id}/regions", s.auth(s.handleAddRegion))
	mux.HandleFunc("POST /api/layers/{id}/jobs", s.auth(s.handleRerun))
	mux.HandleFunc("POST /api/layers/{id}/pack", s.auth(s.handlePack))
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("GET /api/jobs/{name}", s.handleJob)
	mux.HandleFunc("GET /api/jobs/{name}/logs", s.handleLogs)
	mux.HandleFunc("DELETE /api/jobs/{name}", s.auth(s.handleDeleteJob))
	mux.HandleFunc("GET /pmtiles/{file}", s.handlePMTiles)
	mux.HandleFunc("HEAD /pmtiles/{file}", s.handlePMTiles)
	mux.HandleFunc("GET /tiles/output/{id}/{z}/{x}/{y}", s.handleOutputTile)
	mux.HandleFunc("GET "+tileServerRoute+"/{path...}", s.handleTileServer)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	addr := envOr("LISTEN", ":8080")
	log.Printf("tiler-console 监听 %s，命名空间 %s，图层 %d 个", addr, reg.Namespace, len(reg.Layers))
	log.Fatal(http.ListenAndServe(addr, logRequests(mux)))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	if errors.Is(err, errConflict) {
		code = http.StatusConflict
	} else if errors.Is(err, errNotFound) && code == 500 {
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.password == "" {
			writeErr(w, http.StatusForbidden, errors.New("服务端未配置口令，写操作已禁用"))
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.password)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("口令错误"))
			return
		}
		h(w, r)
	}
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"namespace": s.reg.Namespace,
		"writable":  s.password != "",
		"dryRun":    s.kube.dryRun,
		"proxy":     s.reg.Images.Proxy != "",
		"storage":   s.store,
	})
}

// ---- 图层 ----

type layerState struct {
	LayerEntry
	TileName string `json:"tileName,omitempty"` // 输出目录（ConfigMap tm.name），派发任务以它为准
	// 展示用：pmtiles 优先，缺失时用 ConfigMap 兜底；*From 标明来源（pmtiles / configmap）
	Name       string   `json:"name,omitempty"`
	NameFrom   string   `json:"nameFrom,omitempty"`
	Format     string   `json:"format,omitempty"`
	FormatFrom string   `json:"formatFrom,omitempty"`
	Min        int      `json:"min"`
	Max        int      `json:"max"`
	ZoomFrom   string   `json:"zoomFrom,omitempty"`
	ConfMin    int      `json:"confMin"` // ConfigMap 里的 tm.min / tm.max，派发时用
	ConfMax    int      `json:"confMax"`
	Missing    []string `json:"missing,omitempty"`  // pmtiles 缺失、已用 ConfigMap 兜底的项
	Mismatch   []string `json:"mismatch,omitempty"` // pmtiles 与 ConfigMap 不一致的项
	Meta       *PMMeta  `json:"meta,omitempty"`
	ConfFormat string   `json:"confFormat,omitempty"` // tm.format：下载目录里的文件扩展名
	// 各预览来源的可用性与地址
	Output         bool         `json:"output"`                   // 下载目录里已有该图层目录（预览 A）
	PMTilesURL     string       `json:"pmtilesUrl,omitempty"`     // 预览 B：pmtiles.js 读取地址
	TileServerPath string       `json:"tileServerPath,omitempty"` // 预览 C：go-pmtiles serve 的瓦片集名
	LrsCount       int          `json:"lrsCount"`
	Regions        []regionRef  `json:"regions"` // 已配置区域（geojson 键 + 层级），页面判断复制目标是否已有
	PMTilesKey     string       `json:"pmtilesKey,omitempty"`
	PMTiles        *objInfo     `json:"pmtiles,omitempty"`
	LastJob        *jobBrief    `json:"lastJob,omitempty"`
	PendingPack    *pendingPack `json:"pendingPack,omitempty"`
	SharedWith     []string     `json:"sharedWith,omitempty"`
	Error          string       `json:"error,omitempty"`
	conf           *TilerConf   `json:"-"`
	raw            string       `json:"-"`
}

// pendingPack 表示目录里有比当前 pmtiles 更新的下载结果，等待人工确认打包。
type pendingPack struct {
	Job   string    `json:"job"`
	Since time.Time `json:"since"`
}

type objInfo struct {
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func (s *Server) loadLayer(ctx context.Context, l *LayerEntry, withObject bool) *layerState {
	st := &layerState{LayerEntry: *l}
	cm, err := s.kube.GetConfigMap(l.ConfigMap)
	if err != nil {
		st.Error = "读取 ConfigMap " + l.ConfigMap + " 失败: " + err.Error()
		return st
	}
	st.raw = cm.Data["conf.toml"]
	conf, err := parseConf(st.raw)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.conf = conf
	st.TileName, st.LrsCount = conf.TM.Name, len(conf.Lrs)
	st.Regions = make([]regionRef, 0, len(conf.Lrs))
	for _, x := range conf.Lrs {
		st.Regions = append(st.Regions, regionRef{Key: geojsonBase(x.Geojson), Min: x.Min, Max: x.Max})
	}
	st.ConfMin, st.ConfMax = conf.TM.Min, conf.TM.Max
	st.ConfFormat = conf.TM.Format
	st.PMTilesKey = pmtilesKey(l.MinioPath, conf.TM.Name)
	if s.store.MinioReady {
		// 预览 B：配置了公开地址就让浏览器直连，否则经控制台 /pmtiles/ 中转
		if s.store.PMTilesPublicURL.Value != "" {
			st.PMTilesURL = s.store.PMTilesPublicURL.Value + "/" + st.PMTilesKey
		} else {
			st.PMTilesURL = "/pmtiles/" + url.PathEscape(l.ID) + ".pmtiles"
		}
	}
	if s.store.tileProxy != nil {
		if p, err := tileServerPath(l.MinioPath, s.store.TileServerPrefix.Value, conf.TM.Name); err == nil {
			st.TileServerPath = p
		}
	}
	if withObject {
		st.Output = s.outputExists(conf.TM.Name)
		if s.store.MinioReady {
			if info, err := s.store.minio.StatObject(ctx, s.store.bucket, st.PMTilesKey, minio.StatObjectOptions{}); err == nil {
				st.PMTiles = &objInfo{Size: info.Size, Modified: info.LastModified}
				st.Meta = s.readPMMeta(ctx, st.PMTilesKey, info.ETag)
			}
		}
	}
	resolveInfo(st)
	return st
}

func (s *Server) loadAllLayers(ctx context.Context) []*layerState {
	out := make([]*layerState, len(s.reg.Layers))
	var wg sync.WaitGroup
	for i := range s.reg.Layers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = s.loadLayer(ctx, &s.reg.Layers[i], true)
		}(i)
	}
	wg.Wait()
	byDir := map[string][]string{}
	for _, st := range out {
		if st.TileName != "" {
			byDir[st.TileName] = append(byDir[st.TileName], st.ID)
		}
	}
	for _, st := range out {
		for _, id := range byDir[st.TileName] {
			if id != st.ID {
				st.SharedWith = append(st.SharedWith, id)
			}
		}
	}
	return out
}

func (s *Server) handleLayers(w http.ResponseWriter, r *http.Request) {
	layers := s.loadAllLayers(r.Context())
	jobs, err := s.kube.ListJobs(labelManaged + "=" + managedValue)
	if err == nil {
		latest := map[string]*Job{}
		for i := range jobs {
			j := &jobs[i]
			id := j.Metadata.Labels[labelLayer]
			if cur, ok := latest[id]; !ok || created(j).After(created(cur)) {
				latest[id] = j
			}
		}
		for _, st := range layers {
			if j, ok := latest[st.ID]; ok {
				b := briefJob(j)
				st.LastJob = &b
			}
			if s.store.Upload { // 没有上传目标就谈不上「待打包」
				st.PendingPack = pendingFor(st, jobs)
			}
		}
	}
	writeJSON(w, 200, layers)
}

// pendingFor：同目录最近一次成功下载晚于最近一次成功打包（及 pmtiles 修改时间），即为待打包。
// 下载 Job 过了 TTL 被清理后，这个提示也随之消失。
func pendingFor(st *layerState, jobs []Job) *pendingPack {
	if st.TileName == "" {
		return nil
	}
	dir := tileNameLabel(st.TileName)
	var lastDL *Job
	var lastPack time.Time
	if st.PMTiles != nil {
		lastPack = st.PMTiles.Modified
	}
	for i := range jobs {
		j := &jobs[i]
		if j.Metadata.Labels[labelTileName] != dir || j.Phase() != "Succeeded" || j.Status.CompletionTime == nil {
			continue
		}
		done := *j.Status.CompletionTime
		switch jobKind(j) {
		case KindDownload:
			if lastDL == nil || done.After(*lastDL.Status.CompletionTime) {
				lastDL = j
			}
		default:
			if done.After(lastPack) {
				lastPack = done
			}
		}
	}
	if lastDL == nil || !lastDL.Status.CompletionTime.After(lastPack) {
		return nil
	}
	return &pendingPack{Job: lastDL.Metadata.Name, Since: *lastDL.Status.CompletionTime}
}

func jobKind(j *Job) string {
	if k := j.Metadata.Labels[labelKind]; k != "" {
		return k
	}
	return KindAll
}

var tkParam = regexp.MustCompile(`((?:tk|key|access_token)=)[^&"\s]+`)

func (s *Server) handleLayer(w http.ResponseWriter, r *http.Request) {
	l, ok := s.reg.layer(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, errors.New("图层不存在"))
		return
	}
	st := s.loadLayer(r.Context(), l, true)
	if st.conf == nil {
		writeErr(w, 500, errors.New(st.Error))
		return
	}
	gcm, err := s.kube.GetConfigMap(l.GeojsonConfigMap)
	if err != nil {
		writeErr(w, 500, fmt.Errorf("读取 ConfigMap %s 失败: %w", l.GeojsonConfigMap, err))
		return
	}
	type lrsView struct {
		Lrs
		Key     string          `json:"key"`
		Missing bool            `json:"missing,omitempty"`
		Data    json.RawMessage `json:"data,omitempty"`
	}
	lrs := []lrsView{}
	for _, x := range st.conf.Lrs {
		v := lrsView{Lrs: x, Key: geojsonBase(x.Geojson)}
		if data, ok := gcm.Data[v.Key]; ok && json.Valid([]byte(data)) {
			v.Data = json.RawMessage(data)
		} else {
			v.Missing = true
		}
		v.URL = tkParam.ReplaceAllString(v.URL, "${1}***")
		lrs = append(lrs, v)
	}
	writeJSON(w, 200, map[string]any{
		"layer": st,
		"lrs":   lrs,
		"conf":  tkParam.ReplaceAllString(st.raw, "${1}***"),
	})
}

type addRegionReq struct {
	Proxy    ProxyOpt        `json:"proxy"`
	AutoPack bool            `json:"autoPack"`
	Name     string          `json:"name"`
	Min      int             `json:"min"`
	Max      int             `json:"max"`
	Geojson  json.RawMessage `json:"geojson"`
	// From 复制已配置区域：取源图层 geojson ConfigMap 里的这个键，不传 Geojson
	From *regionRef `json:"from,omitempty"`
}

func (s *Server) handleAddRegion(w http.ResponseWriter, r *http.Request) {
	l, ok := s.reg.layer(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, errors.New("图层不存在"))
		return
	}
	var req addRegionReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
		writeErr(w, 400, fmt.Errorf("请求体无效: %w", err))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, 400, errors.New("请填写区域名称"))
		return
	}
	if req.Min < 0 || req.Max < req.Min || req.Max > 22 {
		writeErr(w, 400, fmt.Errorf("层级范围无效: %d-%d", req.Min, req.Max))
		return
	}
	var geo []byte
	var src *LayerEntry
	var err error
	if req.From != nil {
		src, geo, err = s.regionData(req.From)
	} else {
		geo, err = normalizeRegion(req.Geojson, name)
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	proxy, err := req.Proxy.resolve(l, s.reg)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.AutoPack && !s.store.Upload {
		writeErr(w, 400, errors.New(s.store.UploadReason))
		return
	}

	s.dispatch.Lock()
	defer s.dispatch.Unlock()

	st := s.loadLayer(r.Context(), l, false)
	if st.conf == nil {
		writeErr(w, 500, errors.New(st.Error))
		return
	}
	if err := s.ensureIdle(st.TileName); err != nil {
		writeErr(w, 409, err)
		return
	}

	now := time.Now()
	gcm, err := s.kube.GetConfigMap(l.GeojsonConfigMap)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if gcm.Data == nil {
		gcm.Data = map[string]string{}
	}
	key, write := pickRegionKey(gcm.Data, req.From, geo, name, now)
	if hasRegion(st.conf, key, req.Min, req.Max) {
		writeErr(w, 409, fmt.Errorf("图层已有区域 %s（%d-%d 级）", key, req.Min, req.Max))
		return
	}
	if write {
		if _, exists := gcm.Data[key]; exists {
			writeErr(w, 409, fmt.Errorf("geojson %s 已存在", key))
			return
		}
		total := len(geo)
		for k, v := range gcm.Data {
			total += len(k) + len(v)
		}
		if total > 900*1024 {
			writeErr(w, 400, fmt.Errorf("ConfigMap %s 加上新区域约 %d KB，超过 900 KB 安全上限，请简化多边形", l.GeojsonConfigMap, total/1024))
			return
		}
	}

	ccm, err := s.kube.GetConfigMap(l.ConfigMap)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	newConf, err := appendLrs(ccm.Data["conf.toml"], Lrs{Min: req.Min, Max: req.Max, Geojson: strings.TrimRight(s.reg.GeojsonMountPath, "/") + "/" + key})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	parsed, _ := parseConf(newConf)

	if write {
		gcm.Data[key] = string(geo)
		if err := s.kube.UpdateConfigMap(gcm); err != nil {
			writeErr(w, 500, fmt.Errorf("写入 geojson 失败: %w", err))
			return
		}
	}
	ccm.Data["conf.toml"] = newConf
	if err := s.kube.UpdateConfigMap(ccm); err != nil {
		writeErr(w, 500, fmt.Errorf("geojson %s 已写入，但更新 conf.toml 失败: %w", key, err))
		return
	}
	kind := downloadKind(req.AutoPack)
	jn := jobName(l.ID, kind, now)
	reason := fmt.Sprintf("新增区域 %s（%d-%d 级）", name, req.Min, req.Max)
	if src != nil && src.ID != l.ID {
		reason = fmt.Sprintf("复制区域 %s（%d-%d 级，来自%s）", name, req.Min, req.Max, src.Title)
	}
	meta := ""
	if kind == KindAll {
		meta = packMetadata(parsed, gcm.Data)
	}
	if _, err := s.kube.CreateJob(buildJob(s.reg, l, parsed, jn, reason, kind, proxy, meta)); err != nil {
		writeErr(w, 500, fmt.Errorf("区域已写入配置，但创建 Job 失败（可在页面点「重新下载」）: %w", err))
		return
	}
	log.Printf("派发 %s：图层 %s %s", jn, l.ID, reason)
	writeJSON(w, 201, map[string]any{"job": jn, "geojsonKey": key, "dryRun": s.kube.dryRun})
}

// regionData 读取某图层已配置区域的 geojson，复制区域时用。
func (s *Server) regionData(ref *regionRef) (*LayerEntry, []byte, error) {
	src, ok := s.reg.layer(ref.Layer)
	if !ok {
		return nil, nil, fmt.Errorf("源图层 %q 不存在", ref.Layer)
	}
	if ref.Key == "" || strings.ContainsAny(ref.Key, `/\`) {
		return nil, nil, fmt.Errorf("源区域 %q 无效", ref.Key)
	}
	cm, err := s.kube.GetConfigMap(src.GeojsonConfigMap)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 ConfigMap %s 失败: %w", src.GeojsonConfigMap, err)
	}
	data, ok := cm.Data[ref.Key]
	if !ok || !json.Valid([]byte(data)) {
		return nil, nil, fmt.Errorf("%s 里没有区域 %s", src.GeojsonConfigMap, ref.Key)
	}
	return src, []byte(data), nil
}

func (s *Server) handleRerun(w http.ResponseWriter, r *http.Request) {
	l, ok := s.reg.layer(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, errors.New("图层不存在"))
		return
	}
	var req struct {
		Proxy    ProxyOpt `json:"proxy"`
		AutoPack bool     `json:"autoPack"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && err != io.EOF {
			writeErr(w, 400, fmt.Errorf("请求体无效: %w", err))
			return
		}
	}
	proxy, err := req.Proxy.resolve(l, s.reg)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.dispatch.Lock()
	defer s.dispatch.Unlock()
	st := s.loadLayer(r.Context(), l, false)
	if st.conf == nil {
		writeErr(w, 500, errors.New(st.Error))
		return
	}
	if err := s.ensureIdle(st.TileName); err != nil {
		writeErr(w, 409, err)
		return
	}
	if req.AutoPack && !s.store.Upload {
		writeErr(w, 400, errors.New(s.store.UploadReason))
		return
	}
	kind := downloadKind(req.AutoPack)
	jn := jobName(l.ID, kind, time.Now())
	meta := ""
	if kind == KindAll {
		meta = s.packMetaFor(l, st.conf)
	}
	if _, err := s.kube.CreateJob(buildJob(s.reg, l, st.conf, jn, "按当前配置重新下载", kind, proxy, meta)); err != nil {
		writeErr(w, 500, err)
		return
	}
	log.Printf("派发 %s：图层 %s 重新下载", jn, l.ID)
	writeJSON(w, 201, map[string]any{"job": jn, "dryRun": s.kube.dryRun})
}

func downloadKind(autoPack bool) string {
	if autoPack {
		return KindAll
	}
	return KindDownload
}

// handlePack 人工确认后打包：把整个目录打成 pmtiles，覆盖 MinIO 上的同名文件。
func (s *Server) handlePack(w http.ResponseWriter, r *http.Request) {
	l, ok := s.reg.layer(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, errors.New("图层不存在"))
		return
	}
	if !s.store.Upload {
		writeErr(w, 400, errors.New(s.store.UploadReason))
		return
	}
	s.dispatch.Lock()
	defer s.dispatch.Unlock()
	st := s.loadLayer(r.Context(), l, false)
	if st.conf == nil {
		writeErr(w, 500, errors.New(st.Error))
		return
	}
	if err := s.ensureIdle(st.TileName); err != nil {
		writeErr(w, 409, err)
		return
	}
	jn := jobName(l.ID, KindPack, time.Now())
	reason := "打包上传 " + st.PMTilesKey
	if _, err := s.kube.CreateJob(buildJob(s.reg, l, st.conf, jn, reason, KindPack, ProxyOpt{Mode: "direct"}, s.packMetaFor(l, st.conf))); err != nil {
		writeErr(w, 500, err)
		return
	}
	log.Printf("派发 %s：图层 %s 打包上传", jn, l.ID)
	writeJSON(w, 201, map[string]any{"job": jn, "dryRun": s.kube.dryRun})
}

// ensureIdle 同一输出目录同时只允许一个控制台 Job，避免两次打包互相覆盖。
func (s *Server) ensureIdle(tileName string) error {
	jobs, err := s.kube.ListJobs(labelManaged + "=" + managedValue + "," + labelTileName + "=" + tileNameLabel(tileName))
	if err != nil {
		return fmt.Errorf("检查运行中的 Job 失败: %w", err)
	}
	for i := range jobs {
		if p := jobs[i].Phase(); p == "Running" || p == "Pending" {
			return fmt.Errorf("目录 %s 已有 Job %s 在执行，等它结束后再派发", tileName, jobs[i].Metadata.Name)
		}
	}
	return nil
}

// ---- Job ----

type jobBrief struct {
	Name      string     `json:"name"`
	Layer     string     `json:"layer,omitempty"`
	Managed   bool       `json:"managed"`
	Kind      string     `json:"kind,omitempty"`
	Phase     string     `json:"phase"`
	Reason    string     `json:"reason,omitempty"`
	Proxy     string     `json:"proxy,omitempty"`
	Created   *time.Time `json:"created,omitempty"`
	Started   *time.Time `json:"started,omitempty"`
	Completed *time.Time `json:"completed,omitempty"`
	Message   string     `json:"message,omitempty"`
}

func created(j *Job) time.Time {
	if j.Metadata.CreationTimestamp != nil {
		return *j.Metadata.CreationTimestamp
	}
	return time.Time{}
}

func briefJob(j *Job) jobBrief {
	b := jobBrief{
		Name:      j.Metadata.Name,
		Layer:     j.Metadata.Labels[labelLayer],
		Managed:   j.Metadata.Labels[labelManaged] == managedValue,
		Phase:     j.Phase(),
		Reason:    j.Metadata.Annotations[annoReason],
		Proxy:     j.Metadata.Annotations[annoProxy],
		Created:   j.Metadata.CreationTimestamp,
		Started:   j.Status.StartTime,
		Completed: j.Status.CompletionTime,
	}
	if b.Managed {
		b.Kind = jobKind(j)
	}
	for _, c := range j.Status.Conditions {
		if c.Status == "True" && c.Message != "" {
			b.Message = c.Message
		}
	}
	return b
}

func isLegacyJob(name string) bool {
	return strings.HasPrefix(name, "tiler-") || strings.HasPrefix(name, "pmtiles-") || strings.HasPrefix(name, "coastline-")
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	layer := r.URL.Query().Get("layer")
	jobs, err := s.kube.ListJobs("")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := []jobBrief{}
	for i := range jobs {
		j := &jobs[i]
		managed := j.Metadata.Labels[labelManaged] == managedValue
		if !managed && !isLegacyJob(j.Metadata.Name) {
			continue
		}
		if layer != "" && j.Metadata.Labels[labelLayer] != layer {
			continue
		}
		out = append(out, briefJob(j))
	}
	sort.Slice(out, func(a, b int) bool {
		ta, tb := out[a].Created, out[b].Created
		if ta == nil || tb == nil {
			return tb == nil && ta != nil
		}
		return ta.After(*tb)
	})
	writeJSON(w, 200, out)
}

type containerView struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // init / sidecar / main
	State    string `json:"state"`
	Detail   string `json:"detail,omitempty"`
	Restarts int    `json:"restarts"`
}

type podView struct {
	Name       string          `json:"name"`
	Phase      string          `json:"phase"`
	Node       string          `json:"node,omitempty"`
	Created    *time.Time      `json:"created,omitempty"`
	Containers []containerView `json:"containers"`
}

func containerState(cs *ContainerStatus) (string, string) {
	if cs == nil {
		return "waiting", ""
	}
	switch {
	case cs.State.Running != nil:
		return "running", ""
	case cs.State.Terminated != nil:
		t := cs.State.Terminated
		if t.ExitCode == 0 {
			return "done", t.Reason
		}
		return "failed", fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
	case cs.State.Waiting != nil:
		return "waiting", strings.TrimSpace(cs.State.Waiting.Reason + " " + cs.State.Waiting.Message)
	}
	return "waiting", ""
}

func viewPod(p *Pod, sidecars map[string]bool) podView {
	v := podView{Name: p.Metadata.Name, Phase: p.Status.Phase, Node: p.Spec.NodeName, Created: p.Metadata.CreationTimestamp}
	find := func(list []ContainerStatus, name string) *ContainerStatus {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
		return nil
	}
	for _, c := range p.Spec.InitContainers {
		cs := find(p.Status.InitContainerStatuses, c.Name)
		state, detail := containerState(cs)
		kind := "init"
		if sidecars[c.Name] {
			kind = "sidecar"
		}
		cv := containerView{Name: c.Name, Kind: kind, State: state, Detail: detail}
		if cs != nil {
			cv.Restarts = cs.RestartCount
		}
		v.Containers = append(v.Containers, cv)
	}
	for _, c := range p.Spec.Containers {
		cs := find(p.Status.ContainerStatuses, c.Name)
		state, detail := containerState(cs)
		cv := containerView{Name: c.Name, Kind: "main", State: state, Detail: detail}
		if cs != nil {
			cv.Restarts = cs.RestartCount
		}
		v.Containers = append(v.Containers, cv)
	}
	return v
}

func jobSidecars(j *Job) map[string]bool {
	out := map[string]bool{}
	tpl, _ := j.Spec["template"].(map[string]any)
	spec, _ := tpl["spec"].(map[string]any)
	inits, _ := spec["initContainers"].([]any)
	for _, c := range inits {
		m, _ := c.(map[string]any)
		if m["restartPolicy"] == "Always" {
			name, _ := m["name"].(string)
			out[name] = true
		}
	}
	return out
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	j, err := s.kube.GetJob(name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	pods, err := s.kube.ListPods("job-name=" + name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	sort.Slice(pods, func(a, b int) bool {
		ta, tb := pods[a].Metadata.CreationTimestamp, pods[b].Metadata.CreationTimestamp
		return ta != nil && tb != nil && ta.After(*tb)
	})
	sc := jobSidecars(j)
	views := make([]podView, 0, len(pods))
	for i := range pods {
		views = append(views, viewPod(&pods[i], sc))
	}
	writeJSON(w, 200, map[string]any{"job": briefJob(j), "pods": views})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	q := r.URL.Query()
	pod, container := q.Get("pod"), q.Get("container")
	tail, _ := strconv.Atoi(q.Get("tail"))
	if tail <= 0 || tail > 5000 {
		tail = 500
	}
	pods, err := s.kube.ListPods("job-name=" + name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	belongs := false
	for _, p := range pods {
		if p.Metadata.Name == pod {
			belongs = true
		}
	}
	if !belongs {
		writeErr(w, 404, errors.New("Pod 不属于该 Job"))
		return
	}
	rc, err := s.kube.StreamLogs(pod, container, tail, q.Get("follow") == "1")
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	defer rc.Close()
	go func() { <-r.Context().Done(); rc.Close() }()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	j, err := s.kube.GetJob(name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if j.Metadata.Labels[labelManaged] != managedValue {
		writeErr(w, 403, errors.New("只能删除控制台派发的 Job"))
		return
	}
	if err := s.kube.DeleteJob(name); err != nil {
		writeErr(w, 500, err)
		return
	}
	log.Printf("删除 Job %s", name)
	writeJSON(w, 200, map[string]any{"deleted": name, "dryRun": s.kube.dryRun})
}

// ---- PMTiles 代理 ----

// handlePMTiles 把 MinIO 上的 pmtiles 以支持 Range 的方式转给浏览器，浏览器不需要直连 MinIO。
func (s *Server) handlePMTiles(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("file"), ".pmtiles")
	l, ok := s.reg.layer(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	c, err := s.layerKeys(r.Context(), l)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !s.store.MinioReady {
		http.Error(w, "未配置 MinIO", http.StatusNotFound)
		return
	}
	key := c.key
	obj, err := s.store.minio.GetObject(r.Context(), s.store.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer obj.Close()
	info, err := obj.Stat()
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			http.Error(w, "pmtiles 尚未生成: "+key, http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", `"`+info.ETag+`"`)
	http.ServeContent(w, r, "", info.LastModified, obj)
}

func (s *Server) layerKeys(ctx context.Context, l *LayerEntry) (cachedKey, error) {
	s.keyMu.Lock()
	c, ok := s.keyCache[l.ID]
	s.keyMu.Unlock()
	if ok && time.Since(c.at) < 30*time.Second {
		return c, nil
	}
	st := s.loadLayer(ctx, l, false)
	if st.conf == nil {
		return cachedKey{}, errors.New(st.Error)
	}
	c = cachedKey{key: st.PMTilesKey, tileName: st.TileName, format: st.conf.TM.Format, at: time.Now()}
	s.keyMu.Lock()
	s.keyCache[l.ID] = c
	s.keyMu.Unlock()
	return c, nil
}
