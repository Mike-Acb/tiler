package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// 配置项来源：环境变量优先，其次 layers.json，最后默认值。
const (
	fromEnv     = "env"
	fromJSON    = "json"
	fromDefault = "default"
	fromNone    = "none"
)

type setting struct {
	Value string `json:"value,omitempty"`
	From  string `json:"from"`
}

func pick(env, jsonVal, def string) setting {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return setting{v, fromEnv}
	}
	if v := strings.TrimSpace(jsonVal); v != "" {
		return setting{v, fromJSON}
	}
	if def != "" {
		return setting{def, fromDefault}
	}
	return setting{From: fromNone}
}

// Storage 是启动时解析出的存储与预览配置。
//   - 下载目录：主存储，下载 Job 写入；控制台只读挂载，预览 A 直接读文件。
//   - MinIO：可选的上传目标，打包后的 pmtiles 存这里；预览 B 读它。
//   - 瓦片服务：可选的 go-pmtiles serve，预览 C 经控制台反向代理读它。
type Storage struct {
	OutputDir setting `json:"outputDir"`
	OutputPVC string  `json:"outputPVC"`

	MinioEndpoint setting `json:"minioEndpoint"`
	MinioBucket   setting `json:"minioBucket"`
	MinioUser     setting `json:"minioAccessKey"`
	MinioReady    bool    `json:"minioReady"` // 控制台自己能读 MinIO
	JobMinioRefs  bool    `json:"jobMinioRefs"`
	Upload        bool    `json:"upload"` // 打包上传可用：控制台能读 + Job 有凭证引用
	UploadReason  string  `json:"uploadReason,omitempty"`

	PMTilesPublicURL setting `json:"pmtilesPublicUrl"`
	TileServer       setting `json:"tileServer"`
	TileServerPrefix setting `json:"tileServerPrefix"`

	minio      *minio.Client
	bucket     string
	tileProxy  *httputil.ReverseProxy
	tileTarget *url.URL
}

func loadStorage(r *Registry) (*Storage, error) {
	s := &Storage{OutputPVC: r.OutputPVC}
	s.OutputDir = pick("OUTPUT_DIR", r.OutputDir, "/data/output")

	s.MinioEndpoint = pick("MINIO_ENDPOINT", r.MinioEndpoint, "")
	s.MinioBucket = pick("MINIO_BUCKET", r.MinioBucket, "")
	s.MinioUser = pick("MINIO_ACCESS_KEY", "", "")
	s.JobMinioRefs = r.MinioConfigMap != "" && r.MinioSecret != ""
	if s.MinioEndpoint.Value != "" {
		u, err := url.Parse(s.MinioEndpoint.Value)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("MinIO endpoint 无效: %q", s.MinioEndpoint.Value)
		}
		if s.MinioBucket.Value == "" {
			s.MinioBucket = setting{"open", fromDefault}
		}
		mc, err := minio.New(u.Host, &minio.Options{
			Creds:  credentials.NewStaticV4(os.Getenv("MINIO_ACCESS_KEY"), os.Getenv("MINIO_SECRET_KEY"), ""),
			Secure: u.Scheme == "https",
		})
		if err != nil {
			return nil, fmt.Errorf("初始化 MinIO 客户端失败: %w", err)
		}
		s.minio, s.bucket, s.MinioReady = mc, s.MinioBucket.Value, true
	}
	switch {
	case !s.MinioReady:
		s.UploadReason = "未配置 MinIO：只下载，不提供打包上传"
	case !s.JobMinioRefs:
		s.UploadReason = "图层清单缺少 minioConfigMap / minioSecret，打包 Job 拿不到 MinIO 凭证"
	default:
		s.Upload = true
	}

	s.PMTilesPublicURL = pick("PMTILES_PUBLIC_URL", r.PMTilesPublicURL, "")
	s.PMTilesPublicURL.Value = strings.TrimRight(s.PMTilesPublicURL.Value, "/")

	s.TileServer = pick("TILE_SERVER_URL", r.TileServer, "")
	s.TileServerPrefix = pick("TILE_SERVER_PREFIX", r.TileServerPrefix, "")
	if s.TileServer.Value != "" {
		u, err := url.Parse(s.TileServer.Value)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("瓦片服务地址无效: %q", s.TileServer.Value)
		}
		s.tileTarget = u
		s.tileProxy = newTileProxy(u)
	}
	return s, nil
}

func (s *Storage) logSummary() {
	log.Printf("下载目录 %s（%s）", s.OutputDir.Value, s.OutputDir.From)
	if s.MinioReady {
		log.Printf("MinIO %s / %s（%s）；打包上传：%v %s", s.MinioEndpoint.Value, s.bucket, s.MinioEndpoint.From, s.Upload, s.UploadReason)
	} else {
		log.Print(s.UploadReason)
	}
	if s.PMTilesPublicURL.Value != "" {
		log.Printf("pmtiles 公开地址 %s（%s）", s.PMTilesPublicURL.Value, s.PMTilesPublicURL.From)
	}
	if s.TileServer.Value != "" {
		log.Printf("瓦片服务 %s 前缀 %q（%s）", s.TileServer.Value, s.TileServerPrefix.Value, s.TileServer.From)
	}
}

// ---- 预览 A：下载目录 ----

// outputTilePath 返回下载目录里某张瓦片的文件路径，防止目录名或坐标跳出下载目录。
func outputTilePath(root, tileName string, z, x, y int, format string) (string, error) {
	if tileName == "" || strings.Contains(tileName, "..") || strings.ContainsAny(tileName, `/\`) {
		return "", fmt.Errorf("目录名无效: %q", tileName)
	}
	if format == "" || strings.ContainsAny(format, `./\`) {
		return "", fmt.Errorf("瓦片格式无效: %q", format)
	}
	p := filepath.Join(root, tileName, strconv.Itoa(z), strconv.Itoa(x), strconv.Itoa(y)+"."+format)
	if !strings.HasPrefix(p, filepath.Clean(root)+string(filepath.Separator)) {
		return "", errors.New("路径越界")
	}
	return p, nil
}

func parseZXY(r *http.Request) (z, x, y int, err error) {
	z, ez := strconv.Atoi(r.PathValue("z"))
	x, ex := strconv.Atoi(r.PathValue("x"))
	y, ey := strconv.Atoi(r.PathValue("y"))
	if ez != nil || ex != nil || ey != nil || z < 0 || z > 30 || x < 0 || y < 0 || x >= 1<<z || y >= 1<<z {
		return 0, 0, 0, errors.New("瓦片坐标无效")
	}
	return z, x, y, nil
}

var tileContentTypes = map[string]string{"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "webp": "image/webp", "pbf": "application/x-protobuf"}

// handleOutputTile 预览 A：直接读下载目录里的 z/x/y 文件，能看到还没打包的下载结果。
// 不存在或 0 字节（tiler 写的 404 标记）都回 204，MapLibre 当空瓦片处理。
func (s *Server) handleOutputTile(w http.ResponseWriter, r *http.Request) {
	l, ok := s.reg.layer(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	z, x, y, err := parseZXY(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := s.layerKeys(r.Context(), l)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	p, err := outputTilePath(s.store.OutputDir.Value, c.tileName, z, x, y, c.format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 || info.IsDir() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ct := tileContentTypes[c.format]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if c.format == "pbf" {
		w.Header().Set("Content-Encoding", "gzip") // tiler 存 pbf 时已 gzip
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	io.Copy(w, f)
}

// outputExists 下载目录里是否已有该图层的目录。
func (s *Server) outputExists(tileName string) bool {
	if tileName == "" || strings.ContainsAny(tileName, `/\`) || strings.Contains(tileName, "..") {
		return false
	}
	info, err := os.Stat(filepath.Join(s.store.OutputDir.Value, tileName))
	return err == nil && info.IsDir()
}

// ---- 预览 C：瓦片服务反向代理 ----

const tileServerRoute = "/tile-server"

// newTileProxy 把 /tile-server/<路径> 原样转给 go-pmtiles serve（z/x/y、metadata、TileJSON 都可用）。
// 范围外的瓦片（404）改回 204，避免 MapLibre 在浏览器里刷错误。
func newTileProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			rest := strings.TrimPrefix(pr.In.URL.Path, tileServerRoute)
			pr.Out.URL.Path = strings.TrimRight(target.Path, "/") + rest
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = "" // v= 只用于浏览器缓存，不往后传
			pr.Out.Host = target.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusNotFound && isTileRequest(resp.Request.URL.Path) {
				resp.Body.Close()
				resp.StatusCode, resp.Status = http.StatusNoContent, "204 No Content"
				resp.Body = http.NoBody
				resp.ContentLength = 0
				resp.Header.Del("Content-Length")
				resp.Header.Del("Content-Type")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "瓦片服务不可用: "+err.Error(), http.StatusBadGateway)
		},
	}
}

func isTileRequest(p string) bool {
	ext := strings.TrimPrefix(filepath.Ext(p), ".")
	_, ok := tileContentTypes[ext]
	return ok || ext == "mvt"
}

func (s *Server) handleTileServer(w http.ResponseWriter, r *http.Request) {
	if s.store.tileProxy == nil {
		http.Error(w, "未配置瓦片服务（TILE_SERVER_URL）", http.StatusNotFound)
		return
	}
	s.store.tileProxy.ServeHTTP(w, r)
}

// tileServerPath 把 MinIO 对象路径换算成 go-pmtiles serve 的瓦片集名：
// minioPath=map/tianditu、prefix=map、tm.name=satellite -> tianditu/satellite
func tileServerPath(minioPath, prefix, tileName string) (string, error) {
	p := strings.Trim(minioPath, "/")
	pre := strings.Trim(prefix, "/")
	if pre != "" {
		if p != pre && !strings.HasPrefix(p, pre+"/") {
			return "", fmt.Errorf("minioPath %q 不在瓦片服务前缀 %q 下", minioPath, prefix)
		}
		p = strings.TrimPrefix(strings.TrimPrefix(p, pre), "/")
	}
	name := strings.ReplaceAll(tileName, " ", "_")
	if p == "" {
		return name, nil
	}
	return p + "/" + name, nil
}
