// Package novel 实现 WorldSim 的小说化联通（设计文档 §8/§9.10）：
//
//	编年史（FACT/SAID/STATE）+ 主角内心 → 小说章节 → 全书导出
//	核心原则：素材全部转化为叙事（对话化/场景化/心理化），限知视角不剧透。
package novel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/llm"
	"worldsim/internal/narrative"
	"worldsim/internal/sim"
	"worldsim/internal/worldbook"
)

// ---------- 章节计划 ----------

type ChapterPlan struct {
	SourceMode   string `json:"source_mode,omitempty"`
	SourceDigest string `json:"source_digest,omitempty"`
	Num          int    `json:"num"`
	Title        string `json:"title"`
	DayStart     int    `json:"day_start"`
	DayEnd       int    `json:"day_end"`
	Days         []int  `json:"days"`
	Status       string `json:"status"` // pending | done
	Path         string `json:"path,omitempty"`
}

// Writer 小说化写手（把模拟成果写成可阅读的小说）
type Writer struct {
	Independent      *IndependentSource // Explicit POV source capsule; bypasses all legacy global context.
	semanticReview   *SemanticReview
	IndependentCall  IndependentCompletion // Optional stateless transport for deterministic tests.
	NarrativeContext string                // Author plan, never character knowledge.
	APICfg           *config.APIConfig
	BookTitle        string
	BookDir          string // storys/{书名}/chapters/
	WB               *worldbook.Worldbook
	DaysPerCh        int           // 每章包含的模拟天数（默认3）
	ChapterLen       string        // 章节字数档位：short(1500) | normal(2500) | long(4000)
	Material         *MaterialBank // 描写素材库（真人大神示范，825条）
	HeroName         string        // 主角名（写手必须用模拟主角名，不得自造）
	// 跨章记忆（长文一致性：每章独立请求，靠注入"前情提要+伏笔"防遗忘/防断头）
	PrevSummary string // 前面所有章节的一句话摘要（累积，最近优先）
	Foreshadows string // 未回收伏笔清单（模拟层伏笔账本，写手可推进/回收）
	Decisions   string // 本章涉及剧情岔口与已定方向（用户改选优先，否则 AI 代决；写手必须照此方向写）
	WorldRefs   string // 用户上传的世界参考资料；属于硬设定，小说规划与正文都必须读取
}

// NewWriter 创建小说化写手
func NewWriter(apiCfg *config.APIConfig, bookTitle, bookDir string, wb *worldbook.Worldbook, heroName string) *Writer {
	os.MkdirAll(filepath.Join(bookDir, "chapters"), 0755)
	// 素材库：程序目录 material/（可放自定义素材）
	materialDir := filepath.Join(bookDir, "..", "..", "material")
	return &Writer{
		APICfg:     apiCfg,
		BookTitle:  bookTitle,
		BookDir:    bookDir,
		WB:         wb,
		DaysPerCh:  3,
		HeroName:   heroName,
		ChapterLen: "normal",
		Material:   LoadMaterialBank(materialDir),
	}
}

// SetHeroName 覆盖主角名（初始化时同步，写手必须用模拟主角名）
func (w *Writer) SetHeroName(name string) {
	if name != "" {
		w.HeroName = name
	}
}

// SetWorldRefs 同步当前世界的附件聚合文本。附件中的明确事实优先于通用写作建议。
func (w *Writer) SetWorldRefs(refs string) { w.WorldRefs = strings.TrimSpace(refs) }

// SetMaterialDir 覆盖素材库目录（供外部指定）
func (w *Writer) SetMaterialDir(dir string) { w.Material = LoadMaterialBank(dir) }

// ---------- 章节计划 ----------

// PlanChapters 事件驱动分章：优先由 LLM 按"事件序列"决策章节划分（哪几个事件合成一章），
// 失败/未配置 LLM 时回退到启发式规则（按戏剧日+天数）。时间不再是分章主角，事件才是。
func (w *Writer) PlanChapters(ctx context.Context, chronicle []sim.ChronicleEntry, thinkings map[int]string) []ChapterPlan {
	if len(chronicle) == 0 {
		return nil
	}
	// 有 LLM 配置则优先走 LLM 分章（按事件决策），失败/非法输出回退启发式
	if w.APICfg != nil {
		if plans := w.planChaptersLLM(ctx, chronicle, thinkings); len(plans) > 0 {
			return plans
		}
	}
	return w.planChaptersHeuristic(chronicle, thinkings)
}

// planChaptersHeuristic 启发式分章（回退方案）：每章至少包含 DaysPerCh 天且 ≥2 个"戏剧日"
func (w *Writer) planChaptersHeuristic(chronicle []sim.ChronicleEntry, thinkings map[int]string) []ChapterPlan {
	// 收集有记录的 day，排序
	daySet := map[int]bool{}
	for _, e := range chronicle {
		daySet[e.Day] = true
	}
	for d := range thinkings {
		daySet[d] = true
	}
	var days []int
	for d := range daySet {
		days = append(days, d)
	}
	sort.Ints(days)
	if len(days) == 0 {
		return nil
	}
	// 戏剧日判定：当天有真实事件（FACT·事件源）、对话（SAID）或抉择（thinkings）
	dramaDay := func(d int) bool {
		if thinkings[d] != "" {
			return true
		}
		for _, e := range chronicle {
			if e.Day == d && (e.Kind == "SAID" || (e.Kind == "FACT" && e.Source == "事件")) {
				return true
			}
		}
		return false
	}

	var plans []ChapterPlan
	num := 1
	var chunk []int
	chunkDrama := 0
	for _, d := range days {
		chunk = append(chunk, d)
		if dramaDay(d) {
			chunkDrama++
		}
		// 成章条件：≥2 个戏剧日且覆盖 ≥DaysPerCh 天；或天数超 2×DaysPerCh 强制成章（防超长）
		if (chunkDrama >= 2 && len(chunk) >= w.DaysPerCh) || len(chunk) >= w.DaysPerCh*2 {
			title := w.pickChapterTitle(chronicle, chunk)
			plans = append(plans, ChapterPlan{
				Num: num, Title: title,
				DayStart: chunk[0], DayEnd: chunk[len(chunk)-1],
				Days: chunk, Status: "pending",
			})
			num++
			chunk = nil
			chunkDrama = 0
		}
	}
	if len(chunk) > 0 {
		title := w.pickChapterTitle(chronicle, chunk)
		plans = append(plans, ChapterPlan{
			Num: num, Title: title,
			DayStart: chunk[0], DayEnd: chunk[len(chunk)-1],
			Days: chunk, Status: "pending",
		})
	}
	return plans
}

// chapterBreak JSON：LLM 输出的章节划分（每章一个连续天数区间，覆盖全部编年史）
type chapterBreak struct {
	Title    string `json:"title"`
	DayStart int    `json:"day_start"`
	DayEnd   int    `json:"day_end"`
}

// planChaptersLLM 由 LLM 按"事件序列"决策分章：给 LLM 看压缩后的逐日事件流，
// 让它把"一个完整情节单元"合成一章，输出每章的天数区间与标题。返回空切片表示失败。
func (w *Writer) planChaptersLLM(ctx context.Context, chronicle []sim.ChronicleEntry, thinkings map[int]string) []ChapterPlan {
	ctx = llm.WithSpan(ctx, "LLM分章")
	// 收集有记录的天，排序
	daySet := map[int]bool{}
	for _, e := range chronicle {
		daySet[e.Day] = true
	}
	for d := range thinkings {
		daySet[d] = true
	}
	var days []int
	for d := range daySet {
		days = append(days, d)
	}
	sort.Ints(days)
	if len(days) == 0 {
		return nil
	}

	// 把逐日事件压缩成"事件流"文本（平淡日标注，不占篇幅）
	var sb strings.Builder
	dayEvents := map[int][]string{}
	for _, e := range chronicle {
		t := strings.TrimSpace(e.Content)
		if t == "" {
			continue
		}
		// 只保留有叙事价值的事件/对话/观察，STATE 状态类跳过
		if e.Kind == "STATE" {
			continue
		}
		dayEvents[e.Day] = append(dayEvents[e.Day], t)
	}
	for _, d := range days {
		es := dayEvents[d]
		if th := strings.TrimSpace(thinkings[d]); th != "" {
			es = append(es, "【主角内心】"+th)
		}
		if len(es) == 0 {
			sb.WriteString(fmt.Sprintf("Day%d：（平淡日，时间流逝）\n", d))
			continue
		}
		sb.WriteString(fmt.Sprintf("Day%d：%s\n", d, strings.Join(es, "；")))
	}
	eventFlow := sb.String()

	system := `你是网文小说的"分章师"。你的任务不是写正文，而是拿到一段"逐日事件流"后，判断**哪些事件组成一个完整情节单元（一章）**，把事件流切成章节。
分章原则：
1. **按事件/情节分章，不是按天数分章**。一个完整的情节单元（起因→发展→冲突/转折→落点）应合成一章；新的章节应该从"新的事件、新的冲突、新的场景跳转、新的目标"开始。
2. 平淡的过渡日（时间流逝）并入前后最近的情节章当背景，不要单独成章。
3. 一章覆盖的天数区间必须连续，且所有章节的区间要**首尾相接、覆盖全部事件流**（不能漏天、不能重叠、不能留空）。
4. 每章 1~6 个事件日为宜，最晚不超过 10 天；事件密集时一章 1~2 天，事件稀疏时一章可跨多天。
5. 每章给一个 2~10 字的网文章节标题（要有悬念/冲突/画面感，禁止用"第几章""dayX"这类）。
6. 全篇尽量 5~15 章（按事件量自然浮动，事件少就少切，事件多就多切）。
输出严格 JSON，格式：
{"chapters":[{"title":"章节标题","day_start":N,"day_end":M},...]}`
	user := fmt.Sprintf("请把下面这段模拟事件流按情节单元分章。世界设定：\n%s\n\n事件流：\n%s\n\n请输出 chapters JSON。", w.worldContext(), eventFlow)

	raw, err := llm.CallAPITierSync(ctx, w.APICfg, "fast", system, user)
	if err != nil {
		return nil
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return nil
	}
	var out struct {
		Chapters []chapterBreak `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &out); err != nil || len(out.Chapters) == 0 {
		// 容错：偶尔 LLM 输出 {"title":..,"chapters":[...]} 多包一层
		var alt struct {
			Title    string         `json:"title"`
			Chapters []chapterBreak `json:"chapters"`
		}
		if json.Unmarshal([]byte(jsonStr), &alt) != nil || len(alt.Chapters) == 0 {
			return nil
		}
		out.Chapters = alt.Chapters
	}
	// 校验并规整：区间必须连续有序、覆盖全部天数；非法则回退启发式
	plans := w.normalizeChapterBreaks(chronicle, days, out.Chapters)
	return plans
}

// normalizeChapterBreaks 把 LLM 给出的章节天数区间规整为合法 ChapterPlan。
// 核心：把每个区间的起点当作"新章节断点"，只在有记录的天上切分——
// 保证章节首尾相接、覆盖全部天数、不引入无记录天；平淡日（无记录）自动并入相邻章。
func (w *Writer) normalizeChapterBreaks(chronicle []sim.ChronicleEntry, days []int, breaks []chapterBreak) []ChapterPlan {
	if len(breaks) == 0 {
		return nil
	}
	start, end := days[0], days[len(days)-1]
	// 按 DayStart 排序，逐段处理重叠：若某区间起点落在上一区间覆盖范围内（重叠），
	// 则并入上一章（不产生新断点），仅向后扩展覆盖范围。
	type seg struct {
		ds, de int
		title  string
	}
	var segs []seg
	for _, b := range breaks {
		ds := b.DayStart
		if ds < start {
			ds = start
		}
		if ds > end {
			ds = end
		}
		de := b.DayEnd
		if de < ds {
			de = ds
		}
		if de > end {
			de = end
		}
		segs = append(segs, seg{ds: ds, de: de, title: b.Title})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].ds < segs[j].ds })

	// 生成断点（dedupe 重叠），标题挂断点
	titleByStart := map[int]string{}
	var points []int
	lastEnd := -1
	for _, s := range segs {
		// 断点必须落在有记录的天上
		nd := nextRecordedDay(days, s.ds)
		if nd < 0 {
			continue
		}
		if lastEnd >= 0 && nd <= lastEnd {
			// 重叠：并入上一章，只扩展覆盖范围
			if s.de > lastEnd {
				lastEnd = s.de
			}
			continue
		}
		if _, ok := titleByStart[nd]; !ok {
			titleByStart[nd] = s.title
			points = append(points, nd)
		}
		if s.de > lastEnd {
			lastEnd = s.de
		}
	}
	// 确保首章起点是 days[0]（若 LLM 没从第一天开始）
	if _, ok := titleByStart[start]; !ok {
		titleByStart[start] = ""
		if len(points) == 0 || points[0] != start {
			points = append([]int{start}, points...)
		}
	}
	sort.Ints(points)

	// 按断点切分：每章 = [断点, 下一断点) 的有记录天
	var plans []ChapterPlan
	num := 1
	for i := 0; i < len(points); i++ {
		segLo := points[i]
		segHi := end + 1
		if i+1 < len(points) && points[i+1] > segLo {
			segHi = points[i+1]
		}
		chunk := daysInRange(days, segLo, segHi)
		if len(chunk) == 0 {
			continue
		}
		title := titleByStart[segLo]
		if strings.TrimSpace(title) == "" {
			title = w.pickChapterTitle(chronicle, chunk)
		}
		plans = append(plans, ChapterPlan{
			Num: num, Title: title,
			DayStart: chunk[0], DayEnd: chunk[len(chunk)-1],
			Days: chunk, Status: "pending",
		})
		num++
	}
	return plans
}

// nextRecordedDay 返回 days 中第一个 >= lo 的天；无则返回 -1
func nextRecordedDay(days []int, lo int) int {
	for _, d := range days {
		if d >= lo {
			return d
		}
	}
	return -1
}

// daysInRange 返回 days 中满足 lo <= d < hi 的天（保序）
func daysInRange(days []int, lo, hi int) []int {
	var out []int
	for _, d := range days {
		if d >= lo && d < hi {
			out = append(out, d)
		}
	}
	return out
}

// worldContext 世界设定摘要（供分章/叙事规划注入）
func (w *Writer) worldContext() string {
	var parts []string
	if w.WB != nil {
		if c := strings.TrimSpace(w.WB.ForNovelist()); c != "" {
			parts = append(parts, c)
		}
	}
	if refs := strings.TrimSpace(w.WorldRefs); refs != "" {
		parts = append(parts, "【用户提供的世界参考资料（硬约束；人物、规则、时间线不得擅自改写）】\n"+refs)
	}
	if len(parts) == 0 {
		return "（无世界设定）"
	}
	return strings.Join(parts, "\n\n")
}

// pickChapterTitle 从该章天数范围内的事件条目选标题
func (w *Writer) pickChapterTitle(chronicle []sim.ChronicleEntry, days []int) string {
	daySet := map[int]bool{}
	for _, d := range days {
		daySet[d] = true
	}
	// 从事件型 FACT 条目提取（Source=事件 且 content 像标题的）
	for _, e := range chronicle {
		if !daySet[e.Day] {
			continue
		}
		if e.Kind == "FACT" && e.Source == "事件" {
			t := strings.TrimSpace(e.Content)
			if t != "" && len([]rune(t)) <= 20 {
				return t
			}
		}
	}
	// 兜底：任意 FACT
	for _, e := range chronicle {
		if daySet[e.Day] && e.Kind == "FACT" {
			t := strings.TrimSpace(e.Content)
			if t != "" {
				r := []rune(t)
				if len(r) > 14 {
					return string(r[:14]) + "…"
				}
				return t
			}
		}
	}
	return fmt.Sprintf("第%d至%d天", days[0], days[len(days)-1])
}

// ---------- 叙事规划层（两段式管线第①段） ----------

// NarrativePlan 叙事规划：把编年史素材翻译成"怎么讲"的章节剧本
type NarrativePlan struct {
	OpeningHook   string `json:"opening_hook"`   // 开场钩子：用什么场景/悬念开场
	MiddleDevelop string `json:"middle_develop"` // 中段展开：哪些事件写足、哪些压缩
	Climax        string `json:"climax"`         // 本章高潮/爽点：最该写足的那个时刻
	ClosingHook   string `json:"closing_hook"`   // 收尾钩子：结尾留什么具体悬念
	POVNote       string `json:"pov_note"`       // 视角决策：主角知道什么/不知道什么
	PayoffType    string `json:"payoff_type"`    // 本章爽点类型（打脸/收获/装逼/情感/无）
	Pacing        string `json:"pacing"`         // 节奏：fast(快节奏推进) / slow(蓄力铺垫) / mixed(张弛交替)
	WordBudget    string `json:"word_budget"`    // 字数策略：哪些段写足哪些段省略
}

// planChapterNarrative 叙事规划层：拿编年史素材+世界书+前情，让 LLM 规划"这一章怎么讲"
// 这是两段式管线的第①段——编年史是"发生了什么"，叙事规划决定"怎么讲"
func (w *Writer) planChapterNarrative(ctx context.Context, p ChapterPlan, material string) string {
	ctx = llm.WithSpan(ctx, "叙事规划")
	if w.APICfg == nil {
		return ""
	}

	worldCtx := w.worldContext()

	system := `你是小说叙事规划师。你的任务不是写正文，而是把模拟世界的编年史整理成忠于事实的章节计划。
输出严格 JSON，格式：
{"opening_hook":"从哪一个已有动作或问题开场","middle_develop":"已有事件如何按因果展开，哪些写成场景，哪些概括","climax":"本章已有事实中变化最大的时刻；没有则写无","closing_hook":"停在哪个尚未解决的已有问题；没有则自然收束","pov_note":"主角已经知道、尚不知道和只能推测的内容","payoff_type":"无|发现|关系变化|决策结果","pacing":"fast|slow|mixed","word_budget":"按素材密度分配篇幅"}
规则：
1. 用户参考资料和世界书是硬约束，编年史是已经发生的事实，不得改写。
2. 不得新增人物、物品、规则、历史关系、危险或反转。
3. 视角严格限知；主角不知道的内容不能作为事实写入计划。
4. 开场和结尾都只能从已有素材中选择。素材不足时允许自然收束。
5. 只输出 JSON，不要其他文字。`

	user := fmt.Sprintf("章节信息：第%d章（模拟第%d~%d天）\n\n世界设定：\n%s\n\n", p.Num, p.DayStart, p.DayEnd, worldCtx)
	if w.PrevSummary != "" {
		user += "前情提要：\n" + w.PrevSummary + "\n\n"
	}
	if w.Foreshadows != "" {
		user += "未回收伏笔：\n" + w.Foreshadows + "\n\n"
	}
	user += w.NarrativeContext + "\n本章素材（需按类型区分事实/表达/观察/候选）：\n" + material + "\n\n请规划这一章怎么讲。"

	raw, err := llm.CallAPITierSync(ctx, w.APICfg, "fast", system, user)
	if err != nil {
		return ""
	}
	jsonStr := llm.ExtractJSON(raw)
	if jsonStr == "" {
		return ""
	}
	var plan NarrativePlan
	if err := json.Unmarshal([]byte(jsonStr), &plan); err != nil {
		return ""
	}
	// 格式化成写手可读的叙事大纲
	var sb strings.Builder
	sb.WriteString("\n【叙事规划（本章剧本大纲，写手必须照此结构写）】\n")
	sb.WriteString("开场钩子：" + plan.OpeningHook + "\n")
	sb.WriteString("中段展开：" + plan.MiddleDevelop + "\n")
	sb.WriteString("本章高潮：" + plan.Climax + "\n")
	sb.WriteString("收尾钩子：" + plan.ClosingHook + "\n")
	sb.WriteString("视角注意：" + plan.POVNote + "\n")
	sb.WriteString("爽点类型：" + plan.PayoffType + "\n")
	sb.WriteString("节奏定位：" + plan.Pacing + "\n")
	sb.WriteString("字数策略：" + plan.WordBudget + "\n")
	return sb.String()
}

// ---------- 章节写作 ----------

// WriteChapter 生成一章小说正文（LLM），保存到 chapters/NNN_标题.md，返回正文
func (w *Writer) WriteChapter(ctx context.Context, p ChapterPlan, chronicle []sim.ChronicleEntry, thinkings map[int]string, entities map[string]engine.Entity) (string, error) {
	if w.Independent != nil {
		return w.writeIndependentChapter(ctx, p, chronicle)
	}
	if w.APICfg == nil || w.HeroName == "" {
		return "", fmt.Errorf("写作配置或叙事视角缺失")
	}
	w.LoadSummariesBefore(p.Num)
	if err := w.CheckContinuityBefore(p.Num); err != nil {
		return "", err
	}
	authorPlan, err := narrative.Load(filepath.Dir(w.BookDir))
	if err != nil {
		return "", err
	}
	w.NarrativeContext = authorPlan.WriterContext(p.Num)
	ctx = llm.WithSpan(ctx, "小说写手")
	material := w.buildChapterMaterial(p, chronicle, nil) // reasoning is not a character's established knowledge
	// 两段式管线第①段：叙事规划层——先规划"这一章怎么讲"，再让写手按大纲写
	narrativePlan := w.planChapterNarrative(ctx, p, material)
	// 小说化用设定：A1世界观 + B4伏笔清单 + C文风（世界书静态，利于前缀缓存）
	worldCtx := w.worldContext()
	// 动态内容（角色现状随实体状态变化）单独放 user，保证 system 前缀字节级稳定 → 缓存命中
	dynamicCtx := w.characterIntro(entities)

	lengthRule := "2200~3200 字"
	switch w.ChapterLen {
	case "short":
		lengthRule = "1400~1800 字"
	case "long":
		lengthRule = "3500~4500 字"
	}

	// 小说写手用更大的 token 预算（长文），clone 配置避免影响其他调用
	cfg := w.APICfg.NovelConfig()

	system := `你是` + w.BookTitle + `的小说作者。把世界模拟留下的事实写成自然、清楚、可读的小说章节。

【设定与事实】
` + worldCtx + `

【写作规则】
1. 事实优先级：用户参考资料与世界书硬规则 > 编年史和角色状态 > 叙事规划 > 文学表达。低优先级内容不得改写高优先级事实。
2. 只能使用设定或模拟材料中已经存在的有名角色、地点、物品、规则和历史。不得为了制造悬念而增加第六个人、新钥匙、超自然现象、隐藏组织或旧识关系。
3. 采用第三人称限知，固定跟随主角` + w.HeroName + `。只写主角能够观察、听见、回忆或合理推断的内容；推断必须呈现为推断。
4. 编年史是事实记录。可以调整叙述顺序、补足动作和场景衔接，但不能改变事件结果、人物选择、规则条件和因果关系。材料不足时宁可写短，不得凭空增加事件凑字数。
5. 使用自然、准确的现代汉语。优先具体动作和清楚句意；比喻仅在准确且必要时使用。不要强制堆叠感官、金句、冷僻词、碎句或反常表达。
6. 对话数量由场景需要决定。每个人的表达应符合其已知性格、目标和信息范围，不用对话解释作者设定。
7. 章节需要有明确的场景目标、变化和结束点。悬念只能来自已有事实中尚未解决的问题。
8. 目标篇幅为` + lengthRule + `；事实密度不足时允许低于目标。
9. 第一行严格写“第N章·标题”，随后只写正文。末尾另起一行写“【本章摘要】”，用约100字记录本章事实、人物状态变化和未解决问题。不要输出分析、创作说明或 Markdown 标题。`

	// 动态内容全部放进 user 消息（system 保持静态 → DeepSeek 前缀缓存命中，降本）
	var ub strings.Builder
	ub.WriteString(w.NarrativeContext + "\n")
	if dynamicCtx != "" {
		ub.WriteString("【世界设定与角色现状（作为写作依据，沿用其身份与状态）】\n" + dynamicCtx + "\n\n")
	}
	if narrativePlan != "" {
		ub.WriteString(narrativePlan + "\n\n")
	}
	if w.PrevSummary != "" {
		ub.WriteString("\n【前情提要（前面章节已发生的事，保持连贯：别重复写、别写漏、人物关系与状态沿用）】\n" + w.PrevSummary + "\n")
	}
	if w.Foreshadows != "" {
		ub.WriteString("\n【未回收伏笔（前文埋下的钩子，本章自然推进或回收，别忘掉）】\n" + w.Foreshadows + "\n")
	}
	if w.Decisions != "" {
		ub.WriteString("\n【角色曾考虑的选择（仅是意向，结果必须由已提交事件证明，不得将事后改选写成既成事实）】\n" + w.Decisions + "\n")
	}
	ub.WriteString(strings.TrimSpace(material))
	ub.WriteString("\n\n【最后指令】现在直接写第" + fmt.Sprintf("%d", p.Num) + "章正文。第一行写'第" + fmt.Sprintf("%d", p.Num) + "章·标题'。正文最后完整结束一个场景，另起一行写【本章结束】，再写【本章摘要】和事实摘要。")
	user := ub.String()

	res, err := llm.CallAPITierSyncResult(ctx, cfg, "", system, user)
	if err != nil {
		return "", fmt.Errorf("章节生成失败: %w", err)
	}
	return w.publish(p, res, chronicle)
}

// splitSummary 把 AI 输出拆成（创作思路, 正文, 摘要）
// 输出格式：可选【创作思路】+【正文】+【本章摘要】；正文必须在【正文】之后
func splitSummary(text string) (notes, body, summary string) {
	bodyMark := "【正文】"
	sumMark := "【本章摘要】"
	sumIdx := strings.LastIndex(text, sumMark)
	if sumIdx > 0 {
		summary = strings.TrimSpace(text[sumIdx+len(sumMark):])
		summary = strings.TrimPrefix(summary, "本章完")
		summary = strings.TrimSpace(summary)
		text = text[:sumIdx]
	}
	bodyIdx := strings.Index(text, bodyMark)
	if bodyIdx >= 0 {
		notes = strings.TrimSpace(text[:bodyIdx])
		body = strings.TrimSpace(text[bodyIdx+len(bodyMark):])
		if body == "" {
			body = strings.TrimSpace(text) // 兜底
		}
		return notes, body, summary
	}
	// 没有【正文】标记：整段视为正文（去掉可能的思路开头）
	body = strings.TrimSpace(text)
	return "", body, summary
}

// updatePrevSummary 累积前情提要（每章一行，保留最近8章）
func (w *Writer) updatePrevSummary(num int, title, summary string) {
	sum := fmt.Sprintf("第%d章《%s》：%s", num, title, truncateRunes(summary, 120))
	if w.PrevSummary == "" {
		w.PrevSummary = sum
		return
	}
	w.PrevSummary = w.PrevSummary + "\n" + sum
	lines := strings.Split(w.PrevSummary, "\n")
	if len(lines) > 8 {
		w.PrevSummary = strings.Join(lines[len(lines)-8:], "\n")
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "……"
}

// buildChapterMaterial 剪辑层：按戏剧权重把本章编年史剪成"剧本包"（导演剪辑，不是素材堆）
//
//	【本章焦点】tags 统计主线（谁在追什么/什么伏笔该收）——写手先知道本章任务
//	【场景素材】weight≥0.55 或对话/主角内心：按天原样给（本章血肉，完整场景）
//	【背景素材】0.3≤weight<0.55（时间过渡/状态变化/涟漪）：压缩成"这段时间的变化"（章首时光流转）
//	【时间流逝】<0.3 或空天：一行带过（不喂进 prompt）
//
// token 控制：只喂高权重场景 + 背景压缩，砍掉 79% 的 STATE 水条目
func (w *Writer) buildChapterMaterial(p ChapterPlan, chronicle []sim.ChronicleEntry, thinkings map[int]string) string {
	daySet := map[int]bool{}
	for _, d := range p.Days {
		daySet[d] = true
	}
	dayEntries := map[int][]sim.ChronicleEntry{}
	tagCount := map[string]int{}
	for _, e := range chronicle {
		if daySet[e.Day] {
			if !w.EligibleMaterial(e) {
				continue
			}
			dayEntries[e.Day] = append(dayEntries[e.Day], e)
			for _, t := range e.Tags {
				tagCount[t]++
			}
		}
	}
	var days []int
	for d := range dayEntries {
		days = append(days, d)
	}
	sort.Ints(days)

	// ① 本章焦点：tags 统计主线（过滤流程标签，留角色/地点/伏笔）
	var keySb, bgSb strings.Builder
	keySb.WriteString("【本章焦点】\n")
	focus := topTags(tagCount, 3)
	if len(focus) > 0 {
		keySb.WriteString("主线：" + strings.Join(focus, "、") + "（本章围绕它们展开，别写散）\n")
	} else {
		keySb.WriteString("主线：本章以主角行动与事件推进为主（无突出标签，写实推进即可）\n")
	}
	keySb.WriteString("\n【场景素材】（必须写成完整场景：环境/对话/心理/动作）\n")
	bgSb.WriteString("【背景素材】（章首一两句交代这段时间的变化，不要展开成场景）\n")

	keyCount, bgCount := 0, 0
	for _, d := range days {
		entries := dayEntries[d]
		var dayKey, dayBg []string
		for _, e := range entries {
			line := "· [" + e.Kind + " / " + strings.Join(e.SourceEvents, ",") + "] " + e.Content
			// 场景：SAID 对话 / 高权重（事件/铺垫/伏笔/关系/揭示/里程碑）
			if e.Kind == "SAID" || e.Weight >= 0.55 {
				dayKey = append(dayKey, line)
			} else if e.Weight >= 0.3 {
				// 背景：时间过渡/状态变化/涟漪等，压缩
				dayBg = append(dayBg, line)
			}
			// weight<0.3：天气/张力微调等水条目，直接丢弃（不进 prompt，省 token）
		}
		// 主角内心（重点）
		if th, ok := thinkings[d]; ok && th != "" {
			dayKey = append(dayKey, "· "+w.HeroName+"内心："+th)
		}
		if len(dayKey) > 0 {
			keySb.WriteString(fmt.Sprintf("\nDay%d：\n%s\n", d, strings.Join(dayKey, "\n")))
			keyCount++
		}
		if len(dayBg) > 0 {
			bgSb.WriteString(fmt.Sprintf("Day%d：%s\n", d, strings.Join(dayBg, "；")))
			bgCount++
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("请写第 %d 章（对应模拟第 %d ~ %d 天）：\n\n", p.Num, p.DayStart, p.DayEnd))
	sb.WriteString("详略规则：\n")
	sb.WriteString("· 【场景素材】必须写成完整场景（这是本章血肉）\n")
	sb.WriteString("· 【背景素材】用一句过渡带过（如\"这段时间，日子照旧，但有些东西在变\"），绝不展开\n\n")
	if keyCount > 0 {
		sb.WriteString(keySb.String())
		sb.WriteString("\n")
	}
	if bgCount > 0 {
		sb.WriteString(bgSb.String())
		sb.WriteString("\n")
	}
	// 完全没场景？提示过渡章
	if keyCount == 0 {
		sb.WriteString("\n（本章无重大事件，请写成过渡章：篇幅可缩短，重点写时间流逝与主角状态变化）\n")
	}
	return sb.String()
}

// topTags 取出现次数最多的 N 个"主线标签"（过滤流程性标签，留角色/地点/伏笔/冲突）
func topTags(tagCount map[string]int, n int) []string {
	ban := map[string]bool{"系统": true, "导演": true, "段落": true, "事件": true, "对话": true, "状态": true, "观察": true, "反思": true, "铺垫": true, "时间过渡": true, "快进": true, "降级": true, "受阻": true, "涟漪": true, "世界揭示": true, "里程碑": true, "张力": true}
	type kv struct {
		k string
		v int
	}
	var arr []kv
	for k, v := range tagCount {
		if ban[k] {
			continue
		}
		arr = append(arr, kv{k, v})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
	var out []string
	for i := 0; i < len(arr) && i < n; i++ {
		out = append(out, arr[i].k)
	}
	return out
}

// characterIntro 生成角色简介（供写手参考）
func (w *Writer) characterIntro(entities map[string]engine.Entity) string {
	var sb strings.Builder
	if w.HeroName != "" {
		sb.WriteString(w.HeroName + "：主视角人物。")
	}
	if ent, ok := entities[w.HeroName]; ok && w.HeroName != "" {
		if j, ok := ent.Extra["profile"].(string); ok && j != "" {
			sb.Reset()
			sb.WriteString(w.HeroName + "：" + j + "。")
		}
	}
	// 其他角色：从实体取 job/profile（任何世界通用，不写死角色名）
	names := make([]string, 0, len(entities))
	for name := range entities {
		if strings.TrimSpace(name) != "" && name != w.HeroName {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		ent := entities[name]
		if name == w.HeroName {
			continue
		}
		j, _ := ent.Extra["job"].(string)
		if j == "" {
			j = ent.Job
		}
		if p, ok := ent.Extra["profile"].(string); ok && p != "" {
			sb.WriteString(fmt.Sprintf("%s：%s（%s）。", name, j, p))
		} else if j != "" {
			sb.WriteString(fmt.Sprintf("%s：%s。", name, j))
		}
	}
	if sb.Len() == 0 {
		return "（当前没有可用的角色状态；禁止自行创造有名角色）"
	}
	return sb.String()
}

// ---------- 全书导出 ----------
// ExportBook 把已生成章节拼成全书（txt + md 双格式），返回文件路径列表
func (w *Writer) ExportBook() ([]string, error) {
	chaptersDir := filepath.Join(w.BookDir, "chapters")
	files, err := os.ReadDir(chaptersDir)
	if err != nil {
		return nil, err
	}
	var chFiles []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") && !f.IsDir() {
			chFiles = append(chFiles, f.Name())
		}
	}
	sort.Strings(chFiles)
	if len(chFiles) == 0 {
		return nil, fmt.Errorf("还没有已生成的章节")
	}

	var body strings.Builder
	body.WriteString(w.BookTitle + "\n")
	body.WriteString(strings.Repeat("=", len([]rune(w.BookTitle))) + "\n\n")
	for _, f := range chFiles {
		data, err := os.ReadFile(filepath.Join(chaptersDir, f))
		if err != nil {
			continue
		}
		body.WriteString(strings.TrimSpace(string(data)))
		body.WriteString("\n\n---\n\n")
	}

	txtPath := filepath.Join(w.BookDir, w.BookTitle+".txt")
	mdPath := filepath.Join(w.BookDir, w.BookTitle+".md")
	if err := os.WriteFile(txtPath, []byte(body.String()), 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(mdPath, []byte(body.String()), 0644); err != nil {
		return nil, err
	}
	return []string{txtPath, mdPath}, nil
}

// ExportWebNovel 网文平台格式导出（起点/番茄可直接粘贴）：简介 + 分卷 + 章节 + 字数统计
func (w *Writer) ExportWebNovel() (string, error) {
	chaptersDir := filepath.Join(w.BookDir, "chapters")
	files, err := os.ReadDir(chaptersDir)
	if err != nil {
		return "", err
	}
	var chFiles []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") && !f.IsDir() {
			chFiles = append(chFiles, f.Name())
		}
	}
	sort.Strings(chFiles)
	if len(chFiles) == 0 {
		return "", fmt.Errorf("还没有已生成的章节")
	}

	var sb strings.Builder
	sb.WriteString("《" + w.BookTitle + "》\n\n")
	sb.WriteString("【简介】\n" + w.BookTitle + "：一个由世界模拟器自动生成的多Agent世界，主角在其中生活、遭遇、抉择，命运因每一次选择而改变。\n\n")
	sb.WriteString("【字数统计】\n")
	totalChars := 0
	var vols []string
	volIdx := 1
	for i, f := range chFiles {
		data, _ := os.ReadFile(filepath.Join(chaptersDir, f))
		chars := len([]rune(string(data)))
		totalChars += chars
		title := strings.TrimSuffix(f[4:], ".md")
		// 每5章一卷
		if i%5 == 0 {
			if i > 0 {
				vols = append(vols, fmt.Sprintf("第%d卷（共%d章）：第%d至%d章", volIdx, min(5, len(chFiles)-i+1), i, i+4))
			}
			volIdx++
		}
		_ = title
	}
	sb.WriteString(fmt.Sprintf("全书共 %d 章，约 %d 字。\n", len(chFiles), totalChars))
	sb.WriteString("分卷：\n" + strings.Join(vols, "\n") + "\n\n")
	sb.WriteString("========================================\n\n")

	// 正文（分卷排版）
	volNum := 1
	for i, f := range chFiles {
		if i%5 == 0 {
			sb.WriteString(fmt.Sprintf("—— 第%d卷 ——\n\n", volNum))
			volNum++
		}
		data, _ := os.ReadFile(filepath.Join(chaptersDir, f))
		sb.WriteString(strings.TrimSpace(string(data)))
		sb.WriteString("\n\n")
	}

	path := filepath.Join(w.BookDir, w.BookTitle+"_网文版.txt")
	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return "", err
	}
	return path, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sanitize 文件名安全化
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if out == "" {
		out = "untitled"
	}
	if len([]rune(out)) > 24 {
		r := []rune(out)
		out = string(r[:24])
	}
	return out
}
