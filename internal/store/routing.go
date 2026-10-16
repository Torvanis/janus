package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Auto-routing (migration 0049).
//
// A router is a managed model of kind "router": callers address it by name
// (auto/legal) exactly like an alias, it is granted, bound to security
// policies and scoped by quotas in its own right, and it serves each request
// from ONE of its routes. Which route is decided per request by a decision
// classifier that answers two questions about the new turn: which routing
// category the request belongs to and how difficult it is. Routes are tried
// in order; the first whose categories, difficulty band, capability and
// quota all fit serves. Nothing fitting, a low-confidence answer, a slow or
// failed classifier: the router's default serves. A router never fails a
// request because routing could not decide.
//
// Routing categories are a taxonomy of their own (not the Usage-insights
// taxonomy), because routing needs criteria tuned for "which model should
// answer this", but a routing category can point at an Insights category
// and then follows that category's definition.
//
// Prompt text is never stored: decisions keep the label, its confidence and
// timing. Evaluations keep verdicts and scores, never the two answers.

var routingMigration = migration{name: "0049_auto_routing", stmt: append([]string{
	`ALTER TABLE managed_model ADD COLUMN kind TEXT NOT NULL DEFAULT 'alias'`,
	`CREATE TABLE IF NOT EXISTS routing_category (
		id TEXT PRIMARY KEY,
		slug TEXT NOT NULL,
		name TEXT NOT NULL,
		includes TEXT NOT NULL DEFAULT '',
		excludes TEXT NOT NULL DEFAULT '',
		linked_slug TEXT NOT NULL DEFAULT '',
		min_confidence REAL NOT NULL DEFAULT 0.5,
		examples TEXT NOT NULL DEFAULT '[]',
		builtin INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_routing_category_slug ON routing_category(slug)`,
	`CREATE TABLE IF NOT EXISTS routing_router (
		managed_model_id TEXT PRIMARY KEY,
		mode TEXT NOT NULL DEFAULT 'shadow',
		config TEXT NOT NULL DEFAULT '{}',
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS routing_decision (
		id TEXT PRIMARY KEY,
		created_at TEXT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		router_id TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT '',
		route_id TEXT NOT NULL DEFAULT '',
		route_name TEXT NOT NULL DEFAULT '',
		model_id TEXT NOT NULL DEFAULT '',
		model_name TEXT NOT NULL DEFAULT '',
		category TEXT NOT NULL DEFAULT '',
		confidence REAL NOT NULL DEFAULT 0,
		difficulty REAL NOT NULL DEFAULT 0,
		thinking TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		decision_ms INTEGER NOT NULL DEFAULT 0,
		calls INTEGER NOT NULL DEFAULT 0,
		user_id TEXT NOT NULL DEFAULT '',
		service_token_id TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_routing_decision_router_time ON routing_decision(router_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_routing_decision_time ON routing_decision(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_routing_decision_request ON routing_decision(request_id)`,
	`CREATE TABLE IF NOT EXISTS routing_sticky (
		key TEXT PRIMARY KEY,
		router_id TEXT NOT NULL,
		route_id TEXT NOT NULL,
		category TEXT NOT NULL DEFAULT '',
		difficulty REAL NOT NULL DEFAULT 0,
		expires_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_routing_sticky_expiry ON routing_sticky(expires_at)`,
	`CREATE TABLE IF NOT EXISTS routing_eval_result (
		id TEXT PRIMARY KEY,
		created_at TEXT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		router_id TEXT NOT NULL,
		route_id TEXT NOT NULL,
		routed_model_id TEXT NOT NULL DEFAULT '',
		default_model_id TEXT NOT NULL DEFAULT '',
		judge_model_id TEXT NOT NULL DEFAULT '',
		verdict TEXT NOT NULL DEFAULT '',
		routed_score REAL NOT NULL DEFAULT 0,
		default_score REAL NOT NULL DEFAULT 0,
		consistent INTEGER NOT NULL DEFAULT 0,
		rationale TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		routed_cost_nanousd BIGINT NOT NULL DEFAULT 0,
		default_cost_nanousd BIGINT NOT NULL DEFAULT 0,
		eval_cost_nanousd BIGINT NOT NULL DEFAULT 0,
		routed_latency_ms INTEGER NOT NULL DEFAULT 0,
		default_latency_ms INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_routing_eval_route_time ON routing_eval_result(router_id, route_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_routing_eval_time ON routing_eval_result(created_at)`,
	// The router (or alias) a request was addressed to, so quotas scoped to
	// it can count its traffic, and why a router chose the model it did.
	`ALTER TABLE usage_event ADD COLUMN managed_model_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE usage_event ADD COLUMN route_reason TEXT NOT NULL DEFAULT ''`,
}, seedRoutingCategoryStatements()...), down: []string{
	// Routers are managed models of kind router: without their config table
	// they would be callable as broken aliases, so they go with it (and the
	// grants that point at them).
	`DELETE FROM model_grant WHERE model_kind = 'managed' AND model_id IN (SELECT id FROM managed_model WHERE kind = 'router')`,
	`DELETE FROM managed_model WHERE kind = 'router'`,
	`ALTER TABLE usage_event DROP COLUMN route_reason`,
	`ALTER TABLE usage_event DROP COLUMN managed_model_id`,
	`ALTER TABLE managed_model DROP COLUMN kind`,
	`DROP INDEX IF EXISTS idx_routing_eval_time`,
	`DROP INDEX IF EXISTS idx_routing_eval_route_time`,
	`DROP TABLE IF EXISTS routing_eval_result`,
	`DROP INDEX IF EXISTS idx_routing_sticky_expiry`,
	`DROP TABLE IF EXISTS routing_sticky`,
	`DROP INDEX IF EXISTS idx_routing_decision_request`,
	`DROP INDEX IF EXISTS idx_routing_decision_time`,
	`DROP INDEX IF EXISTS idx_routing_decision_router_time`,
	`DROP TABLE IF EXISTS routing_decision`,
	`DROP TABLE IF EXISTS routing_router`,
	`DROP INDEX IF EXISTS idx_routing_category_slug`,
	`DROP TABLE IF EXISTS routing_category`,
}}

// servedManagedModelMigration records the alias a router's route served
// through, so a quota on that alias is consumed by routed traffic and
// ledger rebuilds from the event log count it (migration 0051).
var servedManagedModelMigration = migration{name: "0051_usage_served_managed_model", stmt: []string{
	`ALTER TABLE usage_event ADD COLUMN served_managed_model_id TEXT NOT NULL DEFAULT ''`,
}, down: []string{
	`ALTER TABLE usage_event DROP COLUMN served_managed_model_id`,
}}

// Managed model kinds.
const (
	ManagedKindAlias  = "alias"
	ManagedKindRouter = "router"
)

// --- Categories --------------------------------------------------------------

// RoutingCategory is one answer the routing classifier can give.
//
// Includes and Excludes are what the classifier reads: what belongs, and the
// near neighbours that do not (measured: full in/NOT-in criteria beat
// names alone by several points of accuracy). A category with LinkedSlug
// follows a Usage-insights category instead: its name and description come
// from there at plan-build time, and Includes/Excludes are ignored.
type RoutingCategory struct {
	ID            string    `json:"id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	Includes      string    `json:"includes"`
	Excludes      string    `json:"excludes"`
	LinkedSlug    string    `json:"linked_slug"`
	MinConfidence float64   `json:"min_confidence"`
	Examples      []string  `json:"examples"`
	Builtin       bool      `json:"builtin"`
	Enabled       bool      `json:"enabled"`
	SortOrder     int       `json:"sort_order"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// Read-time presentation.
	LinkedName string   `json:"linked_name,omitempty"`
	UsedBy     []string `json:"used_by"`
}

// RoutingCategoryNone is the answer for "none of these"; it is always
// offered so a request that fits nothing is not forced into a category.
const RoutingCategoryNone = "none"

// MaxRoutingCategories keeps the question within every backend's option
// limit (Ollama 26, leaving room for "none of these").
const MaxRoutingCategories = 25

// DefaultRoutingMinConfidence is the probability below which a category's
// answer does not count as a match.
const DefaultRoutingMinConfidence = 0.5

// BuiltinRoutingCategories are the categories Janus ships. They are the
// fourteen measured in the autoselection study (Jev 94% / Clef 95% on a
// blind-labelled set) with Specialist split so Legal is its own category,
// the example the admin guide uses for "keep this kind of work on-prem".
// Examples are prompts that SHOULD land in the category; the batch test
// scores them.
var BuiltinRoutingCategories = []RoutingCategory{
	{Slug: "quick_facts", Name: "Quick facts and everyday questions",
		Includes: "Short questions with a well-known or easily checked answer: definitions, facts, conversions, simple how-to, recommendations and everyday practical questions, answered in a few sentences.",
		Excludes: "Questions that need sources weighed or a report (Research and analysis); school or course work (Homework and learning); a decision needing expert judgement (Specialist or Legal).",
		Examples: []string{"convert 100 kilometers to miles", "what is the capital of australia?", "how long should I boil an egg for a runny yolk?", "what's the difference between a latte and a flat white?"}},
	{Slug: "writing", Name: "Writing and editing",
		Includes: "Writing, rewriting, proofreading or changing the tone of non-fiction prose: emails, letters, messages, reports, articles, posts, speeches, cover letters. The output is prose for a reader.",
		Excludes: "Fill-in templates, forms, minutes or policies (Admin documents); stories, poems or fiction (Creative writing); condensing a supplied text (Summarising).",
		Examples: []string{"Rewrite this email so it sounds less passive-aggressive: 'As per my last email, I'm still waiting.'", "Draft a LinkedIn post announcing our new office in Denver.", "Proofread my cover letter and tighten the second paragraph."}},
	{Slug: "admin_docs", Name: "Admin documents and forms",
		Includes: "Producing or completing standard paperwork with a known structure: forms, templates, meeting minutes and agendas, invoices, checklists, job descriptions, policies, standard operating procedures, rotas.",
		Excludes: "Free-form prose like emails or articles (Writing and editing); drafting or interpreting legal terms (Legal).",
		Examples: []string{"Create an agenda for a 30-minute weekly team sync.", "Write a job description for a junior bookkeeper.", "Make a checklist for onboarding a new employee in their first week."}},
	{Slug: "summarising", Name: "Summarising and extracting",
		Includes: "Condensing, restructuring or pulling information out of text the user supplies: summaries, key points, action items, tables of names or dates from a document, turning notes into bullets. The answer comes from the supplied material.",
		Excludes: "Finding new information or comparing sources (Research and analysis); analysing numbers or datasets (Data analysis); converting between languages (Translation).",
		Examples: []string{"Summarise this meeting transcript into five bullet points and list the action items.", "Pull every date and person's name out of the text below into a table."}},
	{Slug: "translation", Name: "Translation",
		Includes: "Converting text between human languages, or explaining what a phrase in another language means, including localising tone or wording for a region.",
		Excludes: "Converting between programming languages (Coding).",
		Examples: []string{"Translate this paragraph into formal German.", "What does 'ça me fait une belle jambe' mean?"}},
	{Slug: "coding", Name: "Coding",
		Includes: "Writing, reviewing, explaining, refactoring or debugging source code, scripts, queries, regular expressions or configuration as code, and questions about software design, libraries and tooling.",
		Excludes: "Spreadsheet formulas or analysing a dataset (Data analysis); a student's programming assignment where the aim is to learn (Homework and learning).",
		Examples: []string{"Write a Python function that deduplicates a list while keeping order.", "Why does this Go code deadlock when the channel is unbuffered?", "Refactor this React component to use a custom hook for the fetch logic."}},
	{Slug: "data_analysis", Name: "Data analysis and spreadsheets",
		Includes: "Working with numbers and data: statistics, spreadsheet formulas, pivot tables, charts, interpreting metrics, survey results or experiments, cleaning or comparing datasets.",
		Excludes: "Pure maths or science problems without real data (Maths and science); writing an application or data pipeline in code (Coding).",
		Examples: []string{"Write an Excel formula that sums column C when column A says 'paid'.", "Our A/B test had 4.1% vs 4.6% conversion on 12k visitors each — is that significant?"}},
	{Slug: "maths_science", Name: "Maths and science problem solving",
		Includes: "Solving or explaining maths, physics, chemistry, biology, engineering or other science problems: calculations, proofs, derivations, mechanisms, estimates, technical explanations.",
		Excludes: "School or course work where the user is a student learning the topic (Homework and learning); analysing a real dataset (Data analysis); clinical medical advice (Specialist).",
		Examples: []string{"Derive the terminal velocity of a sphere falling through a viscous fluid.", "Estimate the heat load of a 40-person meeting room."}},
	{Slug: "research", Name: "Research and in-depth analysis",
		Includes: "Investigating a topic in depth: comparing options or sources, literature or market research, explaining a complex subject thoroughly, building an argument, multi-part analysis or a report that needs many facts pulled together and weighed.",
		Excludes: "A single fact (Quick facts); condensing one supplied document (Summarising); a personal or business decision (Planning, strategy and advice).",
		Examples: []string{"Compare the main approaches to grid-scale energy storage and their trade-offs.", "Give me an overview of the research on remote work and productivity."}},
	{Slug: "planning_advice", Name: "Planning, strategy and advice",
		Includes: "Helping the user decide or plan: business or product strategy, project plans, prioritising, schedules and itineraries, career moves, negotiating, weighing pros and cons of their own situation.",
		Excludes: "Advice that needs a lawyer (Legal) or a doctor, accountant or financial adviser (Specialist); emotional support (Personal and well-being); a general investigation of a topic (Research and analysis); a coding or technical question phrased as 'what should I do' (Coding).",
		Examples: []string{"Plan a 5-day itinerary for Lisbon on a mid-range budget.", "Should we launch the beta in March or wait for the payments feature?"}},
	{Slug: "homework_learning", Name: "Homework and learning help",
		Includes: "A student or learner working on school, college or course material: homework questions, assignments, essay questions set by a teacher, revision, exam practice, quizzes, explaining a topic so they understand it, checking their answer. Applies whatever the subject.",
		Excludes: "A professional or personal task that only uses the same skills.",
		Examples: []string{"I have a chemistry test tomorrow, can you quiz me on balancing equations?", "My teacher asked us to explain the causes of WW1 in 300 words, can you check my draft?"}},
	{Slug: "creative_writing", Name: "Creative writing",
		Includes: "Fiction and imaginative writing: stories, poems, lyrics, scripts, role-play, jokes, character and world building, creative names and slogans played for fun.",
		Excludes: "Practical prose like emails or articles, or marketing copy for a real business (Writing and editing).",
		Examples: []string{"Write a short ghost story set in a lighthouse.", "Give me a limerick about a cat who hates Mondays."}},
	{Slug: "personal_wellbeing", Name: "Personal conversations and well-being",
		Includes: "The user talking about themselves or their feelings: stress, worry, loneliness, motivation, relationships, family, friendships, confidence, grief, sleep, wanting someone to talk to, or casual chat with the assistant.",
		Excludes: "Practical planning of their life (Planning, strategy and advice); a medical diagnosis or treatment question (Specialist).",
		Examples: []string{"I've been feeling really overwhelmed at work lately and can't switch off.", "hey, how's your day going?"}},
	{Slug: "specialist", Name: "Specialist professional (medical, financial)",
		Includes: "Questions whose good answer depends on a qualified professional's knowledge and carries real consequences: medical symptoms, medication and treatment, tax, accounting, investments, insurance, pensions.",
		Excludes: "Legal rights, contracts, disputes and regulatory compliance (Legal); a general fact about these fields (Quick facts); a student studying medicine or finance (Homework and learning).",
		Examples: []string{"I've had a dull headache behind one eye for three days, should I worry?", "Can I deduct my home office if I'm a W-2 employee who works remotely?"}},
	{Slug: "legal", Name: "Legal",
		Includes: "Legal questions and legal work: rights and obligations, contracts and their clauses, disputes, employment law, privacy and data-protection law, regulatory compliance, drafting or reviewing legal documents.",
		Excludes: "Standard business paperwork with no legal judgement (Admin documents); a student studying law (Homework and learning); medical or financial questions (Specialist).",
		Examples: []string{"Is a non-compete clause enforceable in California for a remote employee?", "Review this indemnification clause and tell me what risk it puts on us.", "Our supplier missed the delivery date in the contract — what are our options?"}},
}

func seedRoutingCategoryStatements() []string {
	const seededAt = "2026-10-07T00:00:00.000000000Z"
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	out := make([]string, 0, len(BuiltinRoutingCategories))
	for i, c := range BuiltinRoutingCategories {
		examples, _ := json.Marshal(c.Examples)
		out = append(out, fmt.Sprintf(`INSERT INTO routing_category (id, slug, name, includes, excludes, linked_slug, min_confidence, examples, builtin, enabled, sort_order, created_at, updated_at)
			VALUES (%s, %s, %s, %s, %s, '', %g, %s, 1, 1, %d, '%s', '%s') ON CONFLICT (slug) DO NOTHING`,
			quote("rcat_"+c.Slug), quote(c.Slug), quote(c.Name), quote(c.Includes), quote(c.Excludes),
			DefaultRoutingMinConfidence, quote(string(examples)), (i+1)*10, seededAt, seededAt))
	}
	return out
}

const routingCategoryColumns = `id, slug, name, includes, excludes, linked_slug, min_confidence, examples, builtin, enabled, sort_order, created_at, updated_at`

func scanRoutingCategory(scan func(...any) error) (*RoutingCategory, error) {
	var c RoutingCategory
	var examples, created, updated string
	var builtin, enabled int
	if err := scan(&c.ID, &c.Slug, &c.Name, &c.Includes, &c.Excludes, &c.LinkedSlug, &c.MinConfidence, &examples,
		&builtin, &enabled, &c.SortOrder, &created, &updated); err != nil {
		return nil, err
	}
	c.Builtin, c.Enabled = builtin == 1, enabled == 1
	c.CreatedAt, c.UpdatedAt = ParseTime(created), ParseTime(updated)
	c.Examples = []string{}
	_ = json.Unmarshal([]byte(examples), &c.Examples)
	if c.Examples == nil {
		c.Examples = []string{}
	}
	c.UsedBy = []string{}
	return &c, nil
}

// ListRoutingCategories returns every routing category in display order,
// with linked Insights names resolved.
func (s *Store) ListRoutingCategories(ctx context.Context) ([]RoutingCategory, error) {
	rows, err := s.query(ctx, `SELECT `+routingCategoryColumns+` FROM routing_category ORDER BY sort_order ASC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list routing categories: %w", err)
	}
	out := []RoutingCategory{}
	for rows.Next() {
		c, err := scanRoutingCategory(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan routing category: %w", err)
		}
		out = append(out, *c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	linked := map[string]string{}
	if cats, err := s.ListClassificationCategories(ctx); err == nil {
		for _, c := range cats {
			linked[c.Slug] = c.Name
			if c.ParentSlug != "" {
				for _, p := range cats {
					if p.Slug == c.ParentSlug {
						linked[c.Slug] = p.Name + " > " + c.Name
					}
				}
			}
		}
	}
	for i := range out {
		if out[i].LinkedSlug != "" {
			out[i].LinkedName = linked[out[i].LinkedSlug]
		}
	}
	return out, nil
}

// RoutingCategoryByID loads one category.
func (s *Store) RoutingCategoryByID(ctx context.Context, id string) (*RoutingCategory, error) {
	c, err := scanRoutingCategory(s.queryRow(ctx, `SELECT `+routingCategoryColumns+` FROM routing_category WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load routing category: %w", err)
	}
	return c, nil
}

// RoutingCategoryInput is a category as an admin submits it. Nil fields are
// left unchanged on update.
type RoutingCategoryInput struct {
	Name          *string   `json:"name"`
	Includes      *string   `json:"includes"`
	Excludes      *string   `json:"excludes"`
	LinkedSlug    *string   `json:"linked_slug"`
	MinConfidence *float64  `json:"min_confidence"`
	Examples      *[]string `json:"examples"`
	Enabled       *bool     `json:"enabled"`
	SortOrder     *int      `json:"sort_order"`
}

func (s *Store) applyRoutingCategoryInput(ctx context.Context, c *RoutingCategory, in RoutingCategoryInput) error {
	if in.Name != nil {
		c.Name = strings.TrimSpace(*in.Name)
	}
	if in.Includes != nil {
		c.Includes = strings.TrimSpace(*in.Includes)
	}
	if in.Excludes != nil {
		c.Excludes = strings.TrimSpace(*in.Excludes)
	}
	if in.LinkedSlug != nil {
		c.LinkedSlug = strings.TrimSpace(*in.LinkedSlug)
	}
	if in.MinConfidence != nil {
		c.MinConfidence = *in.MinConfidence
	}
	if in.Examples != nil {
		c.Examples = []string{}
		for _, e := range *in.Examples {
			if e = strings.TrimSpace(e); e != "" {
				c.Examples = append(c.Examples, e)
			}
		}
	}
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	}
	if in.SortOrder != nil {
		c.SortOrder = *in.SortOrder
	}
	if c.Name == "" {
		return &ValidationError{Field: "name", Message: "give the category a name"}
	}
	if len(c.Name) > 80 {
		return &ValidationError{Field: "name", Message: "names are limited to 80 characters"}
	}
	if len(c.Includes) > 1500 || len(c.Excludes) > 1500 {
		return &ValidationError{Field: "includes", Message: "descriptions are limited to 1,500 characters each"}
	}
	if c.LinkedSlug == "" && c.Includes == "" {
		return &ValidationError{Field: "includes", Message: "describe what belongs in this category (the classifier reads it), or link it to a Usage insights category"}
	}
	if c.LinkedSlug != "" {
		if _, err := s.categoryBySlug(ctx, c.LinkedSlug); err != nil {
			if errors.Is(err, ErrNotFound) {
				return &ValidationError{Field: "linked_slug", Message: "that Usage insights category does not exist"}
			}
			return err
		}
	}
	if math.IsNaN(c.MinConfidence) || c.MinConfidence < 0 || c.MinConfidence > 0.99 {
		return &ValidationError{Field: "min_confidence", Message: "minimum confidence must be between 0 and 0.99"}
	}
	if len(c.Examples) > 20 {
		return &ValidationError{Field: "examples", Message: "keep test examples to 20 or fewer"}
	}
	for _, e := range c.Examples {
		if len(e) > 4000 {
			return &ValidationError{Field: "examples", Message: "each test example is limited to 4,000 characters"}
		}
	}
	return nil
}

func (s *Store) assertRoutingCategoryLimit(ctx context.Context, excludeID string) error {
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM routing_category WHERE enabled = 1 AND id <> ?`, excludeID).Scan(&n); err != nil {
		return err
	}
	if n+1 > MaxRoutingCategories {
		return &ValidationError{Field: "enabled", Message: fmt.Sprintf("at most %d routing categories can be enabled at once (classifiers cap the options in one question); disable one first", MaxRoutingCategories)}
	}
	return nil
}

// CreateRoutingCategory adds a custom category.
func (s *Store) CreateRoutingCategory(ctx context.Context, in RoutingCategoryInput) (*RoutingCategory, error) {
	now := nowUTC()
	c := &RoutingCategory{ID: NewID(), MinConfidence: DefaultRoutingMinConfidence, Examples: []string{}, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := s.applyRoutingCategoryInput(ctx, c, in); err != nil {
		return nil, err
	}
	c.Slug = ClassificationSlug(c.Name)
	if c.Slug == "" || c.Slug == RoutingCategoryNone {
		return nil, &ValidationError{Field: "name", Message: "choose a name with some letters or digits"}
	}
	if c.Enabled {
		if err := s.assertRoutingCategoryLimit(ctx, ""); err != nil {
			return nil, err
		}
	}
	if in.SortOrder == nil {
		var max sql.NullInt64
		_ = s.queryRow(ctx, `SELECT MAX(sort_order) FROM routing_category`).Scan(&max)
		c.SortOrder = int(max.Int64) + 10
	}
	examples, _ := json.Marshal(c.Examples)
	err := s.exec(ctx, `INSERT INTO routing_category (`+routingCategoryColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Slug, c.Name, c.Includes, c.Excludes, c.LinkedSlug, c.MinConfidence, string(examples), 0, boolInt(c.Enabled), c.SortOrder,
		FormatTime(c.CreatedAt), FormatTime(c.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, &ValidationError{Field: "name", Message: "a routing category with that name already exists"}
		}
		return nil, err
	}
	return c, nil
}

// UpdateRoutingCategory edits a category. Built-in categories can be
// re-described, re-linked and disabled, but keep their identity.
func (s *Store) UpdateRoutingCategory(ctx context.Context, id string, in RoutingCategoryInput) (*RoutingCategory, error) {
	c, err := s.RoutingCategoryByID(ctx, id)
	if err != nil {
		return nil, err
	}
	wasEnabled := c.Enabled
	if err := s.applyRoutingCategoryInput(ctx, c, in); err != nil {
		return nil, err
	}
	if c.Enabled && !wasEnabled {
		if err := s.assertRoutingCategoryLimit(ctx, c.ID); err != nil {
			return nil, err
		}
	}
	if !c.Enabled && wasEnabled {
		if users, err := s.routersUsingCategory(ctx, c.Slug); err != nil {
			return nil, err
		} else if len(users) > 0 {
			return nil, &ValidationError{Field: "enabled", Message: "routers " + strings.Join(users, ", ") + " use this category; remove it from their routes before disabling it"}
		}
	}
	c.UpdatedAt = nowUTC()
	examples, _ := json.Marshal(c.Examples)
	if err := s.exec(ctx, `UPDATE routing_category SET name = ?, includes = ?, excludes = ?, linked_slug = ?, min_confidence = ?, examples = ?, enabled = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.Includes, c.Excludes, c.LinkedSlug, c.MinConfidence, string(examples), boolInt(c.Enabled), c.SortOrder, FormatTime(c.UpdatedAt), c.ID); err != nil {
		return nil, err
	}
	return c, nil
}

// ResetRoutingCategory restores a built-in category's shipped definition.
func (s *Store) ResetRoutingCategory(ctx context.Context, id string) (*RoutingCategory, error) {
	c, err := s.RoutingCategoryByID(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, b := range BuiltinRoutingCategories {
		if b.Slug == c.Slug && c.Builtin {
			name, inc, exc, link, conf, ex := b.Name, b.Includes, b.Excludes, "", DefaultRoutingMinConfidence, b.Examples
			return s.UpdateRoutingCategory(ctx, id, RoutingCategoryInput{Name: &name, Includes: &inc, Excludes: &exc, LinkedSlug: &link, MinConfidence: &conf, Examples: &ex})
		}
	}
	return nil, &ValidationError{Field: "id", Message: "only built-in categories can be reset"}
}

// DeleteRoutingCategory removes a custom category. Built-ins are disabled
// instead, and a category a router still uses cannot go.
func (s *Store) DeleteRoutingCategory(ctx context.Context, id string) error {
	c, err := s.RoutingCategoryByID(ctx, id)
	if err != nil {
		return err
	}
	if c.Builtin {
		return &ValidationError{Field: "id", Message: "built-in categories cannot be deleted; disable it instead"}
	}
	if users, err := s.routersUsingCategory(ctx, c.Slug); err != nil {
		return err
	} else if len(users) > 0 {
		return &ValidationError{Field: "id", Message: "routers " + strings.Join(users, ", ") + " use this category; remove it from their routes first"}
	}
	return s.exec(ctx, `DELETE FROM routing_category WHERE id = ?`, id)
}

func (s *Store) routersUsingCategory(ctx context.Context, slug string) ([]string, error) {
	routers, err := s.ListRouters(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range routers {
		for _, route := range r.Config.Routes {
			if containsString(route.Categories, slug) {
				out = append(out, r.Name)
				break
			}
		}
	}
	return out, nil
}

// --- Settings ----------------------------------------------------------------

// SettingRouting is the system_setting key for routing-wide settings.
const SettingRouting = "auto_routing"

// Difficulty question styles.
const (
	// RoutingDifficultyGeneral asks one five-level complexity question for
	// every request (AUC 0.83-0.85 against judge-derived labels).
	RoutingDifficultyGeneral = "general"
	// RoutingDifficultyCategory asks a category-specific scale where one
	// exists (coding: AUC 0.87-0.89), the general one otherwise.
	RoutingDifficultyCategory = "category"
)

// RoutingSettings is the admin configuration shared by every router.
type RoutingSettings struct {
	// ClassifierModelID is the decision model (catalog model with role
	// "decision") that answers the routing questions.
	ClassifierModelID string `json:"classifier_model_id"`
	// MaxConcurrentDecisions caps in-flight routing calls per replica; a
	// request over it goes straight to its router's default.
	MaxConcurrentDecisions int `json:"max_concurrent_decisions"`
	// DifficultyStyle selects the difficulty question.
	DifficultyStyle string `json:"difficulty_style"`
	// MaxChars trims the text the classifier reads (most recent kept).
	MaxChars int `json:"max_chars"`
	// Evaluation-wide limits.
	EvalJudgeModelID       string `json:"eval_judge_model_id"`
	EvalMaxConcurrent      int    `json:"eval_max_concurrent"`
	EvalPauseAboveInFlight int    `json:"eval_pause_above_in_flight"`
	EvalMaxOutputTokens    int    `json:"eval_max_output_tokens"`
	// RetentionDays deletes decisions and evaluation verdicts older than
	// this (0 keeps them).
	RetentionDays int       `json:"retention_days"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// DefaultRoutingSettings is the starting point before an admin saves.
func DefaultRoutingSettings() RoutingSettings {
	return RoutingSettings{MaxConcurrentDecisions: 32, DifficultyStyle: RoutingDifficultyGeneral, MaxChars: 6000,
		EvalMaxConcurrent: 2, EvalPauseAboveInFlight: 50, EvalMaxOutputTokens: 2048, RetentionDays: 90}
}

// Validate normalises and checks the settings.
func (r *RoutingSettings) Validate() error {
	r.ClassifierModelID = strings.TrimSpace(r.ClassifierModelID)
	if r.MaxConcurrentDecisions < 1 || r.MaxConcurrentDecisions > 1024 {
		return &ValidationError{Field: "max_concurrent_decisions", Message: "concurrent decisions must be between 1 and 1024"}
	}
	switch r.DifficultyStyle {
	case RoutingDifficultyGeneral, RoutingDifficultyCategory:
	default:
		return &ValidationError{Field: "difficulty_style", Message: "difficulty style must be general or category"}
	}
	if r.MaxChars < 200 || r.MaxChars > 200000 {
		return &ValidationError{Field: "max_chars", Message: "maximum characters must be between 200 and 200,000"}
	}
	if r.EvalMaxConcurrent < 1 || r.EvalMaxConcurrent > 32 {
		return &ValidationError{Field: "eval_max_concurrent", Message: "concurrent evaluations must be between 1 and 32"}
	}
	if r.EvalPauseAboveInFlight < 0 || r.EvalPauseAboveInFlight > 100000 {
		return &ValidationError{Field: "eval_pause_above_in_flight", Message: "the pause threshold must be between 0 (never pause) and 100,000"}
	}
	if r.EvalMaxOutputTokens < 64 || r.EvalMaxOutputTokens > 32000 {
		return &ValidationError{Field: "eval_max_output_tokens", Message: "evaluation answers must be capped between 64 and 32,000 tokens"}
	}
	if r.RetentionDays < 0 || r.RetentionDays > 3650 {
		return &ValidationError{Field: "retention_days", Message: "retention must be between 0 (keep) and 3650 days"}
	}
	return nil
}

// RoutingSettings returns the stored settings, or the defaults.
func (s *Store) RoutingSettings(ctx context.Context) (RoutingSettings, error) {
	raw, updated, err := s.systemSetting(ctx, SettingRouting)
	if errors.Is(err, ErrNotFound) {
		return DefaultRoutingSettings(), nil
	}
	if err != nil {
		return RoutingSettings{}, err
	}
	out := DefaultRoutingSettings()
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return RoutingSettings{}, fmt.Errorf("decode %s setting: %w", SettingRouting, err)
	}
	out.UpdatedAt = updated
	return out, nil
}

// SetRoutingSettings validates and stores the settings.
func (s *Store) SetRoutingSettings(ctx context.Context, r RoutingSettings) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.ClassifierModelID != "" {
		m, err := s.ModelByID(ctx, r.ClassifierModelID)
		if errors.Is(err, ErrNotFound) {
			return &ValidationError{Field: "classifier_model_id", Message: "that classifier model does not exist"}
		}
		if err != nil {
			return err
		}
		if m.ClassifierRole != ClassifierRoleDecision {
			return &ValidationError{Field: "classifier_model_id", Message: "routing needs a decision model (role: decision); assign the role on the Models page first"}
		}
	}
	if r.EvalJudgeModelID != "" {
		if err := s.assertJudgeModel(ctx, r.EvalJudgeModelID, "eval_judge_model_id"); err != nil {
			return err
		}
	}
	r.UpdatedAt = time.Time{}
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.putSystemSetting(ctx, SettingRouting, string(encoded))
}

// --- Routers -----------------------------------------------------------------

// Router modes.
const (
	// RouterModeShadow serves every request from the default and records,
	// in the background, which route WOULD have served. Zero added latency.
	RouterModeShadow = "shadow"
	// RouterModeLive routes.
	RouterModeLive = "live"
)

// Thinking settings for a route.
const (
	// ThinkingPassthrough leaves whatever the caller asked for.
	ThinkingPassthrough = "passthrough"
	// ThinkingScale picks low / medium / high from the request's difficulty.
	ThinkingScale = "scale"
)

// ThinkingLevels are the reasoning efforts a route may force, least first.
var ThinkingLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Route reasons recorded on decisions and the request log.
const (
	RouteReasonMatched       = "matched"
	RouteReasonSticky        = "sticky"
	RouteReasonNoMatch       = "no_match"
	RouteReasonLowConfidence = "low_confidence"
	RouteReasonTimeout       = "timeout"
	RouteReasonError         = "classifier_error"
	RouteReasonBusy          = "busy"
	RouteReasonNoClassifier  = "no_classifier"
	RouteReasonNoText        = "no_text"
	RouteReasonShadow        = "shadow"
	RouteReasonDefault       = "default_only"
	// RouteReasonUnavailable: the chosen route could not serve (its model
	// was removed, or every member of its pool is out of service), so the
	// default served instead.
	RouteReasonUnavailable = "route_unavailable"
	// RouteReasonOverQuota: the caller was already over a quota that
	// applies whichever route serves, so no decision was made.
	RouteReasonOverQuota = "over_quota"
)

// RouteEval is one route's evaluation configuration.
type RouteEval struct {
	Enabled bool `json:"enabled"`
	// SamplePct is the percentage of requests this route serves that are
	// compared against the default.
	SamplePct float64 `json:"sample_pct"`
	// MaxPerDay caps evaluations of this route per UTC day.
	MaxPerDay int `json:"max_per_day"`
	// DailyBudgetUSD caps what evaluating this route may spend per UTC day
	// (the default model's answer plus the judge calls).
	DailyBudgetUSD float64 `json:"daily_budget_usd"`
	// Until stops evaluation after this time (zero = no end date).
	Until time.Time `json:"until"`
	// JudgeModelID overrides the routing-wide judge.
	JudgeModelID string `json:"judge_model_id"`
}

type routeEvalWire struct {
	Enabled        bool    `json:"enabled"`
	SamplePct      float64 `json:"sample_pct"`
	MaxPerDay      int     `json:"max_per_day"`
	DailyBudgetUSD float64 `json:"daily_budget_usd"`
	Until          string  `json:"until"`
	JudgeModelID   string  `json:"judge_model_id"`
}

// MarshalJSON writes an unset end date as "" (not year 1), so the admin UI
// can show "no end date" and send the value back unchanged.
func (e RouteEval) MarshalJSON() ([]byte, error) {
	w := routeEvalWire{Enabled: e.Enabled, SamplePct: e.SamplePct, MaxPerDay: e.MaxPerDay, DailyBudgetUSD: e.DailyBudgetUSD, JudgeModelID: e.JudgeModelID}
	if !e.Until.IsZero() {
		w.Until = e.Until.UTC().Format(time.RFC3339)
	}
	return json.Marshal(w)
}

// UnmarshalJSON accepts "" (no end date), an RFC 3339 time, or a bare
// YYYY-MM-DD date (end of that UTC day).
func (e *RouteEval) UnmarshalJSON(b []byte) error {
	var w routeEvalWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*e = RouteEval{Enabled: w.Enabled, SamplePct: w.SamplePct, MaxPerDay: w.MaxPerDay, DailyBudgetUSD: w.DailyBudgetUSD, JudgeModelID: w.JudgeModelID}
	until := strings.TrimSpace(w.Until)
	if until == "" || strings.HasPrefix(until, "0001-01-01") {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, until); err == nil {
		e.Until = t.UTC()
		return nil
	}
	t, err := time.Parse("2006-01-02", until)
	if err != nil {
		return &ValidationError{Field: "until", Message: "evaluation end date must be a date like 2026-11-30"}
	}
	e.Until = t.Add(24*time.Hour - time.Second).UTC()
	return nil
}

// RouterRoute is one way a router can serve a request.
type RouterRoute struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ModelID is a catalog model or a managed model (alias or pool, never
	// another router).
	ModelID string `json:"model_id"`
	// Categories are routing category slugs; empty matches any category.
	Categories []string `json:"categories"`
	// DifficultyMin / DifficultyMax bound the request's difficulty (1-5);
	// 0 means unbounded on that side.
	DifficultyMin float64 `json:"difficulty_min"`
	DifficultyMax float64 `json:"difficulty_max"`
	// Thinking is ThinkingPassthrough, ThinkingScale or one of ThinkingLevels.
	Thinking string `json:"thinking"`
	// RestrictToGranted serves this route only to callers who also hold a
	// grant on its model (otherwise the router's own grant suffices).
	RestrictToGranted bool `json:"restrict_to_granted"`
	// SkipWhenOverQuota falls through to the next route when the caller is
	// over a quota on this route's model, rather than refusing the request.
	SkipWhenOverQuota bool      `json:"skip_when_over_quota"`
	Eval              RouteEval `json:"eval"`

	// Read-time presentation.
	ModelKind     string `json:"model_kind,omitempty"`
	ModelName     string `json:"model_name,omitempty"`
	UpstreamName  string `json:"upstream_name,omitempty"`
	ModelHosting  string `json:"model_hosting,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	Broken        bool   `json:"broken,omitempty"`
	BrokenReason  string `json:"broken_reason,omitempty"`
}

// RouterDefault is the route that serves whenever no other route fits.
type RouterDefault struct {
	ModelID  string `json:"model_id"`
	Thinking string `json:"thinking"`

	ModelKind     string `json:"model_kind,omitempty"`
	ModelName     string `json:"model_name,omitempty"`
	UpstreamName  string `json:"upstream_name,omitempty"`
	ModelHosting  string `json:"model_hosting,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	Broken        bool   `json:"broken,omitempty"`
	BrokenReason  string `json:"broken_reason,omitempty"`
}

// RouterConfig is a router's whole behaviour.
type RouterConfig struct {
	Routes  []RouterRoute `json:"routes"`
	Default RouterDefault `json:"default"`
	// BudgetMs is the most a request waits for the routing decision before
	// it is served by the default.
	BudgetMs int `json:"budget_ms"`
	// Sticky keeps a conversation on the route its first turn was given, so
	// a multi-turn task does not flip models (and lose its prompt cache)
	// mid-way. StickyMinutes is how long a conversation is remembered.
	Sticky        bool `json:"sticky"`
	StickyMinutes int  `json:"sticky_minutes"`
	// Disclose adds X-Janus-Route headers naming the route and model.
	Disclose bool `json:"disclose"`
}

// MaxRouterRoutes bounds the builder.
const MaxRouterRoutes = 32

// Router is a managed model of kind router with its configuration.
type Router struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Status is draft / shadow / live (derived from the managed model's
	// enabled flag and the router mode).
	Status     string       `json:"status"`
	Mode       string       `json:"mode"`
	Enabled    bool         `json:"enabled"`
	Config     RouterConfig `json:"config"`
	GrantCount int          `json:"grant_count"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	// Problems are configuration faults the builder shows (a route's model
	// was deleted, a category was disabled).
	Problems []string `json:"problems"`
}

// Router statuses as the admin sees them.
const (
	RouterStatusDraft  = "draft"
	RouterStatusShadow = "shadow"
	RouterStatusLive   = "live"
)

// RouterInput is a router as an admin submits it.
type RouterInput struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Status      string       `json:"status"`
	Config      RouterConfig `json:"config"`
}

func normalizeThinking(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ThinkingPassthrough, true
	}
	if v == ThinkingPassthrough || v == ThinkingScale || containsString(ThinkingLevels, v) {
		return v, true
	}
	return v, false
}

// normalizeRouterConfig validates a configuration and fills defaults.
func (s *Store) normalizeRouterConfig(ctx context.Context, c RouterConfig) (RouterConfig, error) {
	if c.BudgetMs == 0 {
		c.BudgetMs = 800
	}
	if c.BudgetMs < 50 || c.BudgetMs > 30000 {
		return c, &ValidationError{Field: "budget_ms", Message: "the decision time budget must be between 50 ms and 30 s"}
	}
	if c.StickyMinutes == 0 {
		c.StickyMinutes = 30
	}
	if c.StickyMinutes < 1 || c.StickyMinutes > 1440 {
		return c, &ValidationError{Field: "sticky_minutes", Message: "conversations can be remembered for 1 minute to 24 hours"}
	}
	if len(c.Routes) > MaxRouterRoutes {
		return c, &ValidationError{Field: "routes", Message: fmt.Sprintf("a router can have at most %d routes", MaxRouterRoutes)}
	}
	if err := s.assertRouteTarget(ctx, c.Default.ModelID, "default"); err != nil {
		return c, err
	}
	var ok bool
	if c.Default.Thinking, ok = normalizeThinking(c.Default.Thinking); !ok {
		return c, &ValidationError{Field: "default", Message: "unknown thinking setting " + c.Default.Thinking}
	}
	cats, err := s.ListRoutingCategories(ctx)
	if err != nil {
		return c, err
	}
	enabled := map[string]bool{}
	for _, cat := range cats {
		if cat.Enabled {
			enabled[cat.Slug] = true
		}
	}
	ids := map[string]bool{}
	routes := make([]RouterRoute, 0, len(c.Routes))
	for i, r := range c.Routes {
		field := fmt.Sprintf("routes[%d]", i)
		r.ID = strings.TrimSpace(r.ID)
		if r.ID == "" || ids[r.ID] {
			r.ID = NewID()[:12]
		}
		ids[r.ID] = true
		r.Name = strings.TrimSpace(r.Name)
		if len(r.Name) > 80 {
			return c, &ValidationError{Field: field, Message: "route names are limited to 80 characters"}
		}
		if err := s.assertRouteTarget(ctx, r.ModelID, field); err != nil {
			return c, err
		}
		seen := map[string]bool{}
		cs := []string{}
		for _, slug := range r.Categories {
			slug = strings.TrimSpace(slug)
			if slug == "" || seen[slug] {
				continue
			}
			if !enabled[slug] {
				return c, &ValidationError{Field: field, Message: fmt.Sprintf("route %d uses category %q, which does not exist or is disabled", i+1, slug)}
			}
			seen[slug] = true
			cs = append(cs, slug)
		}
		r.Categories = cs
		for _, v := range []float64{r.DifficultyMin, r.DifficultyMax} {
			if math.IsNaN(v) || v < 0 || v > 5 {
				return c, &ValidationError{Field: field, Message: "difficulty bounds are on a 1-5 scale (0 = unbounded)"}
			}
		}
		if r.DifficultyMin > 0 && r.DifficultyMax > 0 && r.DifficultyMin > r.DifficultyMax {
			return c, &ValidationError{Field: field, Message: fmt.Sprintf("route %d's minimum difficulty is above its maximum", i+1)}
		}
		if r.Thinking, ok = normalizeThinking(r.Thinking); !ok {
			return c, &ValidationError{Field: field, Message: "unknown thinking setting " + r.Thinking}
		}
		if r.Eval.Enabled {
			if r.Eval.SamplePct <= 0 {
				r.Eval.SamplePct = 5
			}
			if r.Eval.SamplePct > 100 {
				return c, &ValidationError{Field: field, Message: "evaluation sample must be between 0 and 100 percent"}
			}
			if r.Eval.MaxPerDay == 0 {
				r.Eval.MaxPerDay = 200
			}
			if r.Eval.MaxPerDay < 1 || r.Eval.MaxPerDay > 100000 {
				return c, &ValidationError{Field: field, Message: "evaluations per day must be between 1 and 100,000"}
			}
			if r.Eval.DailyBudgetUSD == 0 {
				r.Eval.DailyBudgetUSD = 5
			}
			if r.Eval.DailyBudgetUSD < 0 || r.Eval.DailyBudgetUSD > 10000 {
				return c, &ValidationError{Field: field, Message: "the daily evaluation budget must be between $0 and $10,000"}
			}
			if r.Eval.Until.IsZero() {
				r.Eval.Until = nowUTC().Add(14 * 24 * time.Hour).Truncate(time.Second)
			}
			if r.Eval.JudgeModelID != "" {
				if err := s.assertJudgeModel(ctx, r.Eval.JudgeModelID, field); err != nil {
					return c, err
				}
			}
		}
		if r.ModelID == c.Default.ModelID && r.Thinking == c.Default.Thinking {
			return c, &ValidationError{Field: field, Message: fmt.Sprintf("route %d sends requests to the default model with the default thinking setting, so it changes nothing; pick another model or thinking level", i+1)}
		}
		r.ModelKind, r.ModelName, r.UpstreamName, r.Broken, r.BrokenReason = "", "", "", false, ""
		routes = append(routes, r)
	}
	c.Routes = routes
	c.Default.ModelKind, c.Default.ModelName, c.Default.UpstreamName, c.Default.Broken, c.Default.BrokenReason = "", "", "", false, ""
	return c, nil
}

// assertJudgeModel checks an evaluation judge: an enabled catalog model
// that is not a classifier (disabling a model is the admin's off-switch
// for its spend, and classifiers cannot answer chat requests).
func (s *Store) assertJudgeModel(ctx context.Context, id, field string) error {
	m, err := s.ModelByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return &ValidationError{Field: field, Message: "that judge model does not exist"}
	}
	if err != nil {
		return err
	}
	if m.ClassifierRole != "" {
		return &ValidationError{Field: field, Message: m.PublicName() + " is a classifier and cannot judge answers"}
	}
	if m.Status != ModelEnabled {
		return &ValidationError{Field: field, Message: m.PublicName() + " is not enabled; enable it on the Models page or pick another judge"}
	}
	return nil
}

// assertRouteTarget checks a route's model: an existing catalog model or a
// managed alias/pool. Routers never point at routers (no chains).
func (s *Store) assertRouteTarget(ctx context.Context, id, field string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		if field == "default" {
			return &ValidationError{Field: field, Message: "choose the default model: it serves whenever no route fits"}
		}
		return &ValidationError{Field: field, Message: "every route needs a model"}
	}
	if mm, err := s.ManagedModelByID(ctx, id); err == nil {
		if mm.Kind == ManagedKindRouter {
			return &ValidationError{Field: field, Message: "a route cannot point at another router"}
		}
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	m, err := s.ModelByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return &ValidationError{Field: field, Message: "one of the chosen models does not exist"}
	}
	if err != nil {
		return err
	}
	if m.ClassifierRole != "" {
		return &ValidationError{Field: field, Message: m.PublicName() + " is a classifier and cannot serve requests"}
	}
	return nil
}

func routerStatus(enabled bool, mode string) string {
	if !enabled {
		return RouterStatusDraft
	}
	if mode == RouterModeLive {
		return RouterStatusLive
	}
	return RouterStatusShadow
}

func statusToFlags(status string) (bool, string, error) {
	switch status {
	case RouterStatusDraft, "":
		return false, RouterModeShadow, nil
	case RouterStatusShadow:
		return true, RouterModeShadow, nil
	case RouterStatusLive:
		return true, RouterModeLive, nil
	}
	return false, "", &ValidationError{Field: "status", Message: "status must be draft, shadow or live"}
}

// CreateRouter creates a router (a managed model of kind router).
func (s *Store) CreateRouter(ctx context.Context, in RouterInput, createdBy string) (*Router, error) {
	name, err := normalizeManagedModelName(in.Name)
	if err != nil {
		return nil, err
	}
	if len(in.Description) > 500 {
		return nil, &ValidationError{Field: "description", Message: "descriptions are limited to 500 characters"}
	}
	enabled, mode, err := statusToFlags(in.Status)
	if err != nil {
		return nil, err
	}
	cfg, err := s.normalizeRouterConfig(ctx, in.Config)
	if err != nil {
		return nil, err
	}
	if err := s.assertManagedNameAvailable(ctx, name, ""); err != nil {
		return nil, err
	}
	id := NewID()
	now := nowUTC()
	status := ManagedModelDisabled
	if enabled {
		status = ManagedModelEnabled
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	target := s.routeTargetModelID(ctx, cfg.Default.ModelID)
	err = s.modelTx(ctx, func(tx *Store) error {
		if err := tx.exec(ctx, `INSERT INTO managed_model (`+managedModelColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, name, strings.TrimSpace(in.Description), target, status, createdBy, FormatTime(now), FormatTime(now), "", "",
			PoolPolicyFailover, PoolAffinityBounded, DefaultPoolSpillPct, ManagedKindRouter); err != nil {
			return err
		}
		if err := tx.replaceSingleMember(ctx, tx.tx, id, target); err != nil {
			return err
		}
		return tx.exec(ctx, `INSERT INTO routing_router (managed_model_id, mode, config, updated_at) VALUES (?,?,?,?)`,
			id, mode, string(encoded), FormatTime(now))
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrManagedModelNameTaken
		}
		return nil, err
	}
	return s.RouterByID(ctx, id)
}

// routeTargetModelID returns the catalog model a route target serves from
// today (the alias's primary for a managed model). It keeps
// managed_model.target_model_id meaningful for pre-router readers.
func (s *Store) routeTargetModelID(ctx context.Context, id string) string {
	if mm, err := s.ManagedModelByID(ctx, id); err == nil {
		return mm.TargetModelID
	}
	return id
}

// UpdateRouter replaces a router's name, description, status and
// configuration.
func (s *Store) UpdateRouter(ctx context.Context, id string, in RouterInput) (*Router, error) {
	current, err := s.RouterByID(ctx, id)
	if err != nil {
		return nil, err
	}
	name, err := normalizeManagedModelName(in.Name)
	if err != nil {
		return nil, err
	}
	if len(in.Description) > 500 {
		return nil, &ValidationError{Field: "description", Message: "descriptions are limited to 500 characters"}
	}
	enabled, mode, err := statusToFlags(in.Status)
	if err != nil {
		return nil, err
	}
	cfg, err := s.normalizeRouterConfig(ctx, in.Config)
	if err != nil {
		return nil, err
	}
	if err := s.assertManagedNameAvailable(ctx, name, current.ID); err != nil {
		return nil, err
	}
	status := ManagedModelDisabled
	if enabled {
		status = ManagedModelEnabled
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	now := FormatTime(nowUTC())
	target := s.routeTargetModelID(ctx, cfg.Default.ModelID)
	err = s.modelTx(ctx, func(tx *Store) error {
		if err := tx.exec(ctx, `UPDATE managed_model SET name = ?, description = ?, status = ?, target_model_id = ?, updated_at = ? WHERE id = ? AND kind = ?`,
			name, strings.TrimSpace(in.Description), status, target, now, id, ManagedKindRouter); err != nil {
			return err
		}
		if err := tx.replaceSingleMember(ctx, tx.tx, id, target); err != nil {
			return err
		}
		return tx.exec(ctx, `UPDATE routing_router SET mode = ?, config = ?, updated_at = ? WHERE managed_model_id = ?`, mode, string(encoded), now, id)
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrManagedModelNameTaken
		}
		return nil, err
	}
	return s.RouterByID(ctx, id)
}

// DeleteRouter removes a router, its grants and its sticky conversations.
// Decisions and evaluation verdicts are history and stay until retention.
func (s *Store) DeleteRouter(ctx context.Context, id string) error {
	if _, err := s.RouterByID(ctx, id); err != nil {
		return err
	}
	if err := s.DeleteManagedModel(ctx, id); err != nil {
		return err
	}
	return s.modelTx(ctx, func(tx *Store) error {
		if err := tx.exec(ctx, `DELETE FROM routing_router WHERE managed_model_id = ?`, id); err != nil {
			return err
		}
		return tx.exec(ctx, `DELETE FROM routing_sticky WHERE router_id = ?`, id)
	})
}

// RouterByID loads one router with its routes' models resolved.
func (s *Store) RouterByID(ctx context.Context, id string) (*Router, error) {
	mm, err := s.ManagedModelByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if mm.Kind != ManagedKindRouter {
		return nil, ErrNotFound
	}
	return s.routerFromManaged(ctx, mm)
}

// RouterConfigFor loads only a router's mode and configuration, for the
// proxy's hot path (cached by the caller). No presentation is resolved.
func (s *Store) RouterConfigFor(ctx context.Context, id string) (string, RouterConfig, error) {
	var mode, raw string
	err := s.queryRow(ctx, `SELECT mode, config FROM routing_router WHERE managed_model_id = ?`, id).Scan(&mode, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", RouterConfig{}, ErrNotFound
	}
	if err != nil {
		return "", RouterConfig{}, err
	}
	var cfg RouterConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", RouterConfig{}, fmt.Errorf("decode router config: %w", err)
	}
	return mode, cfg, nil
}

func (s *Store) routerFromManaged(ctx context.Context, mm *ManagedModel) (*Router, error) {
	mode, cfg, err := s.RouterConfigFor(ctx, mm.ID)
	if errors.Is(err, ErrNotFound) {
		mode, cfg = RouterModeShadow, RouterConfig{Default: RouterDefault{ModelID: mm.TargetModelID}}
	} else if err != nil {
		return nil, err
	}
	r := &Router{ID: mm.ID, Name: mm.Name, Description: mm.Description, Mode: mode, Enabled: mm.Status == ManagedModelEnabled,
		Config: cfg, GrantCount: mm.GrantCount, CreatedAt: mm.CreatedAt, UpdatedAt: mm.UpdatedAt, Problems: []string{}}
	r.Status = routerStatus(r.Enabled, mode)
	if r.Config.Routes == nil {
		r.Config.Routes = []RouterRoute{}
	}
	cats, err := s.ListRoutingCategories(ctx)
	if err != nil {
		return nil, err
	}
	enabled := map[string]bool{}
	for _, c := range cats {
		if c.Enabled {
			enabled[c.Slug] = true
		}
	}
	hosting := s.modelHostingMap(ctx)
	describe := func(id string) (kind, name, upstream, host string, window int64, broken bool, reason string) {
		if m, err := s.ManagedModelByID(ctx, id); err == nil {
			host = hosting[m.TargetModelID]
			if m.Kind == ManagedKindRouter {
				return ModelKindManaged, m.Name, "", host, 0, true, "points at another router"
			}
			return ModelKindManaged, m.Name, m.TargetUpstreamName, host, m.ContextWindow, !m.Servable, m.BrokenReason
		}
		m, err := s.ModelByID(ctx, id)
		if err != nil {
			return ModelKindModel, id, "", "", 0, true, "the model no longer exists"
		}
		if m.Status != ModelEnabled {
			return ModelKindModel, m.PublicName(), m.UpstreamName, hosting[m.ID], m.ContextWindow, true, "the model is " + m.Status
		}
		return ModelKindModel, m.PublicName(), m.UpstreamName, hosting[m.ID], m.ContextWindow, false, ""
	}
	d := &r.Config.Default
	d.ModelKind, d.ModelName, d.UpstreamName, d.ModelHosting, d.ContextWindow, d.Broken, d.BrokenReason = describe(d.ModelID)
	if d.Broken {
		r.Problems = append(r.Problems, "Default model "+d.ModelName+": "+d.BrokenReason+".")
	}
	for i := range r.Config.Routes {
		rt := &r.Config.Routes[i]
		rt.ModelKind, rt.ModelName, rt.UpstreamName, rt.ModelHosting, rt.ContextWindow, rt.Broken, rt.BrokenReason = describe(rt.ModelID)
		if rt.Categories == nil {
			rt.Categories = []string{}
		}
		if rt.Broken {
			r.Problems = append(r.Problems, fmt.Sprintf("Route %d (%s): %s; it is skipped.", i+1, rt.ModelName, rt.BrokenReason))
		}
		for _, c := range rt.Categories {
			if !enabled[c] {
				r.Problems = append(r.Problems, fmt.Sprintf("Route %d uses category %q, which is disabled or deleted; that category never matches.", i+1, c))
			}
		}
	}
	return r, nil
}

// modelHostingMap reads the admin's per-model hosting classification
// (self_hosted / external / hybrid), used to warn when a hosted classifier
// reads prompts that are routed on-prem.
func (s *Store) modelHostingMap(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := s.query(ctx, `SELECT model_id, hosting FROM report_model_classification`)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, h string
		if rows.Scan(&id, &h) == nil {
			out[id] = h
		}
	}
	return out
}

// ListRouters returns every router.
func (s *Store) ListRouters(ctx context.Context) ([]*Router, error) {
	all, err := s.ListManagedModels(ctx, ManagedModelFilter{Kind: ManagedKindRouter})
	if err != nil {
		return nil, err
	}
	out := []*Router{}
	for _, mm := range all {
		r, err := s.routerFromManaged(ctx, mm)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// RoutersUsingModel names the routers with a route (or default) on a model,
// for blast-radius warnings before a model or alias is disabled or deleted.
func (s *Store) RoutersUsingModel(ctx context.Context, modelID string) ([]string, error) {
	routers, err := s.ListRouters(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range routers {
		uses := r.Config.Default.ModelID == modelID
		for _, rt := range r.Config.Routes {
			uses = uses || rt.ModelID == modelID
		}
		if uses {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// --- Decisions ---------------------------------------------------------------

// RoutingDecision is one routing outcome. It holds labels and timing only.
type RoutingDecision struct {
	ID             string    `json:"id"`
	CreatedAt      time.Time `json:"created_at"`
	RequestID      string    `json:"request_id"`
	RouterID       string    `json:"router_id"`
	RouterName     string    `json:"router_name,omitempty"`
	Mode           string    `json:"mode"`
	RouteID        string    `json:"route_id"`
	RouteName      string    `json:"route_name"`
	ModelID        string    `json:"model_id"`
	ModelName      string    `json:"model_name"`
	Category       string    `json:"category"`
	Confidence     float64   `json:"confidence"`
	Difficulty     float64   `json:"difficulty"`
	Thinking       string    `json:"thinking"`
	Reason         string    `json:"reason"`
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
	DecisionMs     int       `json:"decision_ms"`
	Calls          int       `json:"calls"`
	UserID         string    `json:"user_id,omitempty"`
	ServiceTokenID string    `json:"service_token_id,omitempty"`
}

const routingDecisionColumns = `id, created_at, request_id, router_id, mode, route_id, route_name, model_id, model_name, category, confidence, difficulty, thinking, reason, status, error, decision_ms, calls, user_id, service_token_id`

// InsertRoutingDecision records one decision.
func (s *Store) InsertRoutingDecision(ctx context.Context, d *RoutingDecision) error {
	if d.ID == "" {
		d.ID = NewID()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = nowUTC()
	}
	d.Error = truncateUTF8(d.Error, 600)
	return s.exec(ctx, `INSERT INTO routing_decision (`+routingDecisionColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, FormatTime(d.CreatedAt), d.RequestID, d.RouterID, d.Mode, d.RouteID, d.RouteName, d.ModelID, d.ModelName, d.Category,
		d.Confidence, d.Difficulty, d.Thinking, d.Reason, d.Status, d.Error, d.DecisionMs, d.Calls, d.UserID, d.ServiceTokenID)
}

// RoutingDecisionFilter narrows a decision listing.
type RoutingDecisionFilter struct {
	From, To  time.Time
	RouterID  string
	Reason    string
	RequestID string
	Limit     int
}

// ListRoutingDecisions returns recent decisions, newest first.
func (s *Store) ListRoutingDecisions(ctx context.Context, f RoutingDecisionFilter) ([]RoutingDecision, error) {
	where := []string{"1=1"}
	args := []any{}
	if !f.From.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, FormatTime(f.From))
	}
	if !f.To.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, FormatTime(f.To))
	}
	if f.RouterID != "" {
		where = append(where, "router_id = ?")
		args = append(args, f.RouterID)
	}
	if f.Reason != "" {
		where = append(where, "reason = ?")
		args = append(args, f.Reason)
	}
	if f.RequestID != "" {
		where = append(where, "request_id = ?")
		args = append(args, f.RequestID)
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.query(ctx, `SELECT `+routingDecisionColumns+` FROM routing_decision WHERE `+strings.Join(where, " AND ")+
		fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, f.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RoutingDecision{}
	for rows.Next() {
		var d RoutingDecision
		var created string
		if err := rows.Scan(&d.ID, &created, &d.RequestID, &d.RouterID, &d.Mode, &d.RouteID, &d.RouteName, &d.ModelID, &d.ModelName,
			&d.Category, &d.Confidence, &d.Difficulty, &d.Thinking, &d.Reason, &d.Status, &d.Error, &d.DecisionMs, &d.Calls,
			&d.UserID, &d.ServiceTokenID); err != nil {
			return nil, err
		}
		d.CreatedAt = ParseTime(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RouteMixRow is one route's share of a router's decisions.
type RouteMixRow struct {
	RouteID   string `json:"route_id"`
	RouteName string `json:"route_name"`
	ModelName string `json:"model_name"`
	Requests  int64  `json:"requests"`
}

// ReasonRow counts decisions by reason.
type ReasonRow struct {
	Reason   string `json:"reason"`
	Requests int64  `json:"requests"`
}

// RouterStats summarises one router's decisions in a window.
type RouterStats struct {
	RouterID   string        `json:"router_id"`
	Requests   int64         `json:"requests"`
	Routed     int64         `json:"routed"`
	Defaulted  int64         `json:"defaulted"`
	Timeouts   int64         `json:"timeouts"`
	Errors     int64         `json:"errors"`
	Shadow     int64         `json:"shadow"`
	P50Ms      int           `json:"p50_ms"`
	P95Ms      int           `json:"p95_ms"`
	Mix        []RouteMixRow `json:"mix"`
	Reasons    []ReasonRow   `json:"reasons"`
	Categories []ReasonRow   `json:"categories"`
}

// RoutingStats summarises every router's decisions in [from, to).
func (s *Store) RoutingStats(ctx context.Context, from, to time.Time) (map[string]*RouterStats, error) {
	out := map[string]*RouterStats{}
	get := func(id string) *RouterStats {
		st := out[id]
		if st == nil {
			st = &RouterStats{RouterID: id, Mix: []RouteMixRow{}, Reasons: []ReasonRow{}, Categories: []ReasonRow{}}
			out[id] = st
		}
		return st
	}
	rows, err := s.query(ctx, `SELECT router_id, route_id, route_name, model_name, COUNT(*) FROM routing_decision
		WHERE created_at >= ? AND created_at < ? GROUP BY router_id, route_id, route_name, model_name`, FormatTime(from), FormatTime(to))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var r RouteMixRow
		if err := rows.Scan(&id, &r.RouteID, &r.RouteName, &r.ModelName, &r.Requests); err != nil {
			_ = rows.Close()
			return nil, err
		}
		st := get(id)
		st.Mix = append(st.Mix, r)
		st.Requests += r.Requests
		if r.RouteID != "" && r.RouteID != "default" {
			st.Routed += r.Requests
		} else {
			st.Defaulted += r.Requests
		}
	}
	_ = rows.Close()
	rows, err = s.query(ctx, `SELECT router_id, reason, COUNT(*) FROM routing_decision
		WHERE created_at >= ? AND created_at < ? GROUP BY router_id, reason`, FormatTime(from), FormatTime(to))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var r ReasonRow
		if err := rows.Scan(&id, &r.Reason, &r.Requests); err != nil {
			_ = rows.Close()
			return nil, err
		}
		st := get(id)
		st.Reasons = append(st.Reasons, r)
		switch r.Reason {
		case RouteReasonTimeout:
			st.Timeouts += r.Requests
		case RouteReasonError:
			st.Errors += r.Requests
		}
	}
	_ = rows.Close()
	rows, err = s.query(ctx, `SELECT router_id, mode, category, COUNT(*) FROM routing_decision
		WHERE created_at >= ? AND created_at < ? GROUP BY router_id, mode, category`, FormatTime(from), FormatTime(to))
	if err != nil {
		return nil, err
	}
	cats := map[string]map[string]int64{}
	for rows.Next() {
		var id, mode, cat string
		var n int64
		if err := rows.Scan(&id, &mode, &cat, &n); err != nil {
			_ = rows.Close()
			return nil, err
		}
		st := get(id)
		if mode == RouterModeShadow {
			st.Shadow += n
		}
		if cats[id] == nil {
			cats[id] = map[string]int64{}
		}
		cats[id][cat] += n
	}
	_ = rows.Close()
	for id, m := range cats {
		st := get(id)
		for c, n := range m {
			st.Categories = append(st.Categories, ReasonRow{Reason: c, Requests: n})
		}
		sort.Slice(st.Categories, func(i, j int) bool { return st.Categories[i].Requests > st.Categories[j].Requests })
	}
	for id, st := range out {
		var lat []int
		r, err := s.query(ctx, `SELECT decision_ms FROM routing_decision WHERE router_id = ? AND created_at >= ? AND created_at < ? AND calls > 0 ORDER BY created_at DESC LIMIT 5000`,
			id, FormatTime(from), FormatTime(to))
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var ms int
			if r.Scan(&ms) == nil {
				lat = append(lat, ms)
			}
		}
		_ = r.Close()
		sort.Ints(lat)
		st.P50Ms, st.P95Ms = percentile(lat, 0.5), percentile(lat, 0.95)
		sort.Slice(st.Mix, func(i, j int) bool { return st.Mix[i].Requests > st.Mix[j].Requests })
		sort.Slice(st.Reasons, func(i, j int) bool { return st.Reasons[i].Requests > st.Reasons[j].Requests })
	}
	return out, nil
}

// --- Sticky conversations ----------------------------------------------------

// StickyRoute is a conversation's remembered route.
type StickyRoute struct {
	Key        string
	RouterID   string
	RouteID    string
	Category   string
	Difficulty float64
	ExpiresAt  time.Time
}

// LookupStickyRoute returns a live remembered route.
func (s *Store) LookupStickyRoute(ctx context.Context, key string, now time.Time) (*StickyRoute, error) {
	var st StickyRoute
	var exp string
	err := s.queryRow(ctx, `SELECT key, router_id, route_id, category, difficulty, expires_at FROM routing_sticky WHERE key = ? AND expires_at > ?`,
		key, FormatTime(now)).Scan(&st.Key, &st.RouterID, &st.RouteID, &st.Category, &st.Difficulty, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	st.ExpiresAt = ParseTime(exp)
	return &st, nil
}

// PutStickyRoute remembers (or extends) a conversation's route.
func (s *Store) PutStickyRoute(ctx context.Context, st StickyRoute) error {
	return s.exec(ctx, `INSERT INTO routing_sticky (key, router_id, route_id, category, difficulty, expires_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT (key) DO UPDATE SET route_id = excluded.route_id, category = excluded.category, difficulty = excluded.difficulty, expires_at = excluded.expires_at`,
		st.Key, st.RouterID, st.RouteID, st.Category, st.Difficulty, FormatTime(st.ExpiresAt))
}

// RoutingPruned counts what PruneRouting removed.
type RoutingPruned struct {
	Sticky     int64
	Decisions  int64
	Evals      int64
	Anonymised int64
}

// PruneRouting enforces auto-routing retention:
//   - expired sticky conversations (older than now) are deleted;
//   - decisions and evaluation verdicts created before historyBefore are
//     deleted (zero historyBefore keeps them: "Keep decisions" = 0);
//   - decisions created before identitiesBefore lose their user and
//     service-token ids, so routing history never names a caller for
//     longer than the usage log it mirrors (zero keeps them).
func (s *Store) PruneRouting(ctx context.Context, now, historyBefore, identitiesBefore time.Time) (RoutingPruned, error) {
	var out RoutingPruned
	var err error
	if out.Sticky, err = s.execCount(ctx, `DELETE FROM routing_sticky WHERE expires_at <= ?`, FormatTime(now)); err != nil {
		return out, fmt.Errorf("prune routing sticky: %w", err)
	}
	if !historyBefore.IsZero() {
		if out.Decisions, err = s.execCount(ctx, `DELETE FROM routing_decision WHERE created_at < ?`, FormatTime(historyBefore)); err != nil {
			return out, fmt.Errorf("prune routing decisions: %w", err)
		}
		if out.Evals, err = s.execCount(ctx, `DELETE FROM routing_eval_result WHERE created_at < ?`, FormatTime(historyBefore)); err != nil {
			return out, fmt.Errorf("prune routing evaluations: %w", err)
		}
	}
	if !identitiesBefore.IsZero() {
		if out.Anonymised, err = s.execCount(ctx, `UPDATE routing_decision SET user_id = '', service_token_id = ''
			WHERE created_at < ? AND (user_id <> '' OR service_token_id <> '')`, FormatTime(identitiesBefore)); err != nil {
			return out, fmt.Errorf("anonymise routing decisions: %w", err)
		}
	}
	return out, nil
}

// execCount runs a statement and reports the rows it affected.
func (s *Store) execCount(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(query), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Evaluations -------------------------------------------------------------

// Evaluation verdicts, from the routed model's point of view.
const (
	EvalVerdictBetter = "better"
	EvalVerdictTie    = "tie"
	EvalVerdictWorse  = "worse"
)

// RoutingEvalResult is one judged comparison. No answer text is kept.
type RoutingEvalResult struct {
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	RequestID        string    `json:"request_id"`
	RouterID         string    `json:"router_id"`
	RouteID          string    `json:"route_id"`
	RoutedModelID    string    `json:"routed_model_id"`
	DefaultModelID   string    `json:"default_model_id"`
	JudgeModelID     string    `json:"judge_model_id"`
	Verdict          string    `json:"verdict"`
	RoutedScore      float64   `json:"routed_score"`
	DefaultScore     float64   `json:"default_score"`
	Consistent       bool      `json:"consistent"`
	Rationale        string    `json:"rationale"`
	Status           string    `json:"status"`
	Error            string    `json:"error,omitempty"`
	RoutedCostNano   int64     `json:"routed_cost_nanousd"`
	DefaultCostNano  int64     `json:"default_cost_nanousd"`
	EvalCostNano     int64     `json:"eval_cost_nanousd"`
	RoutedLatencyMs  int       `json:"routed_latency_ms"`
	DefaultLatencyMs int       `json:"default_latency_ms"`
}

const routingEvalColumns = `id, created_at, request_id, router_id, route_id, routed_model_id, default_model_id, judge_model_id, verdict, routed_score, default_score, consistent, rationale, status, error, routed_cost_nanousd, default_cost_nanousd, eval_cost_nanousd, routed_latency_ms, default_latency_ms`

// InsertRoutingEval records one evaluation.
func (s *Store) InsertRoutingEval(ctx context.Context, e *RoutingEvalResult) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = nowUTC()
	}
	e.clean()
	return s.exec(ctx, `INSERT INTO routing_eval_result (`+routingEvalColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, FormatTime(e.CreatedAt), e.RequestID, e.RouterID, e.RouteID, e.RoutedModelID, e.DefaultModelID, e.JudgeModelID, e.Verdict,
		e.RoutedScore, e.DefaultScore, boolInt(e.Consistent), e.Rationale, e.Status, e.Error, e.RoutedCostNano, e.DefaultCostNano,
		e.EvalCostNano, e.RoutedLatencyMs, e.DefaultLatencyMs)
}

// clean bounds the free-text fields on character boundaries: PostgreSQL
// refuses invalid UTF-8, and a lost row would also lose its spend from the
// daily budget.
func (e *RoutingEvalResult) clean() {
	e.Rationale = truncateUTF8(e.Rationale, 600)
	e.Error = truncateUTF8(e.Error, 600)
}

// Evaluation result statuses besides "ok" and "error".
const (
	// EvalStatusRunning marks a reservation: the evaluation was admitted
	// against the route's daily limits and is in progress. Its
	// eval_cost_nanousd holds the estimated cost until it finishes.
	EvalStatusRunning = "running"
	// EvalStatusSkipped marks a sampled request that was not evaluated
	// because a Security Gateway policy on the default model or the judge
	// would have blocked or redacted the out-of-band call.
	EvalStatusSkipped = "skipped"
)

// evalReserveMu serialises reservations on this replica; Postgres also takes
// a transaction-scoped advisory lock so replicas serialise with each other.
var evalReserveMu sync.Mutex

// ReserveRouteEval admits one evaluation against a route's daily limits
// (UTC day) atomically: it counts today's results and reservations, adds
// what they spent (in-flight ones at their estimate) plus this one's
// estimate, and only if both limits hold records e as a running
// reservation. It reports whether the evaluation may run. Finish it with
// FinishRoutingEval.
func (s *Store) ReserveRouteEval(ctx context.Context, e *RoutingEvalResult, maxPerDay int, budgetNano, estimateNano int64, now time.Time) (bool, error) {
	evalReserveMu.Lock()
	defer evalReserveMu.Unlock()
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now.UTC()
	}
	e.Status, e.EvalCostNano = EvalStatusRunning, max(estimateNano, 0)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	admitted := false
	err := s.writeTx(ctx, func(tx *Store) error {
		if tx.dialect == DialectPostgres {
			if err := tx.exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(?))`, "janus.routing_eval|"+e.RouterID+"|"+e.RouteID); err != nil {
				return err
			}
		}
		var n int
		var cost sql.NullInt64
		if err := tx.queryRow(ctx, `SELECT COUNT(*), SUM(eval_cost_nanousd) FROM routing_eval_result WHERE router_id = ? AND route_id = ? AND created_at >= ? AND status <> ?`,
			e.RouterID, e.RouteID, FormatTime(day), EvalStatusSkipped).Scan(&n, &cost); err != nil {
			return err
		}
		if n >= maxPerDay || cost.Int64+e.EvalCostNano > budgetNano {
			return nil
		}
		admitted = true
		return tx.InsertRoutingEval(ctx, e)
	})
	return admitted, err
}

// FinishRoutingEval writes the outcome of a reserved evaluation over its
// reservation (actual cost replaces the estimate).
func (s *Store) FinishRoutingEval(ctx context.Context, e *RoutingEvalResult) error {
	e.clean()
	return s.exec(ctx, `UPDATE routing_eval_result SET routed_model_id = ?, default_model_id = ?, judge_model_id = ?, verdict = ?, routed_score = ?,
		default_score = ?, consistent = ?, rationale = ?, status = ?, error = ?, routed_cost_nanousd = ?, default_cost_nanousd = ?, eval_cost_nanousd = ?,
		routed_latency_ms = ?, default_latency_ms = ? WHERE id = ?`,
		e.RoutedModelID, e.DefaultModelID, e.JudgeModelID, e.Verdict, e.RoutedScore, e.DefaultScore, boolInt(e.Consistent), e.Rationale, e.Status,
		e.Error, e.RoutedCostNano, e.DefaultCostNano, e.EvalCostNano, e.RoutedLatencyMs, e.DefaultLatencyMs, e.ID)
}

// RouteEvalToday reports how many evaluations a route ran (or is running)
// today (UTC) and what they cost, for the per-route daily limits.
func (s *Store) RouteEvalToday(ctx context.Context, routerID, routeID string, now time.Time) (int, int64, error) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var n int
	var cost sql.NullInt64
	err := s.queryRow(ctx, `SELECT COUNT(*), SUM(eval_cost_nanousd) FROM routing_eval_result WHERE router_id = ? AND route_id = ? AND created_at >= ? AND status <> ?`,
		routerID, routeID, FormatTime(day), EvalStatusSkipped).Scan(&n, &cost)
	return n, cost.Int64, err
}

// RouteEvalSkipsToday counts today's (UTC) samples of a route that the
// Security Gateway kept from being evaluated.
func (s *Store) RouteEvalSkipsToday(ctx context.Context, routerID, routeID string, now time.Time) (int, error) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM routing_eval_result WHERE router_id = ? AND route_id = ? AND created_at >= ? AND status = ?`,
		routerID, routeID, FormatTime(day), EvalStatusSkipped).Scan(&n)
	return n, err
}

// RouteEvalPairing names the three models one evaluation compared with.
type RouteEvalPairing struct {
	RoutedModelID  string
	DefaultModelID string
	JudgeModelID   string
}

// ListRoutingEvals returns recent finished evaluations of one route, newest
// first.
func (s *Store) ListRoutingEvals(ctx context.Context, routerID, routeID string, limit int) ([]RoutingEvalResult, error) {
	return s.ListRoutingEvalsFor(ctx, routerID, routeID, nil, limit)
}

// ListRoutingEvalsFor is ListRoutingEvals narrowed to one pairing of routed
// model, default and judge (nil = every pairing).
func (s *Store) ListRoutingEvalsFor(ctx context.Context, routerID, routeID string, pair *RouteEvalPairing, limit int) ([]RoutingEvalResult, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT ` + routingEvalColumns + ` FROM routing_eval_result WHERE router_id = ? AND (? = '' OR route_id = ?) AND status <> ?`
	args := []any{routerID, routeID, routeID, EvalStatusRunning}
	if pair != nil {
		q += ` AND routed_model_id = ? AND default_model_id = ? AND judge_model_id = ?`
		args = append(args, pair.RoutedModelID, pair.DefaultModelID, pair.JudgeModelID)
	}
	rows, err := s.query(ctx, q+` ORDER BY created_at DESC LIMIT `+fmt.Sprint(limit), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RoutingEvalResult{}
	for rows.Next() {
		var e RoutingEvalResult
		var created string
		var consistent int
		if err := rows.Scan(&e.ID, &created, &e.RequestID, &e.RouterID, &e.RouteID, &e.RoutedModelID, &e.DefaultModelID, &e.JudgeModelID,
			&e.Verdict, &e.RoutedScore, &e.DefaultScore, &consistent, &e.Rationale, &e.Status, &e.Error, &e.RoutedCostNano,
			&e.DefaultCostNano, &e.EvalCostNano, &e.RoutedLatencyMs, &e.DefaultLatencyMs); err != nil {
			return nil, err
		}
		e.CreatedAt, e.Consistent = ParseTime(created), consistent == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// RouteEvalSummary is one route's evaluation tally.
//
// Tallies are kept per pairing (the route's model, the router's default and
// the judge actually used): changing any of them starts a new tally rather
// than mixing the old model's verdicts into the new one's.
type RouteEvalSummary struct {
	RouterID       string `json:"router_id"`
	RouteID        string `json:"route_id"`
	RoutedModelID  string `json:"routed_model_id"`
	DefaultModelID string `json:"default_model_id"`
	JudgeModelID   string `json:"judge_model_id"`
	// Current reports that this pairing is the one the route is configured
	// with now (set by the API layer, which knows the configuration).
	Current bool `json:"current"`
	// FirstAt / LastAt bound the pairing's results in the window.
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
	Evaluated int64     `json:"evaluated"`
	Failed    int64     `json:"failed"`
	// Skipped counts sampled requests not evaluated because a Security
	// Gateway policy would have blocked or redacted the out-of-band call.
	Skipped          int64   `json:"skipped"`
	Better           int64   `json:"better"`
	Ties             int64   `json:"ties"`
	Worse            int64   `json:"worse"`
	Consistency      float64 `json:"consistency"`
	RoutedCostNano   int64   `json:"routed_cost_nanousd"`
	DefaultCostNano  int64   `json:"default_cost_nanousd"`
	EvalCostNano     int64   `json:"eval_cost_nanousd"`
	RoutedLatencyMs  float64 `json:"routed_latency_ms"`
	DefaultLatencyMs float64 `json:"default_latency_ms"`
	// AtLeastAsGoodLow / High is the 95% Wilson interval on the share of
	// comparisons where the routed answer was at least as good (ties count
	// half).
	AtLeastAsGoodLow  float64 `json:"at_least_as_good_low"`
	AtLeastAsGoodHigh float64 `json:"at_least_as_good_high"`
	// Verdict: keep / losing / needs_data.
	Verdict string `json:"verdict"`
}

// RoutingEvalSummaries tallies every route's evaluations since from.
func (s *Store) RoutingEvalSummaries(ctx context.Context, from time.Time) ([]RouteEvalSummary, error) {
	rows, err := s.query(ctx, `SELECT router_id, route_id, routed_model_id, default_model_id, judge_model_id, created_at, status, verdict, consistent,
		routed_cost_nanousd, default_cost_nanousd, eval_cost_nanousd, routed_latency_ms, default_latency_ms
		FROM routing_eval_result WHERE created_at >= ?`, FormatTime(from))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type acc struct {
		RouteEvalSummary
		consistent int64
		rlat, dlat int64
	}
	byKey := map[string]*acc{}
	var keys []string
	for rows.Next() {
		var router, route, routed, def, judge, created, status, verdict string
		var consistent int
		var rc, dc, ec int64
		var rl, dl int
		if err := rows.Scan(&router, &route, &routed, &def, &judge, &created, &status, &verdict, &consistent, &rc, &dc, &ec, &rl, &dl); err != nil {
			return nil, err
		}
		if status == EvalStatusRunning {
			continue
		}
		k := router + "|" + route + "|" + routed + "|" + def + "|" + judge
		a := byKey[k]
		at := ParseTime(created)
		if a == nil {
			a = &acc{RouteEvalSummary: RouteEvalSummary{RouterID: router, RouteID: route, RoutedModelID: routed, DefaultModelID: def,
				JudgeModelID: judge, FirstAt: at, LastAt: at}}
			byKey[k] = a
			keys = append(keys, k)
		}
		if at.Before(a.FirstAt) {
			a.FirstAt = at
		}
		if at.After(a.LastAt) {
			a.LastAt = at
		}
		a.EvalCostNano += ec
		switch status {
		case "ok":
		case EvalStatusSkipped:
			a.Skipped++
			continue
		default:
			a.Failed++
			continue
		}
		a.Evaluated++
		a.RoutedCostNano += rc
		a.DefaultCostNano += dc
		a.rlat += int64(rl)
		a.dlat += int64(dl)
		if consistent == 1 {
			a.consistent++
		}
		switch verdict {
		case EvalVerdictBetter:
			a.Better++
		case EvalVerdictWorse:
			a.Worse++
		default:
			a.Ties++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Per route, the most recent pairing first.
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := byKey[keys[i]], byKey[keys[j]]
		if a.RouterID != b.RouterID {
			return a.RouterID < b.RouterID
		}
		if a.RouteID != b.RouteID {
			return a.RouteID < b.RouteID
		}
		return a.LastAt.After(b.LastAt)
	})
	out := make([]RouteEvalSummary, 0, len(keys))
	for _, k := range keys {
		a := byKey[k]
		if a.Evaluated > 0 {
			a.Consistency = float64(a.consistent) / float64(a.Evaluated)
			a.RoutedLatencyMs = float64(a.rlat) / float64(a.Evaluated)
			a.DefaultLatencyMs = float64(a.dlat) / float64(a.Evaluated)
		}
		a.AtLeastAsGoodLow, a.AtLeastAsGoodHigh = wilson(float64(a.Better)+float64(a.Ties)/2, float64(a.Evaluated))
		a.Verdict = EvalVerdictFor(a.Evaluated, a.AtLeastAsGoodLow, a.AtLeastAsGoodHigh)
		out = append(out, a.RouteEvalSummary)
	}
	return out, nil
}

// Evaluation route verdicts.
const (
	EvalRouteKeep      = "keep"
	EvalRouteLosing    = "losing"
	EvalRouteNeedsData = "needs_data"
)

// EvalVerdictFor turns a tally into a recommendation. "Losing" when even the
// optimistic end of the interval says the routed model is at least as good
// less than half the time; "keep" when the pessimistic end is above 45% (a
// tie-ish route that saves money is worth keeping); otherwise more data.
func EvalVerdictFor(n int64, low, high float64) string {
	switch {
	case n < 10:
		return EvalRouteNeedsData
	case high < 0.5:
		return EvalRouteLosing
	case low > 0.45:
		return EvalRouteKeep
	}
	return EvalRouteNeedsData
}

// wilson is the 95% Wilson score interval for k successes in n trials.
func wilson(k, n float64) (float64, float64) {
	if n <= 0 {
		return 0, 0
	}
	const z = 1.96
	p := k / n
	den := 1 + z*z/n
	centre := (p + z*z/(2*n)) / den
	half := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n)) / den
	return math.Max(0, centre-half), math.Min(1, centre+half)
}
