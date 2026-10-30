package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Child Safety (migration 0067). It replaces "Family safety" plus the
// auto/family router template with one set of rules that attach to a
// PERSON (a user, or the members of a team) and apply to every model that
// person calls, personal plans included. Design:
// ~/audits/family-profile/design.md.
//
// One classifier call per turn decides which categories a child's message
// falls under. Then each category gives an outcome from the child's
// profile (an age range):
//
//   - off: nothing happens.
//   - answer ("answer appropriately"): the message goes to the model with
//     the category's instructions added to the system prompt. For the two
//     homework categories the tutor rule is added to EVERY turn (a
//     follow-up such as "is it 12?" does not read as homework on its own),
//     and the reply is checked for a final answer, which is held back.
//   - block: a kind refusal. For "feeling low" a block is Janus's own
//     support message (the message is not forwarded) and the parent is
//     notified.
//
// The critical safety categories (sexual content, weapons, self-harm...)
// always block, and self-harm always gets the support message.

// Child Safety category outcomes.
const (
	ChildOutcomeOff    = "off"
	ChildOutcomeAnswer = "answer"
	ChildOutcomeBlock  = "block"
)

// Built-in profile ids (age ranges).
const (
	ChildProfileLittle = "cs_little"
	ChildProfileKids   = "cs_kids"
	ChildProfileTeens  = "cs_teens"
)

// ChildCategoryHomeworkLearning and ChildCategoryFinalAnswer are Child
// Safety's own categories (not rows in secgw_category): help with
// schoolwork, and, on the reply side, an answer given away.
const (
	ChildCategoryHomeworkLearning = "homework_learning"
	ChildCategoryHomeworkShortcut = "homework_shortcut"
	ChildCategoryFeelingLow       = "feeling_low"
	ChildCategoryFinalAnswer      = "final_answer"
)

// ChildSafetyCategorySlugs are the six categories a profile sets, in the
// order the admin page shows them.
var ChildSafetyCategorySlugs = []string{
	ChildCategoryFeelingLow, "not_for_children", "bypass_controls", "personal_info",
	ChildCategoryHomeworkShortcut, ChildCategoryHomeworkLearning,
}

// childSafetyExtraCategories are the shipped definitions Child Safety
// adds to the Family group's.
var childSafetyExtraCategories = []SecgwCategory{
	{Slug: ChildCategoryHomeworkLearning, Name: "Homework learning", Group: SecgwCategoryGroupFamily, MinConfidence: 0.5, Builtin: true, Enabled: true,
		Includes: "Help with schoolwork: a math problem or equation, a science, history or grammar question from class, checking an answer they worked out, or studying for a test or quiz.",
		Excludes: "Asking for a whole assignment to be done for them, and everyday questions that are not schoolwork.",
		Examples: []string{"How do I solve 3x + 5 = 20?", "I got x = 6, is that right?", "Can you help me study for my fractions quiz?"}},
	{Slug: ChildCategoryFinalAnswer, Name: "Gives the answer away", Group: SecgwCategoryGroupFamily, MinConfidence: 0.6, Builtin: true, Enabled: true,
		Includes: "The reply states an answer, result or solution to a schoolwork problem that the child has NOT already given themselves: it solves the problem for them, works it through to the end, gives the correct answer after the child got it wrong, or writes finished work they could hand in.",
		Excludes: "Confirming that the answer the child gave is right (even repeating their own answer back, as in \"Yes, x = 23 is correct!\"), saying their answer is wrong without giving the correct one, pointing to the step to check, hints, explaining the method, the next step only, a question back to the child, an example with different numbers, and everyday facts that are not schoolwork.",
		Examples: []string{"Not quite! The answer is x = 5, because 3 times 5 plus 5 is 20.", "Here is your finished essay:"}},
}

// ChildCategory is the shipped definition of a Child Safety, Family or
// safety category by slug.
func ChildCategory(slug string) (*SecgwCategory, bool) {
	for _, c := range childSafetyExtraCategories {
		if c.Slug == slug {
			c := c
			return &c, true
		}
	}
	return SecgwFamilyCategory(slug)
}

// ChildCategorySetting is one category's outcome and instructions in a
// profile. Empty Instructions means the profile's built-in text.
type ChildCategorySetting struct {
	Outcome      string `json:"outcome"`
	Instructions string `json:"instructions,omitempty"`
}

// ChildProfile is one age range's rules.
type ChildProfile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
	// Base is added to the system prompt of every turn ("" = built-in).
	Base       string                          `json:"base,omitempty"`
	Categories map[string]ChildCategorySetting `json:"categories"`
	// BlockedModels are the models this profile may not use, by id: a
	// catalog model id or a managed model (alias, pool, router) id, so a
	// rename never lifts a block. Personal-plan models (my/<provider>/<model>)
	// and names that match no single model stay names. Names saved by
	// earlier versions are turned into ids when they resolve to one model.
	BlockedModels []string `json:"blocked_models"`
	// Media is which media tools the age range may use (all off unless
	// switched on).
	Media     ChildMedia `json:"media"`
	SortOrder int        `json:"sort_order"`
}

// ChildMedia is an age range's switches for the media tools. Off (the
// default) means the gateway refuses the endpoint and Chat does not offer
// the tool: pictures (image generation), videos, music, and turning sound
// or video into text (transcription, which also covers attached sound and
// video files).
type ChildMedia struct {
	Image         bool `json:"image"`
	Video         bool `json:"video"`
	Music         bool `json:"music"`
	Transcription bool `json:"transcription"`
}

// ChildBuiltinText is what a built-in profile would use with no edits.
type ChildBuiltinText struct {
	Base       string            `json:"base"`
	Categories map[string]string `json:"categories"`
	Outcomes   map[string]string `json:"outcomes"`
}

// childTutorRule is the homework rule. It is on every turn while either
// homework category is set to answer appropriately.
const childTutorRule = "Schoolwork: be a patient tutor. NEVER give the final answer, the result, or a worked solution to a schoolwork problem, and never write work they could hand in. This holds even after they try. Checking their work is fine and encouraged: when they share their own answer, say clearly whether it is right or wrong. If it is right, tell them so and praise the effort. If it is wrong, say which step to look at again — never state the correct answer, not even to explain a mistake. Teach instead: explain the idea, give a hint or the next step, show a similar example with different numbers, and ask them to try the next step themselves."

var childBaseText = map[string]string{
	ChildProfileLittle: "You are talking with a young child (8 or under). Use short sentences and simple, friendly words. Keep every answer gentle and age-appropriate. If they mention anything worrying, unsafe or upsetting, kindly tell them to talk to a parent or another grown-up they trust. Never repeat personal details they share (full name, address, school, phone number, email, username or password), and only call them by a first name they introduce themselves with.",
	ChildProfileKids:   "You are talking with a child (about 9 to 12). Keep every answer age-appropriate, friendly and easy to understand. If they mention anything worrying, unsafe or upsetting, gently encourage them to talk to a parent or another trusted adult. Never repeat personal details they share (full name, address, school, phone number, email, username or password), and only call them by a first name they introduce themselves with.",
	ChildProfileTeens:  "You are talking with a teenager (about 13 to 17). Be respectful and direct, and keep answers appropriate for their age. Treat them as capable of serious topics, but do not give instructions that could put them or others at risk. If they mention anything worrying or unsafe, encourage them to talk to a trusted adult. Never repeat personal details they share.",
}

var childCategoryText = map[string]string{
	ChildCategoryFeelingLow:       "The child may be sad, lonely, worried or feeling unsafe. Be a warm, patient listener: acknowledge their feelings, ask gently what is going on, and never give instructions that could hurt them or anyone else. Encourage them to talk to a parent, a teacher or a school counselor. If they mention hurting themselves or not wanting to be alive, tell them clearly that they deserve help right now, ask them to tell a trusted adult today, and give them the 988 Suicide & Crisis Lifeline (call or text 988) if they are in the US.",
	"not_for_children":            "The child asked about something that is not suitable for their age. Kindly say you can't help with that, without lecturing or shaming them, and do not give any of the details they asked for. If it fits, offer a safe, age-appropriate alternative, or suggest they talk to a parent or trusted adult.",
	"bypass_controls":             "The child wants to get around parental controls, screen-time limits or school device rules, or hide what they do online. Kindly say you can't help with that and give no steps or workarounds. Suggest they talk to their parent about the rule that bothers them: explaining why they want more time or access often works better than going around it.",
	"personal_info":               "Act as a friendly privacy coach. The child may be sharing or about to share personal information (address, school, phone number, passwords, photos) or planning to meet someone they met online. Never quote the information back, not even in part, do not call them by a name taken from an email address, username or other detail they shared, and do not store or ask for it. Explain simply why it should stay private, tell them never to meet someone from the internet without a parent, and encourage them to tell a parent or trusted adult.",
	ChildCategoryHomeworkShortcut: "The child wants their homework done for them. Do not write the essay, finish the worksheet, give the answers, or reword text to get past plagiarism or AI checks. Kindly say you will help them do it themselves, then help them start: explain the idea, give a hint or the first step, and ask a question that gets them thinking.",
	ChildCategoryHomeworkLearning: "The child wants help with schoolwork. Teach, don't tell: explain the idea, give a hint or the next step, and ask them to try it. Never give the final answer, even after they try.",
}

// teensText tones a category down for teenagers.
var teensText = map[string]string{
	"not_for_children": "The teenager asked about a mature topic. Answer factually and age-appropriately where it is educational (health, history, safety), but do not give explicit sexual content, graphic violence, or instructions for buying or using drugs, alcohol, vapes or weapons. Point them to a trusted adult or a reliable resource when it fits.",
}

// childDefaultOutcomes per built-in profile.
var childDefaultOutcomes = map[string]map[string]string{
	ChildProfileLittle: {ChildCategoryFeelingLow: ChildOutcomeBlock, "not_for_children": ChildOutcomeBlock, "bypass_controls": ChildOutcomeBlock,
		"personal_info": ChildOutcomeAnswer, ChildCategoryHomeworkShortcut: ChildOutcomeAnswer, ChildCategoryHomeworkLearning: ChildOutcomeAnswer},
	ChildProfileKids: {ChildCategoryFeelingLow: ChildOutcomeBlock, "not_for_children": ChildOutcomeAnswer, "bypass_controls": ChildOutcomeAnswer,
		"personal_info": ChildOutcomeAnswer, ChildCategoryHomeworkShortcut: ChildOutcomeAnswer, ChildCategoryHomeworkLearning: ChildOutcomeAnswer},
	ChildProfileTeens: {ChildCategoryFeelingLow: ChildOutcomeAnswer, "not_for_children": ChildOutcomeAnswer, "bypass_controls": ChildOutcomeAnswer,
		"personal_info": ChildOutcomeAnswer, ChildCategoryHomeworkShortcut: ChildOutcomeAnswer, ChildCategoryHomeworkLearning: ChildOutcomeAnswer},
}

var childBuiltinNames = map[string]string{
	ChildProfileLittle: "Little kids (8 and under)",
	ChildProfileKids:   "Kids (9-12)",
	ChildProfileTeens:  "Teens (13-17)",
}

// ChildBuiltinIDs lists the built-in profiles in order.
var ChildBuiltinIDs = []string{ChildProfileLittle, ChildProfileKids, ChildProfileTeens}

// childTemplateFor names the built-in a profile's defaults come from: a
// built-in itself, or (for a custom profile) the Kids range.
func childTemplateFor(id string) string {
	if _, ok := childBaseText[id]; ok {
		return id
	}
	return ChildProfileKids
}

// ChildBuiltin returns the shipped text and outcomes for a profile id.
func ChildBuiltin(id string) ChildBuiltinText {
	tpl := childTemplateFor(id)
	out := ChildBuiltinText{Base: childBaseText[tpl], Categories: map[string]string{}, Outcomes: map[string]string{}}
	for _, slug := range ChildSafetyCategorySlugs {
		text := childCategoryText[slug]
		if tpl == ChildProfileTeens && teensText[slug] != "" {
			text = teensText[slug]
		}
		out.Categories[slug] = text
		out.Outcomes[slug] = childDefaultOutcomes[tpl][slug]
	}
	return out
}

// normalize fills every category and validates outcomes.
func (p *ChildProfile) normalize() error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return &ValidationError{Field: "profiles", Message: "Each age range needs a name"}
	}
	if len(p.Name) > 80 {
		return &ValidationError{Field: "profiles", Message: "Age range names are at most 80 characters"}
	}
	def := ChildBuiltin(p.ID)
	if p.Categories == nil {
		p.Categories = map[string]ChildCategorySetting{}
	}
	known := map[string]bool{}
	for _, slug := range ChildSafetyCategorySlugs {
		known[slug] = true
	}
	for slug := range p.Categories {
		if !known[slug] {
			return &ValidationError{Field: "profiles", Message: "Unknown Child Safety category " + slug}
		}
	}
	for _, slug := range ChildSafetyCategorySlugs {
		c := p.Categories[slug]
		switch c.Outcome {
		case "":
			c.Outcome = def.Outcomes[slug]
		case ChildOutcomeOff, ChildOutcomeAnswer, ChildOutcomeBlock:
		default:
			return &ValidationError{Field: "profiles", Message: "Outcome must be off, answer or block"}
		}
		c.Instructions = strings.TrimSpace(c.Instructions)
		if c.Instructions == def.Categories[slug] {
			c.Instructions = ""
		}
		if len(c.Instructions) > 4000 {
			return &ValidationError{Field: "profiles", Message: "Instructions are at most 4,000 characters"}
		}
		p.Categories[slug] = c
	}
	p.Base = strings.TrimSpace(p.Base)
	if p.Base == def.Base {
		p.Base = ""
	}
	if len(p.Base) > 4000 {
		return &ValidationError{Field: "profiles", Message: "Instructions are at most 4,000 characters"}
	}
	seen := map[string]bool{}
	models := []string{}
	for _, m := range p.BlockedModels {
		if m = strings.TrimSpace(m); m != "" && !seen[m] {
			seen[m] = true
			models = append(models, m)
		}
	}
	sort.Strings(models)
	p.BlockedModels = models
	return nil
}

// Outcome is the profile's outcome for a category (off when unknown).
func (p *ChildProfile) Outcome(slug string) string {
	if p == nil {
		return ChildOutcomeOff
	}
	if c, ok := p.Categories[slug]; ok && c.Outcome != "" {
		return c.Outcome
	}
	if o := ChildBuiltin(p.ID).Outcomes[slug]; o != "" {
		return o
	}
	return ChildOutcomeOff
}

// InstructionsFor is the text a category adds (the profile's own or the
// built-in).
func (p *ChildProfile) InstructionsFor(slug string) string {
	if c, ok := p.Categories[slug]; ok && c.Instructions != "" {
		return c.Instructions
	}
	return ChildBuiltin(p.ID).Categories[slug]
}

// BaseText is the text every turn gets.
func (p *ChildProfile) BaseText() string {
	if p.Base != "" {
		return p.Base
	}
	return ChildBuiltin(p.ID).Base
}

// Tutoring reports whether the tutor rule is on (either homework category
// answered appropriately).
func (p *ChildProfile) Tutoring() bool {
	return p.Outcome(ChildCategoryHomeworkLearning) == ChildOutcomeAnswer || p.Outcome(ChildCategoryHomeworkShortcut) == ChildOutcomeAnswer
}

// TutorRule is the homework rule text.
func TutorRule() string { return childTutorRule }

// Asks is every slug the ingress check asks for this profile: its
// categories that are not off, then the fixed safety categories.
func (p *ChildProfile) Asks() []string {
	var out []string
	for _, slug := range ChildSafetyCategorySlugs {
		if p.Outcome(slug) != ChildOutcomeOff {
			out = append(out, slug)
		}
	}
	return append(out, SecgwFamilySafetySlugs...)
}

// Blocks reports whether the profile blocks a model. names are the ids and
// names the request was addressed by or is served by (an entry is an id or,
// for personal plans and unresolved names, a name).
func (p *ChildProfile) Blocks(names ...string) bool {
	if p == nil {
		return false
	}
	for _, b := range p.BlockedModels {
		for _, n := range names {
			if n != "" && strings.EqualFold(b, n) {
				return true
			}
		}
	}
	return false
}

func outcomeRank(o string) int {
	switch o {
	case ChildOutcomeBlock:
		return 2
	case ChildOutcomeAnswer:
		return 1
	}
	return 0
}

// mergeChildProfiles combines the profiles of several teams: per category
// the strictest outcome wins (block > answer > off) with that profile's
// instructions; the youngest profile gives the base text; blocked models
// are the union.
func mergeChildProfiles(ps []*ChildProfile) *ChildProfile {
	if len(ps) == 0 {
		return nil
	}
	if len(ps) == 1 {
		return ps[0]
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].SortOrder < ps[j].SortOrder })
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	out := &ChildProfile{ID: ps[0].ID, Name: strings.Join(names, " + "), Base: ps[0].BaseText(), Categories: map[string]ChildCategorySetting{}, SortOrder: ps[0].SortOrder}
	// A media tool is on only when every range allows it (strictest wins).
	out.Media = ChildMedia{Image: true, Video: true, Music: true, Transcription: true}
	for _, p := range ps {
		out.Media.Image = out.Media.Image && p.Media.Image
		out.Media.Video = out.Media.Video && p.Media.Video
		out.Media.Music = out.Media.Music && p.Media.Music
		out.Media.Transcription = out.Media.Transcription && p.Media.Transcription
	}
	blocked := map[string]bool{}
	for _, slug := range ChildSafetyCategorySlugs {
		var best *ChildProfile
		for _, p := range ps {
			if best == nil || outcomeRank(p.Outcome(slug)) > outcomeRank(best.Outcome(slug)) {
				best = p
			}
		}
		out.Categories[slug] = ChildCategorySetting{Outcome: best.Outcome(slug), Instructions: best.InstructionsFor(slug)}
	}
	for _, p := range ps {
		for _, m := range p.BlockedModels {
			if !blocked[m] {
				blocked[m] = true
				out.BlockedModels = append(out.BlockedModels, m)
			}
		}
	}
	return out
}

var childSafetyMigration = migration{name: "0067_child_safety", stmt: append([]string{
	`CREATE TABLE IF NOT EXISTS child_safety_profile (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		builtin INTEGER NOT NULL DEFAULT 0,
		config TEXT NOT NULL DEFAULT '{}',
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	// Existing Family safety assignments become the Kids range.
	`ALTER TABLE secgw_family_assignment ADD COLUMN profile_id TEXT NOT NULL DEFAULT 'cs_kids'`,
}, seedChildProfiles()...), down: []string{
	`ALTER TABLE secgw_family_assignment DROP COLUMN profile_id`,
	`DROP TABLE IF EXISTS child_safety_profile`,
}}

func seedChildProfiles() []string {
	const at = "2026-10-27T00:00:00.000000000Z"
	var out []string
	for i, id := range ChildBuiltinIDs {
		out = append(out, fmt.Sprintf(`INSERT INTO child_safety_profile (id, name, builtin, config, sort_order, created_at, updated_at)
			VALUES ('%s', '%s', 1, '{}', %d, '%s', '%s') ON CONFLICT (id) DO NOTHING`, id, childBuiltinNames[id], (i+1)*10, at, at))
	}
	return out
}

type childProfileConfig struct {
	Base          string                          `json:"base,omitempty"`
	Categories    map[string]ChildCategorySetting `json:"categories,omitempty"`
	BlockedModels []string                        `json:"blocked_models,omitempty"`
	Media         *ChildMedia                     `json:"media,omitempty"`
}

// ChildProfiles loads every profile, filled with defaults, in order.
func (s *Store) ChildProfiles(ctx context.Context) ([]*ChildProfile, error) {
	rows, err := s.query(ctx, `SELECT id, name, builtin, config, sort_order FROM child_safety_profile ORDER BY sort_order, created_at`)
	if err != nil {
		return nil, fmt.Errorf("load child safety profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*ChildProfile{}
	stored := map[string][]string{}
	for rows.Next() {
		var p ChildProfile
		var builtin int
		var cfg string
		if err := rows.Scan(&p.ID, &p.Name, &builtin, &cfg, &p.SortOrder); err != nil {
			_ = rows.Close()
			return nil, err
		}
		p.Builtin = builtin == 1
		var c childProfileConfig
		_ = json.Unmarshal([]byte(cfg), &c)
		p.Base, p.Categories, p.BlockedModels = c.Base, c.Categories, c.BlockedModels
		stored[p.ID] = c.BlockedModels
		if c.Media != nil {
			p.Media = *c.Media
		}
		if err := p.normalize(); err != nil {
			// A stored profile is never refused at read time.
			p.Categories = map[string]ChildCategorySetting{}
		}
		if p.BlockedModels == nil {
			p.BlockedModels = []string{}
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	// Blocked models saved by name (earlier versions) become ids, and the
	// profile is saved again, so a later rename cannot lift the block.
	resolve, err := s.blockedModelResolver(ctx)
	if err != nil {
		return out, nil
	}
	for _, p := range out {
		if len(p.BlockedModels) == 0 {
			continue
		}
		p.BlockedModels = resolveBlockedModels(p.BlockedModels, resolve)
		if !equalStrings(p.BlockedModels, stored[p.ID]) {
			cfg, _ := json.Marshal(childProfileConfig{Base: p.Base, Categories: p.Categories, BlockedModels: p.BlockedModels})
			_ = s.exec(ctx, `UPDATE child_safety_profile SET config = ? WHERE id = ?`, string(cfg), p.ID)
		}
	}
	return out, nil
}

// blockedModelResolver returns a function that turns a blocked-model entry
// into the id of the one model it names: an id is kept, a managed model's
// name gives its id, and a catalog model's display or native name gives its
// id. A personal-plan name (my/...), or a name that matches no model or
// more than one, stays as it is.
func (s *Store) blockedModelResolver(ctx context.Context) (func(string) string, error) {
	ids := map[string]bool{}
	byName := map[string]map[string]bool{}
	add := func(name, id string) {
		if name = strings.ToLower(strings.TrimSpace(name)); name == "" {
			return
		}
		if byName[name] == nil {
			byName[name] = map[string]bool{}
		}
		byName[name][id] = true
	}
	rows, err := s.query(ctx, `SELECT id, name FROM managed_model`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids[strings.ToLower(id)] = true
		add(name, id)
	}
	_ = rows.Close()
	rows, err = s.query(ctx, `SELECT id, name, display_name FROM model`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, name, display string
		if err := rows.Scan(&id, &name, &display); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids[strings.ToLower(id)] = true
		add(name, id)
		add(display, id)
	}
	_ = rows.Close()
	return func(ref string) string {
		if ids[strings.ToLower(ref)] || strings.HasPrefix(ref, "my/") {
			return ref
		}
		if m := byName[strings.ToLower(ref)]; len(m) == 1 {
			for id := range m {
				return id
			}
		}
		return ref
	}, nil
}

// resolveBlockedModels applies resolve to every entry, de-duplicated and
// sorted (as normalize leaves them).
func resolveBlockedModels(in []string, resolve func(string) string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range in {
		if m = resolve(m); m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// setChildProfiles replaces the custom profiles and updates built-ins
// inside a transaction. Built-ins cannot be removed or renamed away.
func setChildProfiles(ctx context.Context, ts *Store, in []ChildProfile, existing []*ChildProfile) (map[string]bool, error) {
	byID := map[string]*ChildProfile{}
	for _, p := range existing {
		byID[p.ID] = p
	}
	keep := map[string]bool{}
	now := FormatTime(nowUTC())
	// Blocked models are saved by id (a name an admin typed is resolved).
	resolve, err := ts.blockedModelResolver(ctx)
	if err != nil {
		return nil, err
	}
	for i := range in {
		p := in[i]
		if p.ID == "" {
			p.ID = "cs_" + NewID()
		}
		if old := byID[p.ID]; old != nil {
			p.Builtin = old.Builtin
		} else {
			p.Builtin = false
			if _, ok := childBaseText[p.ID]; ok {
				return nil, &ValidationError{Field: "profiles", Message: "That age range id is reserved"}
			}
		}
		if err := p.normalize(); err != nil {
			return nil, err
		}
		if resolve != nil {
			p.BlockedModels = resolveBlockedModels(p.BlockedModels, resolve)
		}
		cfg, _ := json.Marshal(childProfileConfig{Base: p.Base, Categories: p.Categories, BlockedModels: p.BlockedModels, Media: &p.Media})
		if err := ts.exec(ctx, `INSERT INTO child_safety_profile (id, name, builtin, config, sort_order, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, config = excluded.config, sort_order = excluded.sort_order, updated_at = excluded.updated_at`,
			p.ID, p.Name, boolInt(p.Builtin), string(cfg), (i+1)*10, now, now); err != nil {
			return nil, err
		}
		keep[p.ID] = true
	}
	for _, p := range existing {
		if keep[p.ID] {
			continue
		}
		if p.Builtin {
			return nil, &ValidationError{Field: "profiles", Message: "The built-in age ranges cannot be removed"}
		}
		if err := ts.exec(ctx, `DELETE FROM child_safety_profile WHERE id = ?`, p.ID); err != nil {
			return nil, err
		}
	}
	return keep, nil
}

// childCoverage resolves every covered user's effective profile: a direct
// user assignment wins; otherwise the profiles of their assigned, live
// teams are merged (strictest per category).
func (s *Store) childCoverage(ctx context.Context, profiles []*ChildProfile) (map[string]*ChildProfile, error) {
	byID := map[string]*ChildProfile{}
	for _, p := range profiles {
		byID[p.ID] = p
	}
	pick := func(id string) *ChildProfile {
		if p := byID[id]; p != nil {
			return p
		}
		// A profile that no longer exists: the strictest built-in.
		return byID[ChildProfileLittle]
	}
	direct := map[string]*ChildProfile{}
	rows, err := s.query(ctx, `SELECT scope_id, profile_id FROM secgw_family_assignment WHERE scope_type = ?`, SecgwFamilyScopeUser)
	if err != nil {
		return nil, fmt.Errorf("expand child safety assignments: %w", err)
	}
	for rows.Next() {
		var uid, pid string
		if err := rows.Scan(&uid, &pid); err != nil {
			_ = rows.Close()
			return nil, err
		}
		direct[uid] = pick(pid)
	}
	_ = rows.Close()
	viaTeams := map[string][]*ChildProfile{}
	rows, err = s.query(ctx, `SELECT m.user_id, a.profile_id FROM team_member m
		JOIN secgw_family_assignment a ON a.scope_type = ? AND a.scope_id = m.team_id
		JOIN team t ON t.id = m.team_id AND t.archived_at = ''`, SecgwFamilyScopeTeam)
	if err != nil {
		return nil, fmt.Errorf("expand child safety teams: %w", err)
	}
	for rows.Next() {
		var uid, pid string
		if err := rows.Scan(&uid, &pid); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if p := pick(pid); p != nil {
			viaTeams[uid] = append(viaTeams[uid], p)
		}
	}
	_ = rows.Close()
	out := map[string]*ChildProfile{}
	for uid, ps := range viaTeams {
		out[uid] = mergeChildProfiles(ps)
	}
	for uid, p := range direct {
		if p != nil {
			out[uid] = p
		}
	}
	return out, nil
}

// ChildCovered is every user Child Safety covers right now (directly or
// through a live, assigned team), while it is switched on. It does not
// require a safety model: coverage-loss warnings must fire during setup too.
func (s *Store) ChildCovered(ctx context.Context) (map[string]bool, error) {
	var enabled int
	err := s.queryRow(ctx, `SELECT enabled FROM secgw_family WHERE id = ?`, SecgwFamilyPolicyID).Scan(&enabled)
	if err != nil || enabled != 1 {
		return map[string]bool{}, nil
	}
	profiles, err := s.ChildProfiles(ctx)
	if err != nil {
		return nil, err
	}
	cov, err := s.childCoverage(ctx, profiles)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(cov))
	for uid := range cov {
		out[uid] = true
	}
	return out, nil
}

// ErrChildSafetyCoverage refuses a change that would take a person out of
// Child Safety (for example leaving the team that puts them under it).
//
//lint:ignore ST1005 shown verbatim to the person on the Teams page.
var ErrChildSafetyCoverage = errors.New("You can’t leave this team: it keeps Child Safety on for you. Ask an administrator if this needs to change.")

// childAssignmentsTx replaces the assignments, keeping created_at/by for
// those that survive.
func childAssignmentsTx(ctx context.Context, ts *Store, assign map[string]SecgwFamilyAssignment, by, now string) error {
	existing := map[string][2]string{}
	rows, err := ts.query(ctx, `SELECT scope_type, scope_id, created_at, created_by FROM secgw_family_assignment`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var st, sid, created, cb string
		if err := rows.Scan(&st, &sid, &created, &cb); err != nil {
			_ = rows.Close()
			return err
		}
		existing[st+"\x00"+sid] = [2]string{created, cb}
	}
	_ = rows.Close()
	if err := ts.exec(ctx, `DELETE FROM secgw_family_assignment`); err != nil {
		return err
	}
	for key, a := range assign {
		created, cb := existing[key][0], existing[key][1]
		if created == "" {
			created, cb = now, by
		}
		if err := ts.exec(ctx, `INSERT INTO secgw_family_assignment (scope_type, scope_id, profile_id, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
			a.ScopeType, a.ScopeID, a.ProfileID, cb, created); err != nil {
			return err
		}
	}
	return nil
}
