package main

import (
	"regexp"
	"sort"
	"strings"
)

// 精选层：搜索引擎返回的结果里混有大量垃圾（SEO 农场、赌博站、不相关内容），
// 直接喂给模型会污染答案。这里在链路拿到结果后做四件事：
//  1. 垃圾特征过滤（赌博/色情等关键词模式）
//  2. 相关性评分与分级（查询词在标题/摘要/URL 的覆盖度）
//  3. 去重（规范化 URL + 规范化标题）
//  4. 排序（相关分优先，合并模式下多引擎共识加权）

type resultGrade int

const (
	gradeJunk    resultGrade = iota // 与查询完全无关或命中垃圾特征，丢弃
	gradePartial                    // 部分相关，仅在优质结果不足时兜底
	gradeGood                       // 相关，优先返回
)

func (g resultGrade) String() string {
	switch g {
	case gradeGood:
		return "good"
	case gradePartial:
		return "partial"
	default:
		return "junk"
	}
}

// qtoken 是查询分词后的一个匹配单元。
// 拉丁/数字整词与 CJK 整段权重高，CJK 二元组权重低（避免长句过度惩罚）。
type qtoken struct {
	text   string
	weight float64
}

// curatedResult 是带评分与溯源的结果。
type curatedResult struct {
	searchResult
	score     float64
	grade     resultGrade
	sources   []string
	consensus int
}

// curationStats 描述一次精选的统计，供 /v1/search 端点暴露质量信息。
type curationStats struct {
	Total   int `json:"total"`
	Good    int `json:"good"`
	Partial int `json:"partial"`
	Junk    int `json:"junk"`
	Deduped int `json:"deduped"`
}

// ---------- 分词 ----------

var (
	latinRunRe = regexp.MustCompile(`[a-z0-9][a-z0-9._+-]*`)
	cjkRunRe   = regexp.MustCompile(`[\x{2E80}-\x{9FFF}]+`)
)

// tokenizeQuery 把查询切分为匹配单元：完整短语（最高权重）、
// 拉丁/数字整词、CJK 整段、CJK 二元组。
func tokenizeQuery(query string) []qtoken {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}

	var tokens []qtoken
	seen := make(map[string]bool)
	add := func(text string, weight float64) {
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		tokens = append(tokens, qtoken{text: text, weight: weight})
	}

	// 完整短语（空格压缩后），命中则强相关信号。
	add(strings.Join(strings.Fields(q), " "), 3)

	for _, m := range latinRunRe.FindAllString(q, -1) {
		add(m, 2)
	}
	for _, m := range cjkRunRe.FindAllString(q, -1) {
		runes := []rune(m)
		switch {
		case len(runes) == 1:
			add(m, 1)
		case len(runes) == 2:
			add(m, 2)
		default:
			add(m, 2) // 整段
			for i := 0; i+2 <= len(runes); i++ {
				add(string(runes[i:i+2]), 1) // 二元组
			}
		}
	}
	return tokens
}

// ---------- 垃圾特征 ----------

// spamPatterns 是保守的垃圾内容特征（赌博/色情/诱导下载）。
// 宁可漏判也不要误伤正常内容，因此只用强特征词。
var spamPatterns = []string{
	// 赌博
	"赌博", "赌场", "博彩", "娱乐城", "澳门赌", "真人娱乐", "老虎机", "六合彩",
	"时时彩", "北京赛车", "极速赛车", "快三", "六閤綵",
	"casino", "gambling", "poker online", "betting", "jackpot",
	// 色情
	"色情", "情色", "成人影片", "porn", "xxx video",
	// 典型 SEO 农场话术
	"点击进入", "点击进入首页", "澳门线上", "注册送体验金", "首存优惠",
	"大奖", "信誉老品牌", "全网最", "独家发布",
}

var spamRe = regexp.MustCompile(buildSpamAlternation())

func buildSpamAlternation() string {
	parts := make([]string, 0, len(spamPatterns))
	for _, p := range spamPatterns {
		if p == "" {
			continue
		}
		parts = append(parts, regexp.QuoteMeta(strings.ToLower(p)))
	}
	if len(parts) == 0 {
		return `\A\z` // 永不匹配
	}
	return `(?i)(?:` + strings.Join(parts, "|") + `)`
}

// isSpam 判断结果是否命中垃圾特征（标题 + 摘要 + URL）。
func isSpam(r searchResult) bool {
	hay := strings.ToLower(r.Title + " " + r.Snippet + " " + r.URL)
	return spamRe.MatchString(hay)
}

// ---------- 评分与分级 ----------

// scoreResult 计算单个结果与查询的相关性。
// 分级规则：
//   - good：覆盖率 ≥ 0.5，或主词（权重 ≥ 2 的整词/整段/短语）全部命中
//   - partial：有任意命中
//   - junk：完全无命中
//
// 标题命中与完整短语命中提升排序分。
func scoreResult(r searchResult, tokens []qtoken, phrase string) (float64, resultGrade) {
	if len(tokens) == 0 {
		return 1, gradeGood
	}
	title := strings.ToLower(r.Title)
	snippet := strings.ToLower(r.Snippet)
	url := strings.ToLower(r.URL)

	var matched, total float64
	var matchedHigh, totalHigh float64
	var rank float64
	for _, t := range tokens {
		total += t.weight
		if t.weight >= 2 {
			totalHigh += t.weight
		}
		inTitle := strings.Contains(title, t.text)
		inSnippet := strings.Contains(snippet, t.text)
		inURL := strings.Contains(url, t.text)
		if !inTitle && !inSnippet && !inURL {
			continue
		}
		matched += t.weight
		if t.weight >= 2 {
			matchedHigh += t.weight
		}
		if inTitle {
			rank += t.weight * 3
		}
		if inSnippet {
			rank += t.weight
		}
		if inURL {
			rank += t.weight * 0.5
		}
	}
	if matched == 0 {
		return 0, gradeJunk
	}
	coverage := matched / total
	highCoverage := 1.0
	if totalHigh > 0 {
		highCoverage = matchedHigh / totalHigh
	}
	if phrase != "" && strings.Contains(title, phrase) {
		rank += 5 // 标题含完整查询短语，强信号
	}
	// 归一化到 0..1 附近，避免长查询把分数撑得过高。
	score := coverage + rank/(total*10)

	var grade resultGrade
	switch {
	case coverage >= 0.5 || highCoverage >= 0.99:
		grade = gradeGood
	default:
		grade = gradePartial
	}
	return score, grade
}

// ---------- 规范化与去重 ----------

// normalizeURL 去掉 scheme、www、query/fragment 和末尾斜杠（保留路径，
// 不同路径是不同页面），用于跨引擎去重。
func normalizeURL(raw string) string {
	u := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(u, "://"); i != -1 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "?#"); i != -1 {
		u = u[:i]
	}
	u = strings.TrimPrefix(u, "www.")
	return strings.TrimRight(u, "/")
}

var titleNoiseRe = regexp.MustCompile(`[\s\-_|,，。.、:：;；!！?？()（）\[\]【】"'']+`)

// normalizeTitle 压缩标题噪声，用于近似去重。
func normalizeTitle(t string) string {
	return strings.ToLower(titleNoiseRe.ReplaceAllString(t, ""))
}

// ---------- 精选主流程 ----------

// curate 对单个后端的结果做精选：垃圾过滤 → 评分分级 → 去重 → 排序。
// 返回的结果已按分数降序，good 在前、partial 兜底，总数不超过 limit。
func curate(results []searchResult, query string, limit int) ([]curatedResult, curationStats) {
	var stats curationStats
	stats.Total = len(results)

	tokens := tokenizeQuery(query)
	phrase := strings.Join(strings.Fields(strings.ToLower(query)), " ")

	seenURL := make(map[string]bool)
	seenTitle := make(map[string]bool)
	var goods, partials []curatedResult

	for _, r := range results {
		if isSpam(r) {
			stats.Junk++
			continue
		}
		if nu := normalizeURL(r.URL); nu != "" {
			if seenURL[nu] {
				stats.Deduped++
				continue
			}
			seenURL[nu] = true
		}
		if nt := normalizeTitle(r.Title); nt != "" {
			if seenTitle[nt] {
				stats.Deduped++
				continue
			}
			seenTitle[nt] = true
		}
		score, grade := scoreResult(r, tokens, phrase)
		if grade == gradeJunk {
			stats.Junk++
			continue
		}
		cr := curatedResult{searchResult: r, score: score, grade: grade}
		if grade == gradeGood {
			goods = append(goods, cr)
			stats.Good++
		} else {
			partials = append(partials, cr)
			stats.Partial++
		}
	}

	sort.SliceStable(goods, func(i, j int) bool { return goods[i].score > goods[j].score })
	sort.SliceStable(partials, func(i, j int) bool { return partials[i].score > partials[j].score })

	out := goods
	// 优质结果不足时用 partial 兜底填满 limit。
	if len(out) < limit {
		need := limit - len(out)
		if need > len(partials) {
			need = len(partials)
		}
		out = append(out, partials[:need]...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, stats
}

// hasUsableResults 判断精选结果是否值得采用（存在至少一条非垃圾结果）。
func hasUsableResults(curated []curatedResult) bool {
	return len(curated) > 0
}

// mergeCurated 合并多个后端的精选结果：同源去重 + 多引擎共识加权 + 重新排序。
// 被 k 个后端同时返回的结果获得 (1 + 0.5*(k-1)) 的分数加成——跨引擎共识是
// 抵御单一引擎垃圾/偏见的最有效信号。
func mergeCurated(perBackend [][]curatedResult, limit int) []curatedResult {
	type entry struct {
		result   curatedResult
		backends map[string]bool
	}
	byURL := make(map[string]*entry)
	var order []*entry

	for _, results := range perBackend {
		for _, cr := range results {
			key := normalizeURL(cr.URL)
			if key == "" {
				key = "title:" + normalizeTitle(cr.Title)
			}
			if e, ok := byURL[key]; ok {
				for _, src := range cr.sources {
					e.backends[src] = true
				}
				if cr.score > e.result.score {
					e.result.score = cr.score
					e.result.searchResult = cr.searchResult
				}
				if cr.grade > e.result.grade {
					e.result.grade = cr.grade
				}
				continue
			}
			e := &entry{
				result:   cr,
				backends: make(map[string]bool),
			}
			for _, src := range cr.sources {
				e.backends[src] = true
			}
			byURL[key] = e
			order = append(order, e)
		}
	}

	out := make([]curatedResult, 0, len(order))
	for _, e := range order {
		k := len(e.backends)
		if k > 1 {
			e.result.score *= 1 + 0.5*float64(k-1)
			e.result.consensus = k
		}
		out = append(out, e.result)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
