package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tileIndex 记录某一层级在文件输出目录里已有哪些瓦片，用来跳过已下载的瓦片。
// 输出目录常在 NFS 上，逐张 os.Stat 每次都要问一趟服务端（实测约 25ms 一张），
// 二十多万张已有瓦片光检查就要一个多小时；改成每个 z/x 目录只读一次文件列表，按需加载并缓存。
// tilecover 按行产出瓦片，同一列会被反复访问，所以整层都缓存，层下完即丢弃。
type tileIndex struct {
	dir  string // <输出目录>/<z>
	ext  string // ".jpg" 等
	cols map[uint32]map[uint32]struct{}
}

func newTileIndex(root string, z int, format string) *tileIndex {
	return &tileIndex{
		dir:  filepath.Join(root, strconv.Itoa(z)),
		ext:  "." + format,
		cols: map[uint32]map[uint32]struct{}{},
	}
}

// has 报告 <z>/<x>/<y>.<format> 是否存在（404 留下的空文件也算，和原来的 os.Stat 判断一致）。
func (ix *tileIndex) has(x, y uint32) bool {
	col, ok := ix.cols[x]
	if !ok {
		col = ix.load(x)
		ix.cols[x] = col
	}
	_, ok = col[y]
	return ok
}

func (ix *tileIndex) load(x uint32) map[uint32]struct{} {
	col := map[uint32]struct{}{}
	entries, err := os.ReadDir(filepath.Join(ix.dir, strconv.FormatUint(uint64(x), 10)))
	if err != nil { // 目录不存在即这一列还没下过
		return col
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ix.ext)
		if !ok {
			continue
		}
		if y, err := strconv.ParseUint(name, 10, 32); err == nil {
			col[uint32(y)] = struct{}{}
		}
	}
	return col
}
