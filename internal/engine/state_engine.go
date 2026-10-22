// Package engine implements WorldSim's State Engine —
// the single source of truth for world state (design doc §1).
//
// 核心原则（v0.4 设计文档 §1.1/§1.2）：
//   - LLM 不能直接写状态，只能提交"状态变更提案"
//   - 所有提案带 command_id(幂等) / actor_id(来源) / base_revision(乐观锁)
//   - Validator 两层：硬约束(rules) 走程序，软规则(语义) 交给 GM 回调
//   - Reducer 原子提交并追加 event_log（事件溯源，可重放）
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- 世界状态（§3.2 schema） ----------

type WorldState struct {
	Revision   int               `json:"revision"`
	Day        int               `json:"day"`
	Time       string            `json:"time"`
	Weather    string            `json:"weather"`
	WorldLevel WorldLevel        `json:"world_level"`
	Entities   map[string]Entity `json:"entities"`
}
type WorldLevel struct {
	Protagonist     string              `json:"protagonist,omitempty"`
	NarrativePOV    string              `json:"narrative_pov,omitempty"`
	GlobalEvents    []string            `json:"global_events"`
	Factions        map[string]Faction  `json:"factions"`
	Locations       map[string]Location `json:"locations,omitempty"` // 地点系统（P0-2：状态会变）
	Tension         float64             `json:"tension"`
	TensionOverride *TensionOverride    `json:"tension_override,omitempty"`
	Background      map[string]string   `json:"background,omitempty"` // 背景人物池：名字→一句话身份（还不是NPC，可随剧情晋升为配角）
}

// Location 地点：有状态的场景对象（会随剧情变化）
type Location struct {
	Name     string `json:"name"`
	Type     string `json:"type"`             // 城区/建筑/交通/自然/秘境
	State    string `json:"state"`            // 正常/封禁/毁坏/繁荣/衰败/污染/新开放
	Note     string `json:"note"`             // 地点记忆（发生过的变化）
	Senses   string `json:"senses,omitempty"` // 感官档案：声音/气味/触感/光线（按本世界规则生成，写手写场景直接用）
	SinceDay int    `json:"since_day"`        // 登记日
}

type TensionOverride struct {
	Value    float64 `json:"value"`
	SetBy    string  `json:"set_by"`
	SetAt    string  `json:"set_at"`
	UntilDay int     `json:"until_day,omitempty"`
}

type Faction struct {
	Visibility    string   `json:"visibility"` // public | hidden
	Stance        string   `json:"stance"`
	Power         float64  `json:"power"`
	RecentActions []string `json:"recent_actions"`
}

type Entity struct {
	Location     string             `json:"location"`
	Health       float64            `json:"health"`
	Money        float64            `json:"money"`
	Job          string             `json:"job"`
	Alive        bool               `json:"alive"`
	Status       string             `json:"status"` // active | departed
	Relationship map[string]float64 `json:"relationship,omitempty"`
	Extra        map[string]any     `json:"extra,omitempty"`
	// Stats 世界书驱动的动态属性集：属性名/单位/数值由世界书的力量体系与资源体系决定，
	// 不属于引擎固定字段（引擎不硬编码任何具体属性名，只做通用读写）。
	Stats map[string]any `json:"stats,omitempty"`

	// Assets资产表：键与值均由世界书/主题包驱动（现金/灵石/物资/弹药…），
	//供世界→小说播种（bridge）等消费方读取，引擎不硬编码任何资产名。
	Assets map[string]any `json:"assets,omitempty"`
}

// ---------- 状态变更提案（§1.1） ----------

type Change struct {
	Path  string `json:"path"` // e.g. "entities.protagonist.money"
	Op    string `json:"op"`   // add | set | del
	Value any    `json:"value,omitempty"`
}

type Proposal struct {
	Occurrences  []Occurrence `json:"occurrences,omitempty"`
	CommandID    string       `json:"command_id"`
	ActorID      string       `json:"actor_id"`
	BaseRevision int          `json:"base_revision"`
	Type         string       `json:"type"` // state_change | adjudication
	Changes      []Change     `json:"changes"`
	Reason       string       `json:"reason,omitempty"`
	SourceEvents []string     `json:"source_events,omitempty"`
}

// ---------- 事件日志（事件溯源） ----------

type LogEntry struct {
	Occurrences  []Occurrence `json:"occurrences,omitempty"`
	Reason       string       `json:"reason,omitempty"`
	SourceEvents []string     `json:"source_events,omitempty"`
	CommandID    string       `json:"command_id"`
	ActorID      string       `json:"actor_id"`
	Revision     int          `json:"revision"`
	Timestamp    string       `json:"timestamp"`
	Day          int          `json:"day"`
	Changes      []Change     `json:"changes"`
}

// Occurrence records observable evidence; a claim made in speech is not a true fact.
type Occurrence struct {
	Kind     string   `json:"kind"` // observation | utterance
	Content  string   `json:"content"`
	Actor    string   `json:"actor"`
	Audience []string `json:"audience"`
	Location string   `json:"location"`
}

// ---------- 软规则回调：由世界 Agent（GM）实现 ----------

type SoftValidator func(ctx context.Context, p *Proposal, s *WorldState) error

// ---------- State Engine ----------

type StateEngine struct {
	mu            sync.Mutex
	state         *WorldState
	rules         Rules
	logPath       string
	softValidator SoftValidator
}

func NewStateEngine(rules Rules, logPath string) *StateEngine {
	return &StateEngine{
		state:   &WorldState{Revision: 0, Day: 0, Entities: map[string]Entity{}},
		rules:   rules,
		logPath: logPath,
	}
}

// SetSoftValidator 注入 GM 软规则校验（世界 Agent 的裁决回调）
func (e *StateEngine) SetSoftValidator(f SoftValidator) { e.softValidator = f }

func (e *StateEngine) State() *WorldState { return e.state }

// Snapshot returns a detached state under the engine lock, for derived consumers
// such as the character Agent registry. They must never modify the live state.
func (e *StateEngine) Snapshot() (*WorldState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := json.Marshal(e.state)
	if err != nil {
		return nil, err
	}
	var snapshot WorldState
	if err := json.Unmarshal(b, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// Submit 是全系统唯一写路径（§1.1）：
//
//	硬约束校验(程序) → 软规则校验(GM) → 原子提交 → event_log 追加
//
// GM 裁决提案（Type == "adjudication"）仅过硬约束，软规则不回环（§1.2）。
func (e *StateEngine) Submit(ctx context.Context, p *Proposal) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 幂等：command_id 已存在则跳过
	if e.logHasCommand(p.CommandID) {
		return nil
	}

	// 乐观锁：base_revision 不匹配 → 冲突
	if p.BaseRevision != e.state.Revision {
		return &RevisionConflictError{Expected: e.state.Revision, Got: p.BaseRevision}
	}

	for _, c := range p.Changes {
		if c.Op != "set" && c.Op != "add" && c.Op != "del" {
			return fmt.Errorf("unsupported operation: %s", c.Op)
		}
		parts := strings.Split(c.Path, ".")
		if len(parts) >= 2 && parts[0] == "entities" && strings.TrimSpace(parts[1]) == "" {
			return fmt.Errorf("实体 ID 不得为空: %s", c.Path)
		}
	}
	if len(SanitizeChanges(p.Changes)) != len(p.Changes) {
		return fmt.Errorf("提案含非法路径")
	}

	// 1. 硬约束（程序）
	for _, o := range p.Occurrences {
		if o.Kind != "observation" && o.Kind != "utterance" {
			return fmt.Errorf("invalid occurrence kind")
		}
		if strings.TrimSpace(o.Content) == "" || len(o.Audience) == 0 {
			return fmt.Errorf("occurrence requires content and recipients")
		}
		if _, ok := e.state.Entities[o.Actor]; !ok || o.Actor == "" {
			return fmt.Errorf("unknown occurrence actor")
		}
		for _, name := range o.Audience {
			ent, ok := e.state.Entities[name]
			if !ok || name == "" || ent.Location != o.Location || o.Location == "" {
				return fmt.Errorf("occurrence recipient is not at location: %s", name)
			}
		}
	}
	if err := e.applyHard(p); err != nil {
		return fmt.Errorf("硬约束校验失败: %w", err)
	}

	// 2. 软规则（GM）—— 裁决提案不回环
	if p.Type != "adjudication" && e.softValidator != nil {
		if err := e.softValidator(ctx, p, e.state); err != nil {
			return fmt.Errorf("软规则裁决失败: %w", err)
		}
	}

	// Validate and reduce on a deep copy; publish only after the journal write succeeds.
	b, err := json.Marshal(e.state)
	if err != nil {
		return err
	}
	var next WorldState
	if err := json.Unmarshal(b, &next); err != nil {
		return err
	}
	for _, c := range p.Changes {
		validator := &StateEngine{state: &next, rules: e.rules}
		if err := validator.checkNumericAndEnum(c); err != nil {
			return err
		}
		if err := ApplyPath(&next, c); err != nil {
			return fmt.Errorf("应用变更 %s 失败: %w", c.Path, err)
		}
	}
	next.Revision++

	// 4. 事件日志
	if err := e.appendLog(LogEntry{
		Occurrences:  p.Occurrences,
		Reason:       p.Reason,
		SourceEvents: p.SourceEvents,
		CommandID:    p.CommandID,
		ActorID:      p.ActorID,
		Revision:     next.Revision,
		Timestamp:    time.Now().Format(time.RFC3339),
		Day:          next.Day,
		Changes:      p.Changes,
	}); err != nil {
		return fmt.Errorf("写入事件日志失败: %w", err)
	}
	*e.state = next
	return nil
}

// ---------- 硬约束校验 ----------

func (e *StateEngine) applyHard(p *Proposal) error {
	// 权限规则：actor 不能改 deny_paths 下的字段（支持 * 通配）
	for _, perm := range e.rules.Permissions {
		if !matchGlob(perm.Actor, p.ActorID) {
			continue
		}
		for _, c := range p.Changes {
			for _, deny := range perm.DenyPaths {
				if matchGlob(deny, c.Path) {
					return fmt.Errorf("权限拒绝：%s 无权修改 %s", p.ActorID, c.Path)
				}
			}
		}
	}
	// 数值规则 + 枚举合法性
	for _, c := range p.Changes {
		if err := e.checkNumericAndEnum(c); err != nil {
			return err
		}
	}
	// 前置条件（MVP：会话/购买等动作级校验，由 GM 或调度器负责场景层判断）
	return nil
}

func (e *StateEngine) checkNumericAndEnum(c Change) error {
	// 枚举白名单
	if allowed, ok := e.rules.Enums[c.Path]; ok {
		if c.Op == "set" {
			v, _ := c.Value.(string)
			for _, a := range allowed {
				if a == v {
					return nil
				}
			}
			return fmt.Errorf("枚举非法：%s 不在白名单 %v", v, allowed)
		}
	}
	// 数值范围（支持 * 通配路径）
	for _, nr := range e.rules.NumericRules {
		if !matchGlob(nr.Path, c.Path) {
			continue
		}
		if c.Op != "add" && c.Op != "set" {
			continue
		}
		var newVal float64
		cur, ok := getFloat(e.state, c.Path)
		switch c.Op {
		case "set":
			v, ok2 := toFloat(c.Value)
			if !ok2 {
				continue
			}
			newVal = v
		case "add":
			v, ok2 := toFloat(c.Value)
			if !ok2 || !ok {
				continue
			}
			newVal = cur + v
		}
		if nr.Min != nil && newVal < *nr.Min {
			return fmt.Errorf("数值越界：%s=%v < min %v", c.Path, newVal, *nr.Min)
		}
		if nr.Max != nil && newVal > *nr.Max {
			return fmt.Errorf("数值越界：%s=%v > max %v", c.Path, newVal, *nr.Max)
		}
	}
	return nil
}

// ---------- 事件日志 ----------

func (e *StateEngine) appendLog(entry LogEntry) error {
	if e.logPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.logPath), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(e.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (e *StateEngine) logHasCommand(cmdID string) bool {
	if e.logPath == "" {
		return false
	}
	data, err := os.ReadFile(e.logPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry LogEntry
		if json.Unmarshal([]byte(line), &entry) == nil && entry.CommandID == cmdID {
			return true
		}
	}
	return false
}

// ---------- 持久化 ----------

func (e *StateEngine) Save(path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := json.MarshalIndent(e.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

func (e *StateEngine) Load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, e.state)
}

// Replay 事件溯源重放：从 event_log 重放到目标 revision（§17.1）
// 注意：重放=状态恢复，不=故事重现（LLM 输出不可重现，设计文档 §17.1 明确）
func Replay(logPath string, targetRevision int, init *WorldState) (*WorldState, error) {
	if init == nil {
		init = &WorldState{Revision: 0, Day: 0, Entities: map[string]Entity{}}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(init)
	if err != nil {
		return nil, err
	}
	state := &WorldState{}
	if err := json.Unmarshal(b, state); err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry LogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("事件日志损坏: %w", err)
		}
		if entry.Revision <= state.Revision {
			continue
		}
		if entry.Revision > targetRevision {
			break
		}
		if entry.Revision != state.Revision+1 {
			return nil, fmt.Errorf("事件 revision 不连续: %d", entry.Revision)
		}
		for _, c := range entry.Changes {
			if err := ApplyPath(state, c); err != nil {
				return nil, err
			}
		}
		state.Revision = entry.Revision
		state.Day = entry.Day
	}
	return state, nil
}

// SanitizeChanges 过滤非法路径变更（LLM 输出容错）：
//   - entities.{名字} 缺字段（至少3段）→ 忽略
//   - 未知顶层路径 → 忽略
//   - world_level 路径保留（允许2段，如 world_level.tension）
func SanitizeChanges(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		parts := strings.Split(c.Path, ".")
		switch {
		case len(parts) < 2:
			continue
		case parts[0] == "entities" && len(parts) < 3:
			continue
		case parts[0] != "entities" && parts[0] != "world_level":
			continue
		}
		out = append(out, c)
	}
	return out
}

// ---------- 路径应用 ----------

// ApplyPath 把单条变更应用到状态（支持 entities.* 与 world_level.* 路径）
func ApplyPath(s *WorldState, c Change) error {
	parts := strings.Split(c.Path, ".")
	if len(parts) < 2 {
		return fmt.Errorf("路径过短: %s", c.Path)
	}
	switch parts[0] {
	case "entities":
		if len(parts) < 3 {
			return fmt.Errorf("entities 路径需至少3段: %s", c.Path)
		}
		name := parts[1]
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("实体 ID 为空")
		}
		ent, ok := s.Entities[name]
		if !ok {
			ent = Entity{Alive: true, Status: "active", Health: 100, Relationship: map[string]float64{}, Stats: map[string]any{}}
		}
		field := parts[2]
		var err error
		switch c.Op {
		case "del":
			if field == "" {
				delete(s.Entities, name)
				return nil
			}
			err = setEntityField(&ent, field, nil, c.Op, parts[3:])
		case "add":
			err = addEntityField(&ent, field, c.Value, parts[3:])
		default:
			err = setEntityField(&ent, field, c.Value, c.Op, parts[3:])
		}
		if err != nil {
			return err
		}
		s.Entities[name] = ent
	case "world_level":
		return applyWorldLevel(s, parts[1:], c)
	default:
		return fmt.Errorf("未知顶层路径: %s", parts[0])
	}
	return nil
}

func setEntityField(e *Entity, field string, v any, op string, rest []string) error {
	switch field {
	case "location":
		value, ok := v.(string)
		if op != "set" || !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("location requires nonempty set text")
		}
		e.Location = value
	case "health", "money":
		f, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("数值字段 %s 需数值", field)
		}
		if field == "health" {
			e.Health = f
		} else {
			e.Money = f
		}
	case "job":
		value, ok := v.(string)
		if op != "set" || !ok {
			return fmt.Errorf("job requires set text")
		}
		e.Job = value
	case "alive":
		value, ok := v.(bool)
		if op != "set" || !ok {
			return fmt.Errorf("alive requires set boolean")
		}
		e.Alive = value
	case "status":
		value, ok := v.(string)
		if op != "set" || !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("status requires nonempty set text")
		}
		e.Status = value
	case "relationship":
		if len(rest) != 1 || rest[0] == "" {
			return fmt.Errorf("relationship target required")
		}
		if e.Relationship == nil {
			e.Relationship = map[string]float64{}
		}
		if op == "del" {
			delete(e.Relationship, rest[0])
		} else {
			f, ok := toFloat(v)
			if !ok {
				return fmt.Errorf("关系值需数值")
			}
			e.Relationship[rest[0]] = f
		}
	case "extra":
		if len(rest) != 1 || rest[0] == "" {
			return fmt.Errorf("extra key required")
		}
		if e.Extra == nil {
			e.Extra = map[string]any{}
		}
		if op == "del" {
			delete(e.Extra, rest[0])
		} else {
			e.Extra[rest[0]] = v
		}
	case "stats":
		if len(rest) != 1 || rest[0] == "" {
			return fmt.Errorf("stat key required")
		}
		// 世界书驱动的动态属性集：属性名由世界书决定，引擎只做通用读写
		if e.Stats == nil {
			e.Stats = map[string]any{}
		}
		if op == "del" {
			delete(e.Stats, rest[0])
		} else {
			e.Stats[rest[0]] = v
		}
	case "assets":
		if len(rest) != 1 || rest[0] == "" {
			return fmt.Errorf("asset key required")
		}
		if e.Assets == nil {
			e.Assets = map[string]any{}
		}
		if op == "del" {
			delete(e.Assets, rest[0])
		} else {
			e.Assets[rest[0]] = v
		}
	default:
		return fmt.Errorf("unknown entity field: %s", field)
	}
	return nil
}

func addEntityField(e *Entity, field string, v any, rest []string) error {
	switch field {
	case "money":
		f, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("money 需数值")
		}
		e.Money += f
	case "health":
		f, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("health 需数值")
		}
		e.Health += f
	case "relationship":
		if len(rest) != 1 || rest[0] == "" {
			return fmt.Errorf("relationship target required")
		}
		if e.Relationship == nil {
			e.Relationship = map[string]float64{}
		}
		if len(rest) > 0 {
			f, ok := toFloat(v)
			if !ok {
				return fmt.Errorf("关系值需数值")
			}
			e.Relationship[rest[0]] += f
		}
	case "stats":
		if len(rest) != 1 || rest[0] == "" {
			return fmt.Errorf("stat key required")
		}
		// 动态属性集：支持数值加减与整体设置
		if e.Stats == nil {
			e.Stats = map[string]any{}
		}
		if len(rest) > 0 {
			key := rest[0]
			cur, ok := toFloat(e.Stats[key])
			if !ok {
				cur = 0
			}
			f, ok := toFloat(v)
			if !ok {
				return fmt.Errorf("stats.%s 需数值", key)
			}
			e.Stats[key] = cur + f
		}
	case "assets":
		if len(rest) != 1 || rest[0] == "" {
			return fmt.Errorf("asset key required")
		}
		if e.Assets == nil {
			e.Assets = map[string]any{}
		}
		f, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("asset delta must be numeric")
		}
		cur := float64(0)
		if value, exists := e.Assets[rest[0]]; exists {
			var ok bool
			cur, ok = toFloat(value)
			if !ok {
				return fmt.Errorf("asset is not numeric")
			}
		}
		e.Assets[rest[0]] = cur + f
	default:
		return fmt.Errorf("unsupported additive field: %s", field)
	}
	return nil
}

func applyWorldLevel(s *WorldState, rest []string, c Change) error {
	if len(rest) == 0 {
		return fmt.Errorf("world_level 路径过短")
	}
	switch rest[0] {
	case "day":
		day, ok := toFloat(c.Value)
		if !ok || c.Op != "set" || day < float64(s.Day) || day != float64(int(day)) {
			return fmt.Errorf("invalid world day")
		}
		s.Day = int(day)
	case "protagonist", "narrative_pov":
		name, ok := c.Value.(string)
		if !ok || c.Op != "set" || strings.TrimSpace(name) == "" {
			return fmt.Errorf("身份字段需非空角色名")
		}
		if _, ok := s.Entities[name]; !ok {
			return fmt.Errorf("身份引用角色不存在: %s", name)
		}
		if rest[0] == "protagonist" {
			s.WorldLevel.Protagonist = name
		} else {
			s.WorldLevel.NarrativePOV = name
		}
	case "tension":
		f, ok := toFloat(c.Value)
		if !ok {
			return fmt.Errorf("tension 需数值")
		}
		s.WorldLevel.Tension = f
	case "weather":
		v, ok := c.Value.(string)
		if c.Op != "set" || !ok || strings.TrimSpace(v) == "" {
			return fmt.Errorf("weather requires a nonempty string")
		}
		s.Weather = v
	case "global_events":
		ev, ok := c.Value.(string)
		if c.Op != "add" || !ok || strings.TrimSpace(ev) == "" {
			return fmt.Errorf("global event requires additive text")
		}
		s.WorldLevel.GlobalEvents = append(s.WorldLevel.GlobalEvents, ev)
	case "background":
		// world_level.background.{名}=一句话身份（背景人物池）；del 移除
		if len(rest) < 2 {
			return fmt.Errorf("background 路径需至少2段")
		}
		if s.WorldLevel.Background == nil {
			s.WorldLevel.Background = map[string]string{}
		}
		if c.Op == "del" {
			delete(s.WorldLevel.Background, rest[1])
		} else if v, ok := c.Value.(string); ok && c.Op == "set" {
			s.WorldLevel.Background[rest[1]] = v
		} else {
			return fmt.Errorf("background value requires set text")
		}
	case "locations":
		// world_level.locations.{名}.{state|note|type}
		if len(rest) < 2 {
			return fmt.Errorf("locations 路径需至少2段")
		}
		name := rest[1]
		if s.WorldLevel.Locations == nil {
			s.WorldLevel.Locations = map[string]Location{}
		}
		loc, ok := s.WorldLevel.Locations[name]
		if !ok {
			loc = Location{Name: name, State: "正常"}
		}
		if len(rest) != 3 {
			return fmt.Errorf("location path requires field")
		}
		if len(rest) >= 3 {
			switch rest[2] {
			case "state":
				loc.State, _ = c.Value.(string)
			case "note":
				loc.Note, _ = c.Value.(string)
			case "type":
				loc.Type, _ = c.Value.(string)
			case "senses":
				loc.Senses, _ = c.Value.(string)
			default:
				return fmt.Errorf("unknown location field: %s", rest[2])
			}
		}
		if loc.SinceDay == 0 {
			loc.SinceDay = s.Day
		}
		s.WorldLevel.Locations[name] = loc
	case "factions":
		if len(rest) < 2 {
			return fmt.Errorf("factions 路径需至少2段")
		}
		name := rest[1]
		fac, ok := s.WorldLevel.Factions[name]
		if !ok {
			fac = Faction{}
		}
		if len(rest) != 3 {
			return fmt.Errorf("faction path requires field")
		}
		if len(rest) >= 3 {
			switch rest[2] {
			case "stance":
				fac.Stance, _ = c.Value.(string)
			case "power":
				f, _ := toFloat(c.Value)
				fac.Power = f
			case "visibility":
				fac.Visibility, _ = c.Value.(string)
			case "recent_actions":
				if c.Op == "add" {
					ev, _ := c.Value.(string)
					fac.RecentActions = append(fac.RecentActions, ev)
				}
			default:
				return fmt.Errorf("unknown faction field: %s", rest[2])
			}
		}
		if s.WorldLevel.Factions == nil {
			s.WorldLevel.Factions = map[string]Faction{}
		}
		s.WorldLevel.Factions[name] = fac
	case "tension_override":
		if c.Op == "set" {
			if to, ok := c.Value.(map[string]any); ok {
				o := &TensionOverride{}
				if v, ok := to["value"].(float64); ok {
					o.Value = v
				}
				if v, ok := to["set_by"].(string); ok {
					o.SetBy = v
				}
				if v, ok := to["set_at"].(string); ok {
					o.SetAt = v
				}
				s.WorldLevel.TensionOverride = o
			} else {
				return fmt.Errorf("tension override requires object")
			}
		} else {
			return fmt.Errorf("tension override requires set")
		}
	default:
		return fmt.Errorf("unknown world field: %s", rest[0])
	}
	return nil
}

// ---------- 工具函数 ----------

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func getFloat(s *WorldState, path string) (float64, bool) {
	parts := strings.Split(path, ".")
	if len(parts) >= 3 && parts[0] == "entities" {
		if ent, ok := s.Entities[parts[1]]; ok {
			switch parts[2] {
			case "health":
				return ent.Health, true
			case "money":
				return ent.Money, true
			case "stats":
				if len(parts) >= 4 {
					return toFloat(ent.Stats[parts[3]])
				}
			}
		}
	}
	return 0, false
}

// matchGlob 支持 * 通配（如 "npc_*"、"entities.*.health"）
func matchGlob(pattern, s string) bool {
	if pattern == "*" || pattern == s {
		return true
	}
	// 简化通配：按 * 分段前缀匹配
	seg := strings.Split(pattern, "*")
	if len(seg) == 1 {
		return pattern == s
	}
	pos := 0
	for i, part := range seg {
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(s, part) {
				return false
			}
			pos = len(part)
			continue
		}
		idx := strings.Index(s[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	return true
}

// RevisionConflictError 乐观锁冲突
type RevisionConflictError struct {
	Expected int
	Got      int
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("revision 冲突：期望 %d，实际 %d（需重读后重提）", e.Expected, e.Got)
}
