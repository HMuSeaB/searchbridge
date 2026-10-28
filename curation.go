package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
//   - partial：有主词命中但覆盖率不足
//   - junk：完全无命中，或只命中二元组（权重 1 的弱信号，跨语句误配概率高）
//
// 标题命中与完整短语命中提升排序分；权威域名与时效信号做乘性加权。
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
	if matched == 0 || matchedHigh == 0 {
		// 只命中二元组（如「特性」出现在无关上下文）不足以构成相关。
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

	// 质量信号：权威域名加权 × 时效加权。
	score *= domainBoost(url)
	if hasRecencyIntent(phrase) {
		score *= recencyBoost(r)
	}

	var grade resultGrade
	switch {
	case coverage >= 0.5 || highCoverage >= 0.99:
		grade = gradeGood
	default:
		grade = gradePartial
	}
	return score, grade
}

// ---------- 质量信号：权威域名 ----------

// authoritativeDomains 是公认的第一方/开发文档/问答站点。对编程代理来说，
// 结果来源的可信度往往比关键词覆盖更能决定答案质量——官方文档和源码库
// 应当排在内容农场前面。这里只做「加权」不做「过滤」：漏杀一个真实域名
// 的代价远大于漏杀，且列表不可能穷尽。
var authoritativeDomains = map[string]bool{
	// 语言与包管理官方
	"go.dev": true, "pkg.go.dev": true, "docs.rs": true, "doc.rust-lang.org": true,
	"rust-lang.org": true, "crates.io": true, "python.org": true, "docs.python.org": true,
	"pypi.org": true, "nodejs.org": true, "npmjs.com": true, "docs.npmjs.com": true,
	"typescriptlang.org": true, "cppreference.com": true, "isocpp.org": true,
	"cplusplus.com": true, "rubyonrails.org": true, "guides.rubyonrails.org": true,
	"laravel.com": true, "spring.io": true, "kotlinlang.org": true, "swift.org": true,
	"oracle.com": true, "docs.oracle.com": true,
	// Web 与框架官方
	"developer.mozilla.org": true, "mdn.dev": true, "react.dev": true, "reactjs.org": true,
	"vuejs.org": true, "angular.io": true, "svelte.dev": true, "jquery.com": true,
	"webpack.js.org": true, "vitejs.dev": true, "babeljs.io": true,
	// 代码托管与问答
	"github.com": true, "raw.githubusercontent.com": true, "gist.github.com": true,
	"gitlab.com": true, "stackoverflow.com": true, "serverfault.com": true,
	"superuser.com": true, "stackexchange.com": true,
	// 数据库与基础设施官方
	"postgresql.org": true, "mysql.com": true, "docs.mysql.com": true, "mongodb.com": true,
	"redis.io": true, "sqlite.org": true, "elastic.co": true, "nginx.org": true,
	"kubernetes.io": true, "docs.docker.com": true, "docker.com": true,
	"apache.org": true, "kernel.org": true, "gnu.org": true, "wikipedia.org": true,
	// 云与模型厂商官方
	"cloud.google.com": true, "docs.aws.amazon.com": true, "aws.amazon.com": true,
	"azure.microsoft.com": true, "learn.microsoft.com": true, "docs.microsoft.com": true,
	"microsoft.com": true, "openai.com": true, "anthropic.com": true, "deepmind.google": true,
	"huggingface.co": true,
}

// domainBoost 按主机名后缀匹配权威域名列表，命中返回 1.25 的加权系数。
func domainBoost(lowerURL string) float64 {
	host := urlHost(lowerURL)
	if host == "" {
		return 1
	}
	for domain := range authoritativeDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return 1.25
		}
	}
	return 1
}

// urlHost 提取 URL 的小写主机名（尽力而为，失败返回空串）。
func urlHost(rawURL string) string {
	u := strings.ToLower(strings.TrimSpace(rawURL))
	if i := strings.Index(u, "://"); i != -1 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i != -1 {
		u = u[:i]
	}
	if i := strings.LastIndex(u, "@"); i != -1 { // 剥掉 userinfo@
		u = u[i+1:]
	}
	if i := strings.LastIndex(u, ":"); i != -1 { // 剥掉端口
		u = u[:i]
	}
	return strings.TrimPrefix(u, "www.")
}

// ---------- 质量信号：时效 ----------

// recencyKeywords 是查询里的时效意图信号。命中说明用户在问「最新」，
// 此时结果的新旧应当参与排序。
var recencyKeywords = []string{
	"最新", "近期", "最近", "今年", "当今", "现如今", "当前", "今天", "本周", "本月", "新版",
	"latest", "news", "recent", "current", "today", "this week", "this month", "new release",
}

func hasRecencyIntent(phrase string) bool {
	q := strings.ToLower(phrase)
	for _, kw := range recencyKeywords {
		if strings.Contains(q, kw) {
			return true
		}
	}
	return false
}

// recencyBoost 时效加权：结果文本提及当前年或去年 → ×1.2；只提及更早的
// 年份 → ×0.85（可能已过时）；无年份信息 → 不变。查询无时效意图时不调用。
func recencyBoost(r searchResult) float64 {
	text := strings.ToLower(r.Title + " " + r.Snippet)
	cur := time.Now().Year()
	for _, y := range []int{cur, cur - 1} {
		if strings.Contains(text, strconv.Itoa(y)) {
			return 1.2
		}
	}
	for y := 2015; y <= cur-2; y++ {
		if strings.Contains(text, strconv.Itoa(y)) {
			return 0.85
		}
	}
	return 1
}

// ---------- 近似重复标题 ----------

// titleTokens 把标题切成拉丁词 + CJK 二元组集合，用于近似去重。
func titleTokens(title string) map[string]bool {
	set := make(map[string]bool)
	lower := strings.ToLower(title)
	for _, w := range latinRunRe.FindAllString(lower, -1) {
		set[w] = true
	}
	for _, run := range cjkRunRe.FindAllString(lower, -1) {
		runes := []rune(run)
		for i := 0; i+2 <= len(runes); i++ {
			set[string(runes[i:i+2])] = true
		}
	}
	return set
}

// nearDupTitle 判断两个标题是否近似重复。用「包含度」而非 Jaccard：
// 跨引擎对同一页面常给出标题的详略变体（长的多一个「（完整版）」
// 「- 知乎」之类的后缀），Jaccard 会被多出的token稀释到阈值之下，
// 而较小集合被较大集合高比例覆盖正是这类变体的特征。
// 少于 3 个 token 的标题不足以判定，交给精确去重，避免误杀短标题。
func nearDupTitle(a, b string) bool {
	ta, tb := titleTokens(a), titleTokens(b)
	if len(ta) < 3 || len(tb) < 3 {
		return false
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	smaller := len(ta)
	if len(tb) < smaller {
		smaller = len(tb)
	}
	return float64(inter)/float64(smaller) >= 0.85
}

// nearDupAny 判断标题是否与已有标题列表中的任何一个近似重复。
func nearDupAny(title string, kept []string) bool {
	for _, k := range kept {
		if nearDupTitle(title, k) {
			return true
		}
	}
	return false
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
	var keptTitles []string // 已保留结果的标题，用于近似去重
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
		// 近似去重：跨引擎对同一页面常给出不同 URL 但标题几乎一致，
		// 与已保留的标题做 Jaccard 判定，重复的丢弃（保留先到的高分版本
		// 由后续排序保证——先到者通常是链路上更靠前的源）。
		if nearDupAny(r.Title, keptTitles) {
			stats.Deduped++
			continue
		}
		score, grade := scoreResult(r, tokens, phrase)
		if grade == gradeJunk {
			stats.Junk++
			continue
		}
		keptTitles = append(keptTitles, r.Title)
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
