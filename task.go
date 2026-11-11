package main

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	"github.com/paulmach/orb/maptile/tilecover"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/teris-io/shortid"
	pb "gopkg.in/cheggaaa/pb.v1"
)

// MBTileVersion mbtiles版本号
const MBTileVersion = "1.2"

// Task 下载任务
type Task struct {
	ID                 string
	Name               string
	Description        string
	File               string
	Min                int
	Max                int
	Layers             []Layer
	TileMap            TileMap
	Total              int64
	Current            int64
	Bar                *pb.ProgressBar
	db                 *sql.DB
	workerCount        int
	savePipeSize       int
	timeDelay          int
	bufSize            int
	tileWG             sync.WaitGroup
	abort, pause, play chan struct{}
	workers            chan maptile.Tile
	savingpipe         chan Tile
	tileSet            Set
	outformat          string
}

// NewTask 创建下载任务
func NewTask(layers []Layer, m TileMap) *Task {
	if len(layers) == 0 {
		return nil
	}
	id, _ := shortid.Generate()

	task := Task{
		ID:      id,
		Name:    m.Name,
		Layers:  layers,
		Min:     m.Min,
		Max:     m.Max,
		TileMap: m,
	}

	for i := 0; i < len(layers); i++ {
		if layers[i].URL == "" {
			layers[i].URL = m.URL
		}
		layers[i].Count = tilecover.CollectionCount(layers[i].Collection, maptile.Zoom(layers[i].Zoom))
		log.Printf("zoom: %d, tiles: %d \n", layers[i].Zoom, layers[i].Count)
		task.Total += layers[i].Count
	}
	task.abort = make(chan struct{})
	task.pause = make(chan struct{})
	task.play = make(chan struct{})

	task.workerCount = viper.GetInt("task.workers")
	task.savePipeSize = viper.GetInt("task.savepipe")
	task.timeDelay = viper.GetInt("task.timedelay")
	task.workers = make(chan maptile.Tile, task.workerCount)
	task.savingpipe = make(chan Tile, task.savePipeSize)
	task.bufSize = viper.GetInt("task.mergebuf")
	task.tileSet = Set{M: make(maptile.Set)}

	task.outformat = viper.GetString("output.format")
	return &task
}

// Bound 范围
func (task *Task) Bound() orb.Bound {
	var bound orb.Bound
	hasGeometry := false
	for _, layer := range task.Layers {
		for _, g := range layer.Collection {
			if !hasGeometry {
				bound = g.Bound()
				hasGeometry = true
				continue
			}
			bound = bound.Union(g.Bound())
		}
	}
	return bound
}

// Center 中心点
func (task *Task) Center() orb.Point {
	layer := task.Layers[len(task.Layers)-1]
	if len(layer.Collection) == 0 {
		return orb.Point{}
	}
	bound := layer.Collection[0].Bound()
	for _, g := range layer.Collection[1:] {
		bound = bound.Union(g.Bound())
	}
	return bound.Center()
}

// MetaItems 输出
func (task *Task) MetaItems() map[string]string {
	b := task.Bound()
	c := task.Center()
	data := map[string]string{
		"id":          task.ID,
		"name":        task.Name,
		"description": task.Description,
		"attribution": `<a href="http://www.atlasdata.cn/" target="_blank">&copy; MapCloud</a>`,
		"basename":    task.TileMap.Name,
		"format":      task.TileMap.Format,
		"type":        task.TileMap.Schema,
		"pixel_scale": strconv.Itoa(TileSize),
		"version":     MBTileVersion,
		"bounds":      fmt.Sprintf(`%f,%f,%f,%f`, b.Left(), b.Bottom(), b.Right(), b.Top()),
		"center":      fmt.Sprintf(`%f,%f,%d`, c.X(), c.Y(), (task.Min+task.Max)/2),
		"minzoom":     strconv.Itoa(task.Min),
		"maxzoom":     strconv.Itoa(task.Max),
		"json":        task.TileMap.JSON,
	}
	return data
}

func (task *Task) writeFileMetadata() error {
	data, err := json.MarshalIndent(task.MetaItems(), "", "  ")
	if err != nil {
		return fmt.Errorf("生成 metadata.json 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(task.File, "metadata.json"), data, 0644); err != nil {
		return fmt.Errorf("写入 metadata.json 失败: %w", err)
	}
	return nil
}

// SetupMBTileTables 初始化配置MBTile库
func (task *Task) SetupMBTileTables() error {

	if task.File == "" {
		outdir := viper.GetString("output.directory")
		os.MkdirAll(outdir, os.ModePerm)
		task.File = filepath.Join(outdir, task.Name+".mbtiles")
	}
	os.Remove(task.File)
	db, err := sql.Open("sqlite3", task.File)
	if err != nil {
		return err
	}

	err = optimizeConnection(db)
	if err != nil {
		return err
	}

	_, err = db.Exec("create table if not exists tiles (zoom_level integer, tile_column integer, tile_row integer, tile_data blob);")
	if err != nil {
		return err
	}

	_, err = db.Exec("create table if not exists metadata (name text, value text);")
	if err != nil {
		return err
	}

	_, err = db.Exec("create unique index name on metadata (name);")
	if err != nil {
		return err
	}

	_, err = db.Exec("create unique index tile_index on tiles(zoom_level, tile_column, tile_row);")
	if err != nil {
		return err
	}

	// Load metadata.
	for name, value := range task.MetaItems() {
		_, err := db.Exec("insert into metadata (name, value) values (?, ?)", name, value)
		if err != nil {
			return err
		}
	}

	task.db = db //保存任务的库连接
	return nil
}

func (task *Task) abortFun() {
	// os.Stdin.Read(make([]byte, 1)) // read a single byte
	// <-time.After(8 * time.Second)
	task.abort <- struct{}{}
}

func (task *Task) pauseFun() {
	// os.Stdin.Read(make([]byte, 1)) // read a single byte
	// <-time.After(3 * time.Second)
	task.pause <- struct{}{}
}

func (task *Task) playFun() {
	// os.Stdin.Read(make([]byte, 1)) // read a single byte
	// <-time.After(5 * time.Second)
	task.play <- struct{}{}
}

// SavePipe 保存瓦片管道
func (task *Task) savePipe() {
	for tile := range task.savingpipe {
		err := saveToMBTile(tile, task.db)
		if err != nil {
			if strings.HasPrefix(err.Error(), "UNIQUE constraint failed") {
				log.Warnf("save %v tile to mbtiles db error ~ %s", tile.T, err)
			} else {
				log.Errorf("save %v tile to mbtiles db error ~ %s", tile.T, err)
			}
		}
	}
}

// SaveTile 保存瓦片
func (task *Task) saveTile(tile Tile) error {
	// defer task.wg.Done()
	err := saveToFiles(tile, task)
	if err != nil {
		log.Errorf("create %v tile file error ~ %s", tile.T, err)
	}
	return nil
}

// fetchTile 发送单次 HTTP 请求获取瓦片数据
func fetchTile(tileURL string) ([]byte, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest("GET", tileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://map.tianditu.gov.cn")
	resp, err := client.Do(req)
	if err != nil {
		if usingProxy() { // 连不上代理、代理连不上上游、超时：多半是代理过期了
			triggerProxySwitch("请求失败")
			return nil, fmt.Errorf("%w: 发送请求失败: %v", errProxy, err)
		}
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, errNotFound
	}
	if usingProxy() && isProxyStatus(resp.StatusCode) {
		triggerProxySwitch(strconv.Itoa(resp.StatusCode))
		return nil, fmt.Errorf("%w: 状态码 %d", errProxy, resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("状态码: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if len(body) == 0 {
		return nil, nil
	}
	return body, nil
}

// isProxyStatus 判断走代理时的响应码是否说明代理出了问题：
// 407 认证失效（短效代理到期）、418 IP 被天地图封、502/503/504 代理连不上上游。
func isProxyStatus(code int) bool {
	switch code {
	case 407, 418, 502, 503, 504:
		return true
	}
	return false
}

func validateTileData(format string, body []byte) error {
	if len(body) == 0 {
		return errors.New("瓦片内容为空")
	}

	switch strings.ToLower(format) {
	case JPG, "jpeg":
		if contentType := http.DetectContentType(body); contentType != "image/jpeg" {
			return fmt.Errorf("期望 image/jpeg，实际为 %s", contentType)
		}
	case PNG:
		if contentType := http.DetectContentType(body); contentType != "image/png" {
			return fmt.Errorf("期望 image/png，实际为 %s", contentType)
		}
	case WEBP:
		if contentType := http.DetectContentType(body); contentType != "image/webp" {
			return fmt.Errorf("期望 image/webp，实际为 %s", contentType)
		}
	case PBF:
		// Vector tiles do not have a reliable magic number. A non-empty body is
		// accepted here and is gzip-compressed before storage below.
	default:
		return fmt.Errorf("不支持的瓦片格式: %s", format)
	}

	return nil
}

var errNotFound = errors.New("404")

// lastProxySwitch 是上次通知换代理的时间（UnixNano），用来做 10 秒内只通知一次。
var lastProxySwitch atomic.Int64

// proxySwitchPath 是与代理边车共享的切换通知文件（测试里改到临时目录）。
var proxySwitchPath = "/tmp/proxy/switch"

// errProxy 表示请求坏在代理上（代理过期、认证失效、被封），换代理后重试即可。
var errProxy = errors.New("代理不可用")

// maxProxyRetries 是单张瓦片因代理失败而重试的次数；短效代理约 1 分钟一换，换的间隙会有少量请求失败。
const maxProxyRetries = 5

func usingProxy() bool {
	return os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != ""
}

// triggerProxySwitch 通知代理边车立即换代理（边车每 3 秒检查一次 /tmp/proxy/switch）；10 秒内只触发一次。
func triggerProxySwitch(reason string) {
	now := time.Now().UnixNano()
	last := lastProxySwitch.Load()
	if now-last < int64(10*time.Second) || !lastProxySwitch.CompareAndSwap(last, now) {
		return
	}
	if err := os.WriteFile(proxySwitchPath, []byte(reason), 0644); err != nil {
		log.Warnf("触发代理切换失败: %s", err)
		return
	}
	log.Warnf("已触发代理切换 (%s)", reason)
}

func proxyReady() bool {
	if !usingProxy() {
		return true
	}
	_, err := os.Stat("/tmp/proxy/ready")
	return err == nil
}

func waitForProxy() {
	if proxyReady() {
		return
	}
	log.Warn("代理未就绪，等待中...")
	for !proxyReady() {
		time.Sleep(2 * time.Second)
	}
	log.Info("代理已就绪，继续下载")
}

func (task *Task) tileFetcher(mt maptile.Tile, url string) {
	start := time.Now()
	defer task.tileWG.Done()
	defer func() {
		<-task.workers
	}()

	prep := func(t maptile.Tile, url string) string {
		if os.Getenv("HTTP_PROXY") != "" {
			url = strings.Replace(url, "https://", "http://", 1)
		}
		url = strings.Replace(url, "{x}", strconv.Itoa(int(t.X)), -1)
		url = strings.Replace(url, "{y}", strconv.Itoa(int(t.Y)), -1)
		maxY := int(math.Pow(2, float64(t.Z))) - 1
		url = strings.Replace(url, "{-y}", strconv.Itoa(maxY-int(t.Y)), -1)
		url = strings.Replace(url, "{z}", strconv.Itoa(int(t.Z)), -1)
		return url
	}
	tileURL := prep(mt, url)

	var body []byte
	var err error
	for attempt := 0; ; attempt++ {
		waitForProxy()
		body, err = fetchTile(tileURL)
		if !errors.Is(err, errProxy) || attempt >= maxProxyRetries {
			break
		}
		// 给边车时间收到切换通知、摘掉 ready，waitForProxy 再等新代理就绪
		time.Sleep(3 * time.Second)
	}
	if errors.Is(err, errNotFound) {
		log.Debugf("nil tile %v (404)", mt)
		if task.outformat != "mbtiles" {
			dir := filepath.Join(task.File, fmt.Sprintf(`%d`, mt.Z), fmt.Sprintf(`%d`, mt.X))
			os.MkdirAll(dir, os.ModePerm)
			emptyFile := filepath.Join(dir, fmt.Sprintf(`%d.%s`, mt.Y, task.TileMap.Format))
			os.WriteFile(emptyFile, []byte{}, os.ModePerm)
		}
		return
	}
	if err != nil {
		log.Errorf("tile(z:%d, x:%d, y:%d) 下载失败: %s", mt.Z, mt.X, mt.Y, err)
		return
	}
	if body == nil {
		log.Warnf("nil tile %v ~", mt)
		return
	}
	if err := validateTileData(task.TileMap.Format, body); err != nil {
		log.Errorf("tile(z:%d, x:%d, y:%d) 数据校验失败: %s", mt.Z, mt.X, mt.Y, err)
		return
	}

	td := Tile{T: mt, C: body}

	if task.TileMap.Format == PBF {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, err = zw.Write(body)
		if err != nil {
			log.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			log.Fatal(err)
		}
		td.C = buf.Bytes()
	}

	if task.outformat == "mbtiles" {
		task.savingpipe <- td
	} else {
		task.saveTile(td)
	}

	cost := time.Since(start).Milliseconds()
	log.Infof("tile(z:%d, x:%d, y:%d), %dms , %.2f kb, %s ...\n", mt.Z, mt.X, mt.Y, cost, float32(len(body))/1024.0, tileURL)
}

// DownloadZoom 下载指定层级
func (task *Task) downloadLayer(layer Layer) {
	bar := pb.New64(layer.Count).Prefix(fmt.Sprintf("Zoom %d : ", layer.Zoom)).Postfix("\n")
	// bar.SetRefreshRate(time.Second)
	bar.Start()
	// bar.SetMaxWidth(300)

	var tilelist = make(chan maptile.Tile, task.bufSize)

	go tilecover.CollectionChannel(layer.Collection, maptile.Zoom(layer.Zoom), tilelist)

	var existing *tileIndex
	if task.outformat != "mbtiles" {
		existing = newTileIndex(task.File, layer.Zoom, string(task.TileMap.Format))
	}
	for tile := range tilelist {
		// 文件模式下，已存在的 tile 直接跳过，不走 sleep 和 worker
		if existing != nil {
			if existing.has(tile.X, tile.Y) {
				log.Debugf("tile(z:%d, x:%d, y:%d) 已存在，跳过", tile.Z, tile.X, tile.Y)
				bar.Increment()
				task.Bar.Increment()
				continue
			}
		}
		// log.Infof(`fetching tile %v ~`, tile)
		select {
		case task.workers <- tile:
			//设置请求发送间隔时间
			time.Sleep(time.Duration(task.timeDelay) * time.Millisecond)
			bar.Increment()
			task.Bar.Increment()
			task.tileWG.Add(1)
			go task.tileFetcher(tile, layer.URL)
		case <-task.abort:
			log.Infof("Task %s got canceled.", task.ID)
			close(tilelist)
		case <-task.pause:
			log.Infof("Task %s suspended.", task.ID)
			select {
			case <-task.play:
				log.Infof("Task %s go on.", task.ID)
			case <-task.abort:
				log.Infof("Task %s got canceled.", task.ID)
				close(tilelist)
			}
		}
	}
	//等待该层结束
	task.tileWG.Wait()
	bar.FinishPrint(fmt.Sprintf("Task %s Zoom %d finished ~", task.ID, layer.Zoom))
}

// Download 开启下载任务
func (task *Task) Download() {
	//g orb.Geometry, minz int, maxz int
	task.Bar = pb.New64(task.Total).Prefix("Task : ").Postfix("\n")
	// task.Bar.SetRefreshRate(10 * time.Second)
	// task.Bar.Format("<.- >")
	task.Bar.Start()
	if task.outformat == "mbtiles" {
		task.SetupMBTileTables()
	} else {
		if task.File == "" {
			outdir := viper.GetString("output.directory")
			task.File = filepath.Join(outdir, task.Name)
		}
		os.MkdirAll(task.File, os.ModePerm)
		if err := task.writeFileMetadata(); err != nil {
			log.Errorf("写入文件瓦片元数据失败: %s", err)
			return
		}
	}
	go task.savePipe()
	for _, layer := range task.Layers {
		task.downloadLayer(layer)
	}
	task.Bar.FinishPrint(fmt.Sprintf("Task %s finished ~", task.ID))

	outdir := viper.GetString("output.directory")
	os.WriteFile(filepath.Join(outdir, ".done"), []byte("done"), 0644)
}
