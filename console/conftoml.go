package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// TilerConf 是 tiler conf.toml 中控制台关心的部分。
type TilerConf struct {
	Output struct {
		Format    string `toml:"format"`
		Directory string `toml:"directory"`
	} `toml:"output"`
	TM struct {
		Name   string `toml:"name"`
		Min    int    `toml:"min"`
		Max    int    `toml:"max"`
		Format string `toml:"format"`
		Schema string `toml:"schema"`
		URL    string `toml:"url"`
	} `toml:"tm"`
	Lrs []Lrs `toml:"lrs"`
}

// Lrs 是一段 [[lrs]]：一个 geojson 区域在 min..max 层级下载。
type Lrs struct {
	Min     int    `toml:"min" json:"min"`
	Max     int    `toml:"max" json:"max"`
	Geojson string `toml:"geojson" json:"geojson"`
	URL     string `toml:"url" json:"url,omitempty"`
}

func parseConf(raw string) (*TilerConf, error) {
	var c TilerConf
	if err := toml.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("解析 conf.toml 失败: %w", err)
	}
	if c.TM.Name == "" {
		return nil, fmt.Errorf("conf.toml 缺少 tm.name")
	}
	return &c, nil
}

var (
	sectionHeader = regexp.MustCompile(`^\s*\[`)
	tmHeader      = regexp.MustCompile(`^\s*\[tm\]\s*(#.*)?$`)
	tmMinLine     = regexp.MustCompile(`^(\s*min\s*=\s*)(\d+)(.*)$`)
	tmMaxLine     = regexp.MustCompile(`^(\s*max\s*=\s*)(\d+)(.*)$`)
)

// appendLrs 在 conf.toml 末尾追加一段 [[lrs]]，必要时放宽 [tm] 的 min/max，
// 保留原文其余内容不动（注释、缩进、顺序）。
func appendLrs(raw string, l Lrs) (string, error) {
	cur, err := parseConf(raw)
	if err != nil {
		return "", err
	}
	if l.Min < 0 || l.Max < l.Min || l.Max > 24 {
		return "", fmt.Errorf("层级范围无效: %d-%d", l.Min, l.Max)
	}
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")

	newMin, newMax := cur.TM.Min, cur.TM.Max
	if l.Min < newMin {
		newMin = l.Min
	}
	if l.Max > newMax {
		newMax = l.Max
	}
	if newMin != cur.TM.Min || newMax != cur.TM.Max {
		start := -1
		for i, ln := range lines {
			if tmHeader.MatchString(ln) {
				start = i
				break
			}
		}
		if start < 0 {
			return "", fmt.Errorf("conf.toml 中找不到 [tm] 段")
		}
		seenMin, seenMax := false, false
		for i := start + 1; i < len(lines) && !sectionHeader.MatchString(lines[i]); i++ {
			if m := tmMinLine.FindStringSubmatch(lines[i]); m != nil && !seenMin {
				lines[i] = m[1] + strconv.Itoa(newMin) + m[3]
				seenMin = true
			} else if m := tmMaxLine.FindStringSubmatch(lines[i]); m != nil && !seenMax {
				lines[i] = m[1] + strconv.Itoa(newMax) + m[3]
				seenMax = true
			}
		}
		if (newMin != cur.TM.Min && !seenMin) || (newMax != cur.TM.Max && !seenMax) {
			return "", fmt.Errorf("[tm] 段中找不到 min/max 行，无法放宽层级")
		}
	}

	block := fmt.Sprintf("  [[lrs]]\n    min = %d\n    max = %d\n    geojson = %s", l.Min, l.Max, strconv.Quote(l.Geojson))
	if l.URL != "" {
		block += "\n    url = " + strconv.Quote(l.URL)
	}
	out := strings.Join(lines, "\n") + "\n" + block + "\n"

	// 回读校验：结果必须仍可解析，且正好多出这一段 lrs
	next, err := parseConf(out)
	if err != nil {
		return "", fmt.Errorf("追加后配置无法解析: %w", err)
	}
	if len(next.Lrs) != len(cur.Lrs)+1 || next.Lrs[len(next.Lrs)-1].Geojson != l.Geojson {
		return "", fmt.Errorf("追加后 lrs 数量不符，已放弃写入")
	}
	if next.TM.Min != newMin || next.TM.Max != newMax {
		return "", fmt.Errorf("追加后 tm 层级不符，已放弃写入")
	}
	return out, nil
}
