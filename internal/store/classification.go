package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Prompt classification: what kinds of work flow through the gateway.
//
// A classifier (any System One decision model, or any chat model with a
// prompt) labels sampled requests with one category from an admin-curated
// taxonomy, optionally refined by a subcategory. Results are stored per
// request so reports can break traffic down by category over time, by model,
// by team and by person. Prompt text is never stored: only the label, its
// confidence and how long the classifier took.

var classificationMigration = migration{name: "0047_prompt_classification", stmt: append([]string{
	`CREATE TABLE IF NOT EXISTS classification_category (
		id TEXT PRIMARY KEY,
		slug TEXT NOT NULL,
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		parent_slug TEXT NOT NULL DEFAULT '',
		builtin INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_classification_category_slug ON classification_category(slug)`,
	`CREATE TABLE IF NOT EXISTS classification_result (
		id TEXT PRIMARY KEY,
		created_at TEXT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL DEFAULT '',
		service_token_id TEXT NOT NULL DEFAULT '',
		team_ids TEXT NOT NULL DEFAULT '',
		model_name TEXT NOT NULL DEFAULT '',
		category TEXT NOT NULL DEFAULT '',
		subcategory TEXT NOT NULL DEFAULT '',
		confidence REAL NOT NULL DEFAULT 0,
		probabilities TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT '',
		protocol TEXT NOT NULL DEFAULT '',
		classifier TEXT NOT NULL DEFAULT '',
		latency_ms INTEGER NOT NULL DEFAULT 0,
		queue_ms INTEGER NOT NULL DEFAULT 0,
		input_chars INTEGER NOT NULL DEFAULT 0,
		tokens_in BIGINT NOT NULL DEFAULT 0,
		tokens_out BIGINT NOT NULL DEFAULT 0,
		cost_nanousd BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_classification_result_time ON classification_result(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_classification_result_category ON classification_result(category, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_classification_result_request ON classification_result(request_id)`,
}, seedClassificationStatements()...), down: []string{
	`DROP INDEX IF EXISTS idx_classification_result_request`,
	`DROP INDEX IF EXISTS idx_classification_result_category`,
	`DROP INDEX IF EXISTS idx_classification_result_time`,
	`DROP TABLE IF EXISTS classification_result`,
	`DROP INDEX IF EXISTS idx_classification_category_slug`,
	`DROP TABLE IF EXISTS classification_category`,
}}

// ClassificationCategory is one label in the taxonomy. Built-in categories
// ship with Janus and can be disabled or re-described but not deleted;
// custom categories are the organization's own. A category with a
// ParentSlug is a subcategory: it refines its parent (Rust under Coding,
// Reverse engineering under Security) and is only asked about when the
// classifier picks the parent.
type ClassificationCategory struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	ParentSlug  string    `json:"parent_slug"`
	Builtin     bool      `json:"builtin"`
	Enabled     bool      `json:"enabled"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ClassificationOtherSlug is the catch-all. It is always offered to the
// classifier, so a request that fits nothing is labelled rather than forced
// into the nearest wrong bucket.
const ClassificationOtherSlug = "other"

// Classifier limits. Decision models score every option, and the common
// self-hosted servers cap a choice at 26 options (Ollama) while TypeSafe
// allows 255; the lower bound keeps every backend usable.
const (
	MaxTopLevelCategories   = 26
	MaxSubcategoriesPerRoot = 25
)

// BuiltinClassificationCategories is the default taxonomy, in display order.
// Descriptions are what the classifier reads, so each says what belongs and,
// where neighbours are easy to confuse, what does not.
var BuiltinClassificationCategories = []ClassificationCategory{
	{Slug: "coding", Name: "Coding", Description: "Writing, reviewing, explaining, refactoring or debugging source code, scripts or queries. Not configuring or operating software (Technical help)."},
	{Slug: "technical_help", Name: "Technical help", Description: "IT and technical support: installing, configuring, operating or troubleshooting software, devices, networks and cloud services. Not writing code (Coding)."},
	{Slug: "data_analysis", Name: "Data analysis", Description: "Analysing data: statistics, spreadsheets, metrics, charts, interpreting results or datasets."},
	{Slug: "extraction", Name: "Extraction and formatting", Description: "Pulling structured fields out of text, converting between formats (JSON, CSV, tables), classifying or tagging records."},
	{Slug: "writing", Name: "Writing and editing", Description: "Drafting, rewriting, proofreading or improving non-fiction prose: emails, documents, reports, posts."},
	{Slug: "creative", Name: "Creative writing", Description: "Fiction, poetry, scripts, song lyrics, worldbuilding, role-play and other imaginative writing."},
	{Slug: "summarization", Name: "Summarization", Description: "Condensing a supplied document, meeting, thread or transcript into a shorter form."},
	{Slug: "translation", Name: "Translation and language", Description: "Translating between languages, grammar questions, language learning."},
	{Slug: "research", Name: "Research", Description: "Finding out about a topic: fact-finding, comparing options, literature review, explaining how something works in the world."},
	{Slug: "education", Name: "Learning and tutoring", Description: "Teaching or explaining a concept so the asker learns it, homework help, study plans, quizzes."},
	{Slug: "science_math", Name: "Science and math", Description: "Mathematics, physics, chemistry, biology, engineering calculations and scientific reasoning."},
	{Slug: "business", Name: "Business and strategy", Description: "Business planning, operations, management, strategy, product decisions, project planning."},
	{Slug: "marketing", Name: "Marketing and sales", Description: "Marketing copy, campaigns, SEO, branding, sales outreach and positioning."},
	{Slug: "customer_support", Name: "Customer communication", Description: "Answering or drafting replies to customers, support tickets, complaints and account questions."},
	{Slug: "finance", Name: "Finance and accounting", Description: "Accounting, budgeting, financial analysis, investing, tax, invoices and payments."},
	{Slug: "legal", Name: "Legal", Description: "Law, contracts, terms, compliance, regulation, disputes and legal rights."},
	{Slug: "medical", Name: "Medical and health", Description: "Symptoms, conditions, medication, treatment, clinical documentation, mental and physical health."},
	{Slug: "hr", Name: "HR and people", Description: "Hiring, job descriptions, performance reviews, workplace policy, employee relations."},
	{Slug: "security", Name: "Security", Description: "Cyber security: vulnerabilities, threats, incident response, hardening, penetration testing, malware analysis."},
	{Slug: "personal", Name: "Personal and lifestyle", Description: "Travel, food, hobbies, relationships, personal advice and everyday life."},
	{Slug: "general_chat", Name: "General chat", Description: "Greetings, small talk, trivia, jokes, and short questions with no specific domain."},
	{Slug: ClassificationOtherSlug, Name: "Other", Description: "None of the other categories fits."},
}

var classificationSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,47}$`)

// truncateUTF8 cuts s to at most n bytes on a character boundary and drops
// invalid bytes: PostgreSQL refuses invalid UTF-8 in a text column.
func truncateUTF8(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ClassificationSlug derives a stable identifier from a display name.
func ClassificationSlug(name string) string {
	var b strings.Builder
	underscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			underscore = false
		case r == '+':
			b.WriteString("plus")
			underscore = false
		case r == '#':
			b.WriteString("sharp")
			underscore = false
		default:
			if !underscore && b.Len() > 0 {
				b.WriteByte('_')
				underscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 48 {
		out = strings.Trim(out[:48], "_")
	}
	return out
}

// seedClassificationStatements inserts the built-in taxonomy as part of the
// migration, so every install starts with it. The ids are derived from the
// slug, and ON CONFLICT leaves an existing row alone: an admin's edits
// survive, and a later migration can add new built-ins the same way.
func seedClassificationStatements() []string {
	const seededAt = "2026-10-05T00:00:00.000000000Z"
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	out := make([]string, 0, len(BuiltinClassificationCategories))
	for i, c := range BuiltinClassificationCategories {
		out = append(out, fmt.Sprintf(`INSERT INTO classification_category (id, slug, name, description, parent_slug, builtin, enabled, sort_order, created_at, updated_at)
			VALUES (%s, %s, %s, %s, '', 1, 1, %d, '%s', '%s') ON CONFLICT (slug) DO NOTHING`,
			quote("cat_"+c.Slug), quote(c.Slug), quote(c.Name), quote(c.Description), (i+1)*10, seededAt, seededAt))
	}
	return out
}

const classificationCategoryColumns = `id, slug, name, description, parent_slug, builtin, enabled, sort_order, created_at, updated_at`

func scanClassificationCategory(scan func(...any) error) (*ClassificationCategory, error) {
	var c ClassificationCategory
	var builtin, enabled int
	var created, updated string
	if err := scan(&c.ID, &c.Slug, &c.Name, &c.Description, &c.ParentSlug, &builtin, &enabled, &c.SortOrder, &created, &updated); err != nil {
		return nil, err
	}
	c.Builtin, c.Enabled = builtin == 1, enabled == 1
	c.CreatedAt, c.UpdatedAt = ParseTime(created), ParseTime(updated)
	return &c, nil
}

// ListClassificationCategories returns the whole taxonomy: top-level
// categories in display order, each followed by its subcategories.
func (s *Store) ListClassificationCategories(ctx context.Context) ([]ClassificationCategory, error) {
	rows, err := s.query(ctx, `SELECT `+classificationCategoryColumns+` FROM classification_category`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var all []ClassificationCategory
	for rows.Next() {
		c, err := scanClassificationCategory(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan classification category: %w", err)
		}
		all = append(all, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return orderTaxonomy(all), nil
}

// orderTaxonomy sorts roots by sort order (Other last), each followed by its
// children alphabetically; orphans (parent deleted) sort after everything.
func orderTaxonomy(all []ClassificationCategory) []ClassificationCategory {
	byParent := map[string][]ClassificationCategory{}
	var roots []ClassificationCategory
	for _, c := range all {
		if c.ParentSlug == "" {
			roots = append(roots, c)
		} else {
			byParent[c.ParentSlug] = append(byParent[c.ParentSlug], c)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool {
		oi, oj := roots[i].Slug == ClassificationOtherSlug, roots[j].Slug == ClassificationOtherSlug
		if oi != oj {
			return oj
		}
		if roots[i].SortOrder != roots[j].SortOrder {
			return roots[i].SortOrder < roots[j].SortOrder
		}
		return strings.ToLower(roots[i].Name) < strings.ToLower(roots[j].Name)
	})
	out := make([]ClassificationCategory, 0, len(all))
	seen := map[string]bool{}
	for _, r := range roots {
		out = append(out, r)
		kids := byParent[r.Slug]
		sort.SliceStable(kids, func(i, j int) bool { return strings.ToLower(kids[i].Name) < strings.ToLower(kids[j].Name) })
		out = append(out, kids...)
		seen[r.Slug] = true
	}
	for parent, kids := range byParent {
		if !seen[parent] {
			out = append(out, kids...)
		}
	}
	return out
}

// ClassificationCategoryInput is an admin create or update.
type ClassificationCategoryInput struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	ParentSlug  string `json:"parent_slug"`
	Enabled     *bool  `json:"enabled"`
}

func (s *Store) categoryBySlug(ctx context.Context, slug string) (*ClassificationCategory, error) {
	c, err := scanClassificationCategory(s.queryRow(ctx, `SELECT `+classificationCategoryColumns+` FROM classification_category WHERE slug = ?`, slug).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ClassificationCategoryByID returns one category.
func (s *Store) ClassificationCategoryByID(ctx context.Context, id string) (*ClassificationCategory, error) {
	c, err := scanClassificationCategory(s.queryRow(ctx, `SELECT `+classificationCategoryColumns+` FROM classification_category WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// validateTaxonomyLimits refuses a change that would leave more enabled
// options than every classifier backend can score in one question.
func (s *Store) validateTaxonomyLimits(ctx context.Context, after func([]ClassificationCategory) []ClassificationCategory) error {
	all, err := s.ListClassificationCategories(ctx)
	if err != nil {
		return err
	}
	all = after(all)
	roots, kids := 0, map[string]int{}
	for _, c := range all {
		if !c.Enabled {
			continue
		}
		if c.ParentSlug == "" {
			roots++
		} else {
			kids[c.ParentSlug]++
		}
	}
	if roots > MaxTopLevelCategories {
		return &ValidationError{Field: "enabled", Message: fmt.Sprintf("at most %d top-level categories can be enabled at once (decision models score up to %d options per question); disable one first or add this as a subcategory", MaxTopLevelCategories, MaxTopLevelCategories)}
	}
	for parent, n := range kids {
		if n > MaxSubcategoriesPerRoot {
			return &ValidationError{Field: "parent_slug", Message: fmt.Sprintf("%s already has %d enabled subcategories; the limit is %d", parent, n-1, MaxSubcategoriesPerRoot)}
		}
	}
	return nil
}

// CreateClassificationCategory adds a custom category or subcategory.
func (s *Store) CreateClassificationCategory(ctx context.Context, in ClassificationCategoryInput) (*ClassificationCategory, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 80 {
		return nil, &ValidationError{Field: "name", Message: "a name of 1 to 80 characters is required"}
	}
	desc := strings.TrimSpace(in.Description)
	if desc == "" {
		return nil, &ValidationError{Field: "description", Message: "describe what belongs in this category; the classifier reads this description to decide"}
	}
	if len(desc) > 600 {
		return nil, &ValidationError{Field: "description", Message: "keep the description under 600 characters"}
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = ClassificationSlug(name)
	}
	if !classificationSlugPattern.MatchString(slug) || slug == "none" {
		return nil, &ValidationError{Field: "slug", Message: "use lowercase letters, digits and underscores (start with a letter or digit, at most 48 characters)"}
	}
	if _, err := s.categoryBySlug(ctx, slug); err == nil {
		return nil, &ValidationError{Field: "name", Message: "a category with this name already exists"}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	parent := strings.TrimSpace(in.ParentSlug)
	if parent != "" {
		p, err := s.categoryBySlug(ctx, parent)
		if errors.Is(err, ErrNotFound) {
			return nil, &ValidationError{Field: "parent_slug", Message: "the parent category does not exist"}
		}
		if err != nil {
			return nil, err
		}
		if p.ParentSlug != "" {
			return nil, &ValidationError{Field: "parent_slug", Message: "subcategories cannot have their own subcategories; choose a top-level parent"}
		}
		if p.Slug == ClassificationOtherSlug {
			return nil, &ValidationError{Field: "parent_slug", Message: "Other cannot have subcategories; add a top-level category instead"}
		}
	}
	enabled := in.Enabled == nil || *in.Enabled
	c := ClassificationCategory{ID: NewID(), Slug: slug, Name: name, Description: desc, ParentSlug: parent, Enabled: enabled, SortOrder: 1000}
	if err := s.validateTaxonomyLimits(ctx, func(all []ClassificationCategory) []ClassificationCategory { return append(all, c) }); err != nil {
		return nil, err
	}
	now := FormatTime(nowUTC())
	if err := s.exec(ctx, `INSERT INTO classification_category (id, slug, name, description, parent_slug, builtin, enabled, sort_order, created_at, updated_at) VALUES (?,?,?,?,?,0,?,?,?,?)`,
		c.ID, c.Slug, c.Name, c.Description, c.ParentSlug, boolInt(c.Enabled), c.SortOrder, now, now); err != nil {
		return nil, fmt.Errorf("create classification category: %w", err)
	}
	return s.ClassificationCategoryByID(ctx, c.ID)
}

// UpdateClassificationCategory edits a category. Built-ins keep their slug
// and parent (reports join on the slug); their name, description and enabled
// flag are the admin's to change. The catch-all cannot be disabled.
func (s *Store) UpdateClassificationCategory(ctx context.Context, id string, in ClassificationCategoryInput) (*ClassificationCategory, error) {
	cur, err := s.ClassificationCategoryByID(ctx, id)
	if err != nil {
		return nil, err
	}
	next := *cur
	if n := strings.TrimSpace(in.Name); n != "" {
		if len(n) > 80 {
			return nil, &ValidationError{Field: "name", Message: "keep the name under 80 characters"}
		}
		next.Name = n
	}
	if d := strings.TrimSpace(in.Description); d != "" {
		if len(d) > 600 {
			return nil, &ValidationError{Field: "description", Message: "keep the description under 600 characters"}
		}
		next.Description = d
	}
	if in.Enabled != nil {
		if !*in.Enabled && cur.Slug == ClassificationOtherSlug {
			return nil, &ValidationError{Field: "enabled", Message: "Other is always offered so requests that fit nothing are not forced into a wrong category"}
		}
		next.Enabled = *in.Enabled
	}
	if err := s.validateTaxonomyLimits(ctx, func(all []ClassificationCategory) []ClassificationCategory {
		for i := range all {
			if all[i].ID == id {
				all[i] = next
			}
		}
		return all
	}); err != nil {
		return nil, err
	}
	if err := s.exec(ctx, `UPDATE classification_category SET name = ?, description = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		next.Name, next.Description, boolInt(next.Enabled), FormatTime(nowUTC()), id); err != nil {
		return nil, fmt.Errorf("update classification category: %w", err)
	}
	return s.ClassificationCategoryByID(ctx, id)
}

// DeleteClassificationCategory removes a custom category and its
// subcategories. Past results keep their slug and still appear in reports
// (labelled by slug), so history is never rewritten.
func (s *Store) DeleteClassificationCategory(ctx context.Context, id string) error {
	cur, err := s.ClassificationCategoryByID(ctx, id)
	if err != nil {
		return err
	}
	if cur.Builtin {
		return &ValidationError{Field: "id", Message: "built-in categories cannot be deleted; disable it instead"}
	}
	// Routing categories that follow this category (or one of the
	// subcategories deleted with it) read its description; deleting it
	// would leave them with none, silently misrouting.
	rows, err := s.query(ctx, `SELECT name FROM routing_category WHERE linked_slug = ? OR linked_slug IN (SELECT slug FROM classification_category WHERE parent_slug = ?) ORDER BY name`, cur.Slug, cur.Slug)
	if err != nil {
		return err
	}
	var linked []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return err
		}
		linked = append(linked, n)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(linked) > 0 {
		return &ValidationError{Field: "id", Message: fmt.Sprintf("Auto-routing categories follow this category (%s); give them their own description or link them elsewhere first", strings.Join(linked, ", "))}
	}
	if err := s.exec(ctx, `DELETE FROM classification_category WHERE parent_slug = ? AND builtin = 0`, cur.Slug); err != nil {
		return err
	}
	return s.exec(ctx, `DELETE FROM classification_category WHERE id = ?`, id)
}

// --- Settings ---------------------------------------------------------------

// SettingClassification is the system_setting key for the classifier.
const SettingClassification = "prompt_classification"

// Classification modes.
const (
	// ClassificationModeAsync classifies after the request is admitted, on a
	// bounded background queue: reporting never adds latency, and a busy or
	// failed classifier only drops samples.
	ClassificationModeAsync = "async"
	// ClassificationModeInline classifies before the request is forwarded,
	// within a time budget, so the label is available to the request itself
	// (response header today, routing later). A timeout or failure never
	// fails the request: it proceeds unlabelled and the miss is recorded.
	ClassificationModeInline = "inline"
)

// Classification protocols.
const (
	// ClassificationProtocolSystemOne speaks the System One decision API
	// (TypeSafe Jev, Bespoke Nimble, Laya; served by TypeSafe, OpenRouter,
	// Ollama or llama.cpp): calibrated probabilities, no generation.
	ClassificationProtocolSystemOne = "systemone"
	// ClassificationProtocolChat prompts any chat model through its normal
	// upstream adapter and parses a JSON answer. Works with every provider
	// Janus supports; slower, and its confidence is self-reported.
	ClassificationProtocolChat = "chat"
)

// Text scopes: which part of the conversation is classified.
const (
	ClassificationScopeLastUser = "last_user"
	ClassificationScopeAllUser  = "all_user"
	ClassificationScopeAll      = "conversation"
)

// ClassificationSettings is the admin configuration.
type ClassificationSettings struct {
	Enabled  bool   `json:"enabled"`
	Mode     string `json:"mode"`
	Protocol string `json:"protocol"`
	// UpstreamID is the provider the classifier runs on; Model is the name
	// sent to it (a catalog model, or a decision model the provider does not
	// list, such as OpenRouter's ~typesafe/jev-latest).
	UpstreamID string `json:"upstream_id"`
	Model      string `json:"model"`
	// SampleRate is the percentage (1-100) of eligible requests classified.
	SampleRate int `json:"sample_rate"`
	// TimeoutMs bounds one classifier call. In inline mode it is also the
	// most a request can wait for its label.
	TimeoutMs int `json:"timeout_ms"`
	// TextScope selects the classified text; MaxChars trims it (the most
	// recent text is kept) to fit small classifier contexts.
	TextScope           string `json:"text_scope"`
	IncludeSystemPrompt bool   `json:"include_system_prompt"`
	MaxChars            int    `json:"max_chars"`
	// IncludeServiceTokens extends classification to automated traffic.
	IncludeServiceTokens bool `json:"include_service_tokens"`
	// Concurrency is the number of classifier calls in flight per gateway
	// replica (async workers, or the inline limit); QueueSize bounds the
	// async backlog. A full queue drops the sample and counts it.
	Concurrency int `json:"concurrency"`
	QueueSize   int `json:"queue_size"`
	// Subcategories asks the refining question for parents that have
	// enabled children. Off halves the work for slow classifiers.
	Subcategories bool `json:"subcategories"`
	// RetentionDays deletes results older than this (0 keeps them).
	RetentionDays int       `json:"retention_days"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// DefaultClassificationSettings is the starting point before an admin saves.
func DefaultClassificationSettings() ClassificationSettings {
	return ClassificationSettings{
		Enabled: false, Mode: ClassificationModeAsync, Protocol: ClassificationProtocolSystemOne,
		SampleRate: 100, TimeoutMs: 5000, TextScope: ClassificationScopeLastUser, MaxChars: 6000,
		IncludeServiceTokens: true, Concurrency: 4, QueueSize: 1000, Subcategories: true, RetentionDays: 180,
	}
}

// Validate normalises and checks the settings.
func (c *ClassificationSettings) Validate() error {
	switch c.Mode {
	case ClassificationModeAsync, ClassificationModeInline:
	default:
		return &ValidationError{Field: "mode", Message: "mode must be async or inline"}
	}
	switch c.Protocol {
	case ClassificationProtocolSystemOne, ClassificationProtocolChat:
	default:
		return &ValidationError{Field: "protocol", Message: "protocol must be systemone or chat"}
	}
	switch c.TextScope {
	case ClassificationScopeLastUser, ClassificationScopeAllUser, ClassificationScopeAll:
	default:
		return &ValidationError{Field: "text_scope", Message: "text_scope must be last_user, all_user or conversation"}
	}
	c.Model = strings.TrimSpace(c.Model)
	if c.Enabled && (c.UpstreamID == "" || c.Model == "") {
		return &ValidationError{Field: "model", Message: "choose the provider and model that will classify requests before turning classification on"}
	}
	if c.SampleRate < 1 || c.SampleRate > 100 {
		return &ValidationError{Field: "sample_rate", Message: "sample rate must be between 1 and 100 percent"}
	}
	if c.TimeoutMs < 100 || c.TimeoutMs > 60000 {
		return &ValidationError{Field: "timeout_ms", Message: "timeout must be between 100 ms and 60 s"}
	}
	if c.MaxChars < 200 || c.MaxChars > 200000 {
		return &ValidationError{Field: "max_chars", Message: "maximum characters must be between 200 and 200,000"}
	}
	if c.Concurrency < 1 || c.Concurrency > 64 {
		return &ValidationError{Field: "concurrency", Message: "concurrency must be between 1 and 64"}
	}
	if c.QueueSize < 10 || c.QueueSize > 100000 {
		return &ValidationError{Field: "queue_size", Message: "queue size must be between 10 and 100,000"}
	}
	if c.RetentionDays < 0 || c.RetentionDays > 3650 {
		return &ValidationError{Field: "retention_days", Message: "retention must be between 0 (keep) and 3650 days"}
	}
	return nil
}

// ClassificationSettings returns the stored settings, or the defaults.
func (s *Store) ClassificationSettings(ctx context.Context) (ClassificationSettings, error) {
	raw, updated, err := s.systemSetting(ctx, SettingClassification)
	if errors.Is(err, ErrNotFound) {
		return DefaultClassificationSettings(), nil
	}
	if err != nil {
		return ClassificationSettings{}, err
	}
	out := DefaultClassificationSettings()
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return ClassificationSettings{}, fmt.Errorf("decode %s setting: %w", SettingClassification, err)
	}
	out.UpdatedAt = updated
	return out, nil
}

// SetClassificationSettings validates and stores the settings.
func (s *Store) SetClassificationSettings(ctx context.Context, c ClassificationSettings) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.UpdatedAt = time.Time{}
	encoded, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.putSystemSetting(ctx, SettingClassification, string(encoded))
}

// --- Results ----------------------------------------------------------------

// Classification result statuses.
const (
	ClassificationStatusOK      = "ok"
	ClassificationStatusError   = "error"
	ClassificationStatusTimeout = "timeout"
	ClassificationStatusDropped = "dropped"
)

// ClassificationResult is one classified request. It holds the label and
// the classifier's timing, never the text.
type ClassificationResult struct {
	ID             string             `json:"id"`
	CreatedAt      time.Time          `json:"created_at"`
	RequestID      string             `json:"request_id"`
	UserID         string             `json:"user_id"`
	UserName       string             `json:"user_name,omitempty"`
	ServiceTokenID string             `json:"service_token_id,omitempty"`
	TeamIDs        string             `json:"team_ids"`
	ModelName      string             `json:"model_name"`
	Category       string             `json:"category"`
	Subcategory    string             `json:"subcategory"`
	Confidence     float64            `json:"confidence"`
	Probabilities  map[string]float64 `json:"probabilities,omitempty"`
	Status         string             `json:"status"`
	Error          string             `json:"error,omitempty"`
	Mode           string             `json:"mode"`
	Protocol       string             `json:"protocol"`
	Classifier     string             `json:"classifier"`
	LatencyMs      int                `json:"latency_ms"`
	QueueMs        int                `json:"queue_ms"`
	InputChars     int                `json:"input_chars"`
	TokensIn       int64              `json:"tokens_in"`
	TokensOut      int64              `json:"tokens_out"`
	CostNano       int64              `json:"cost_nanousd"`
	// Calls is the number of classifier requests this classification took.
	Calls int `json:"calls"`
}

// InsertClassificationResult stores one result.
func (s *Store) InsertClassificationResult(ctx context.Context, r *ClassificationResult) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = nowUTC()
	}
	probs := ""
	if len(r.Probabilities) > 0 {
		b, err := json.Marshal(r.Probabilities)
		if err != nil {
			return err
		}
		probs = string(b)
	}
	r.Error = truncateUTF8(r.Error, 500)
	if r.Calls < 0 {
		r.Calls = 0
	}
	return s.exec(ctx, `INSERT INTO classification_result (id, created_at, request_id, user_id, service_token_id, team_ids, model_name, category, subcategory, confidence, probabilities, status, error, mode, protocol, classifier, latency_ms, queue_ms, input_chars, tokens_in, tokens_out, cost_nanousd, calls)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, FormatTime(r.CreatedAt), r.RequestID, r.UserID, r.ServiceTokenID, r.TeamIDs, r.ModelName, r.Category, r.Subcategory, r.Confidence, probs,
		r.Status, r.Error, r.Mode, r.Protocol, r.Classifier, r.LatencyMs, r.QueueMs, r.InputChars, r.TokensIn, r.TokensOut, r.CostNano, r.Calls)
}

// ClassificationResultFilter narrows the recent-results list.
type ClassificationResultFilter struct {
	From, To  time.Time
	Category  string
	Status    string
	RequestID string
	Limit     int
}

// ListClassificationResults returns recent results, newest first.
func (s *Store) ListClassificationResults(ctx context.Context, f ClassificationResultFilter) ([]ClassificationResult, error) {
	where, args := []string{"1=1"}, []any{}
	if !f.From.IsZero() {
		where, args = append(where, "r.created_at >= ?"), append(args, FormatTime(f.From))
	}
	if !f.To.IsZero() {
		where, args = append(where, "r.created_at < ?"), append(args, FormatTime(f.To))
	}
	if f.Category != "" {
		where, args = append(where, "r.category = ?"), append(args, f.Category)
	}
	if f.Status != "" {
		where, args = append(where, "r.status = ?"), append(args, f.Status)
	}
	if f.RequestID != "" {
		where, args = append(where, "r.request_id = ?"), append(args, f.RequestID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.query(ctx, `SELECT r.id, r.created_at, r.request_id, r.user_id, COALESCE(u.name, ''), r.service_token_id, r.team_ids, r.model_name, r.category, r.subcategory, r.confidence, r.probabilities, r.status, r.error, r.mode, r.protocol, r.classifier, r.latency_ms, r.queue_ms, r.input_chars, r.tokens_in, r.tokens_out, r.cost_nanousd, r.calls
		FROM classification_result r LEFT JOIN app_user u ON u.id = r.user_id WHERE `+strings.Join(where, " AND ")+fmt.Sprintf(` ORDER BY r.created_at DESC LIMIT %d`, limit), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ClassificationResult{}
	for rows.Next() {
		var r ClassificationResult
		var created, probs string
		if err := rows.Scan(&r.ID, &created, &r.RequestID, &r.UserID, &r.UserName, &r.ServiceTokenID, &r.TeamIDs, &r.ModelName, &r.Category, &r.Subcategory, &r.Confidence, &probs,
			&r.Status, &r.Error, &r.Mode, &r.Protocol, &r.Classifier, &r.LatencyMs, &r.QueueMs, &r.InputChars, &r.TokensIn, &r.TokensOut, &r.CostNano, &r.Calls); err != nil {
			return nil, fmt.Errorf("scan classification result: %w", err)
		}
		r.CreatedAt = ParseTime(created)
		if probs != "" {
			_ = json.Unmarshal([]byte(probs), &r.Probabilities)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneClassificationResults deletes results older than the cutoff and
// reports how many went.
func (s *Store) PruneClassificationResults(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM classification_result WHERE created_at < ?`), FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("purge classification results: %w", err)
	}
	return res.RowsAffected()
}

// --- Report -----------------------------------------------------------------

// ClassificationCount is one bucket of classified requests.
type ClassificationCount struct {
	Key           string  `json:"key"`
	Label         string  `json:"label,omitempty"`
	Count         int64   `json:"count"`
	AvgConfidence float64 `json:"avg_confidence"`
}

// ClassificationCategoryRow is one category's line in the report.
type ClassificationCategoryRow struct {
	Category      string                `json:"category"`
	Count         int64                 `json:"count"`
	Share         float64               `json:"share"`
	AvgConfidence float64               `json:"avg_confidence"`
	LowConfidence int64                 `json:"low_confidence"`
	Subcategories []ClassificationCount `json:"subcategories"`
	TopModels     []ClassificationCount `json:"top_models"`
	TopPeople     []ClassificationCount `json:"top_people"`
}

// ClassificationSeriesPoint is one day (or hour) of category counts.
type ClassificationSeriesPoint struct {
	Bucket string           `json:"bucket"`
	Counts map[string]int64 `json:"counts"`
}

// ClassifierHealth summarises the classifier's own behaviour in the range.
type ClassifierHealth struct {
	Attempts int64   `json:"attempts"`
	OK       int64   `json:"ok"`
	Errors   int64   `json:"errors"`
	Timeouts int64   `json:"timeouts"`
	Dropped  int64   `json:"dropped"`
	AvgMs    float64 `json:"avg_ms"`
	P50Ms    int     `json:"p50_ms"`
	P95Ms    int     `json:"p95_ms"`
	MaxMs    int     `json:"max_ms"`
	// AvgQueue is the average wait in the background queue (background
	// classifications only; inline ones never queue).
	AvgQueue  float64 `json:"avg_queue_ms"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostNano  int64   `json:"cost_nanousd"`
	// Calls is the number of requests classifiers received (a label that
	// rode on a guard's call took none; a two-stage one took two).
	Calls int64 `json:"calls"`
	// ByClassifier splits latency by classifier and mode, so a change of
	// classifier (or inline versus async) can be compared side by side.
	ByClassifier []ClassifierLatency `json:"by_classifier"`
}

// ClassifierLatency is one classifier's latency in the range.
type ClassifierLatency struct {
	Classifier string  `json:"classifier"`
	Mode       string  `json:"mode"`
	Count      int64   `json:"count"`
	AvgMs      float64 `json:"avg_ms"`
	P50Ms      int     `json:"p50_ms"`
	P95Ms      int     `json:"p95_ms"`
}

// ClassificationReport is the admin overview.
type ClassificationReport struct {
	From       time.Time                   `json:"from"`
	To         time.Time                   `json:"to"`
	Bucket     string                      `json:"bucket"`
	Classified int64                       `json:"classified"`
	Categories []ClassificationCategoryRow `json:"categories"`
	Series     []ClassificationSeriesPoint `json:"series"`
	ByTeam     []ClassificationTeamRow     `json:"by_team"`
	Health     ClassifierHealth            `json:"health"`
}

// ClassificationTeamRow is one team's category mix.
type ClassificationTeamRow struct {
	TeamID string           `json:"team_id"`
	Team   string           `json:"team"`
	Total  int64            `json:"total"`
	Counts map[string]int64 `json:"counts"`
}

// LowConfidenceThreshold marks labels an admin should treat as uncertain.
const LowConfidenceThreshold = 0.5

// ClassificationReport aggregates results in [from, to).
func (s *Store) ClassificationReport(ctx context.Context, from, to time.Time) (*ClassificationReport, error) {
	rep := &ClassificationReport{From: from, To: to, Bucket: "day", Categories: []ClassificationCategoryRow{}, Series: []ClassificationSeriesPoint{}, ByTeam: []ClassificationTeamRow{}}
	bucketLen := 10 // YYYY-MM-DD
	if to.Sub(from) <= 48*time.Hour {
		rep.Bucket, bucketLen = "hour", 13 // YYYY-MM-DDTHH
	}
	f, t := FormatTime(from), FormatTime(to)

	// Category totals.
	rows, err := s.query(ctx, `SELECT category, COUNT(*), AVG(confidence), SUM(CASE WHEN confidence < ? THEN 1 ELSE 0 END)
		FROM classification_result WHERE created_at >= ? AND created_at < ? AND status = 'ok' GROUP BY category`, LowConfidenceThreshold, f, t)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for rows.Next() {
		var row ClassificationCategoryRow
		var avg sql.NullFloat64
		if err := rows.Scan(&row.Category, &row.Count, &avg, &row.LowConfidence); err != nil {
			_ = rows.Close()
			return nil, err
		}
		row.AvgConfidence = avg.Float64
		row.Subcategories, row.TopModels, row.TopPeople = []ClassificationCount{}, []ClassificationCount{}, []ClassificationCount{}
		rep.Classified += row.Count
		idx[row.Category] = len(rep.Categories)
		rep.Categories = append(rep.Categories, row)
	}
	_ = rows.Close()
	sort.SliceStable(rep.Categories, func(i, j int) bool { return rep.Categories[i].Count > rep.Categories[j].Count })
	for i := range rep.Categories {
		idx[rep.Categories[i].Category] = i
		if rep.Classified > 0 {
			rep.Categories[i].Share = float64(rep.Categories[i].Count) / float64(rep.Classified)
		}
	}

	// Breakdowns within each category.
	type sub struct {
		query string
		apply func(row *ClassificationCategoryRow, c ClassificationCount)
	}
	subs := []sub{
		{`SELECT category, subcategory, '', COUNT(*), AVG(confidence) FROM classification_result WHERE created_at >= ? AND created_at < ? AND status = 'ok' AND subcategory <> '' GROUP BY category, subcategory`,
			func(row *ClassificationCategoryRow, c ClassificationCount) {
				row.Subcategories = append(row.Subcategories, c)
			}},
		{`SELECT category, model_name, '', COUNT(*), AVG(confidence) FROM classification_result WHERE created_at >= ? AND created_at < ? AND status = 'ok' GROUP BY category, model_name`,
			func(row *ClassificationCategoryRow, c ClassificationCount) { row.TopModels = append(row.TopModels, c) }},
		{`SELECT r.category, CASE WHEN r.user_id <> '' THEN r.user_id ELSE 'svc:' || r.service_token_id END,
			COALESCE(MAX(u.name), MAX(st.name), ''), COUNT(*), AVG(r.confidence)
			FROM classification_result r LEFT JOIN app_user u ON u.id = r.user_id LEFT JOIN service_token st ON st.id = r.service_token_id
			WHERE r.created_at >= ? AND r.created_at < ? AND r.status = 'ok'
			GROUP BY r.category, CASE WHEN r.user_id <> '' THEN r.user_id ELSE 'svc:' || r.service_token_id END`,
			func(row *ClassificationCategoryRow, c ClassificationCount) { row.TopPeople = append(row.TopPeople, c) }},
	}
	for _, q := range subs {
		rows, err := s.query(ctx, q.query, f, t)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cat string
			var c ClassificationCount
			var avg sql.NullFloat64
			if err := rows.Scan(&cat, &c.Key, &c.Label, &c.Count, &avg); err != nil {
				_ = rows.Close()
				return nil, err
			}
			c.AvgConfidence = avg.Float64
			if i, ok := idx[cat]; ok {
				q.apply(&rep.Categories[i], c)
			}
		}
		_ = rows.Close()
	}
	for i := range rep.Categories {
		row := &rep.Categories[i]
		for _, list := range []*[]ClassificationCount{&row.Subcategories, &row.TopModels, &row.TopPeople} {
			sort.SliceStable(*list, func(a, b int) bool { return (*list)[a].Count > (*list)[b].Count })
			if len(*list) > 5 {
				*list = (*list)[:5]
			}
		}
	}

	// Time series.
	rows, err = s.query(ctx, fmt.Sprintf(`SELECT SUBSTR(created_at, 1, %d), category, COUNT(*) FROM classification_result
		WHERE created_at >= ? AND created_at < ? AND status = 'ok' GROUP BY SUBSTR(created_at, 1, %d), category`, bucketLen, bucketLen), f, t)
	if err != nil {
		return nil, err
	}
	points := map[string]map[string]int64{}
	for rows.Next() {
		var bucket, cat string
		var n int64
		if err := rows.Scan(&bucket, &cat, &n); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if points[bucket] == nil {
			points[bucket] = map[string]int64{}
		}
		points[bucket][cat] = n
	}
	_ = rows.Close()
	step := 24 * time.Hour
	layout := "2006-01-02"
	if rep.Bucket == "hour" {
		step, layout = time.Hour, "2006-01-02T15"
	}
	start := from.UTC().Truncate(step)
	for b := start; b.Before(to); b = b.Add(step) {
		key := b.Format(layout)
		counts := points[key]
		if counts == nil {
			counts = map[string]int64{}
		}
		rep.Series = append(rep.Series, ClassificationSeriesPoint{Bucket: key, Counts: counts})
	}

	// Teams. team_ids is the admitted team context (comma separated); a
	// request counted for several teams appears under each.
	rows, err = s.query(ctx, `SELECT team_ids, category, COUNT(*) FROM classification_result
		WHERE created_at >= ? AND created_at < ? AND status = 'ok' AND team_ids <> '' GROUP BY team_ids, category`, f, t)
	if err != nil {
		return nil, err
	}
	teams := map[string]*ClassificationTeamRow{}
	for rows.Next() {
		var ids, cat string
		var n int64
		if err := rows.Scan(&ids, &cat, &n); err != nil {
			_ = rows.Close()
			return nil, err
		}
		for _, id := range strings.Split(ids, ",") {
			if id = strings.TrimSpace(id); id == "" {
				continue
			}
			tr := teams[id]
			if tr == nil {
				tr = &ClassificationTeamRow{TeamID: id, Counts: map[string]int64{}}
				teams[id] = tr
			}
			tr.Total += n
			tr.Counts[cat] += n
		}
	}
	_ = rows.Close()
	for id, tr := range teams {
		_ = s.queryRow(ctx, `SELECT name FROM team WHERE id = ?`, id).Scan(&tr.Team)
		rep.ByTeam = append(rep.ByTeam, *tr)
	}
	sort.SliceStable(rep.ByTeam, func(i, j int) bool { return rep.ByTeam[i].Total > rep.ByTeam[j].Total })
	if len(rep.ByTeam) > 20 {
		rep.ByTeam = rep.ByTeam[:20]
	}

	// Classifier health and latency.
	h := &rep.Health
	h.ByClassifier = []ClassifierLatency{}
	var avg, avgQueue sql.NullFloat64
	var maxMs sql.NullInt64
	if err := s.queryRow(ctx, `SELECT COUNT(*),
		SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END), SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = 'timeout' THEN 1 ELSE 0 END), SUM(CASE WHEN status = 'dropped' THEN 1 ELSE 0 END),
		AVG(CASE WHEN status <> 'dropped' THEN latency_ms END), MAX(latency_ms), AVG(CASE WHEN status <> 'dropped' AND mode = 'async' THEN queue_ms END),
		COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0), COALESCE(SUM(cost_nanousd), 0), COALESCE(SUM(calls), 0)
		FROM classification_result WHERE created_at >= ? AND created_at < ?`, f, t).Scan(
		&h.Attempts, nullInt(&h.OK), nullInt(&h.Errors), nullInt(&h.Timeouts), nullInt(&h.Dropped), &avg, &maxMs, &avgQueue, &h.TokensIn, &h.TokensOut, &h.CostNano, &h.Calls); err != nil {
		return nil, err
	}
	h.AvgMs, h.MaxMs, h.AvgQueue = avg.Float64, int(maxMs.Int64), avgQueue.Float64
	lat, err := s.classifierLatencies(ctx, f, t, "", "")
	if err != nil {
		return nil, err
	}
	h.P50Ms, h.P95Ms = percentile(lat, 0.5), percentile(lat, 0.95)
	rows, err = s.query(ctx, `SELECT classifier, mode, COUNT(*), AVG(latency_ms) FROM classification_result
		WHERE created_at >= ? AND created_at < ? AND status IN ('ok','error','timeout') GROUP BY classifier, mode`, f, t)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c ClassifierLatency
		var a sql.NullFloat64
		if err := rows.Scan(&c.Classifier, &c.Mode, &c.Count, &a); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.AvgMs = a.Float64
		h.ByClassifier = append(h.ByClassifier, c)
	}
	_ = rows.Close()
	for i := range h.ByClassifier {
		l, err := s.classifierLatencies(ctx, f, t, h.ByClassifier[i].Classifier, h.ByClassifier[i].Mode)
		if err != nil {
			return nil, err
		}
		h.ByClassifier[i].P50Ms, h.ByClassifier[i].P95Ms = percentile(l, 0.5), percentile(l, 0.95)
	}
	sort.SliceStable(h.ByClassifier, func(i, j int) bool { return h.ByClassifier[i].Count > h.ByClassifier[j].Count })
	return rep, nil
}

// classifierLatencies returns sorted latencies for percentile maths. The
// range is capped at the most recent 20,000 calls, which keeps percentiles
// exact for ordinary volumes and cheap for large ones.
func (s *Store) classifierLatencies(ctx context.Context, from, to, classifier, mode string) ([]int, error) {
	q := `SELECT latency_ms FROM classification_result WHERE created_at >= ? AND created_at < ? AND status IN ('ok','error','timeout')`
	args := []any{from, to}
	if classifier != "" {
		q += ` AND classifier = ? AND mode = ?`
		args = append(args, classifier, mode)
	}
	rows, err := s.query(ctx, q+` ORDER BY created_at DESC LIMIT 20000`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	sort.Ints(out)
	return out, rows.Err()
}

// percentile is the nearest-rank percentile: the smallest value at or below
// which p of the samples fall.
func percentile(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

type nullIntScanner struct{ dst *int64 }

func (n nullIntScanner) Scan(v any) error {
	var x sql.NullInt64
	if err := x.Scan(v); err != nil {
		return err
	}
	*n.dst = x.Int64
	return nil
}

func nullInt(dst *int64) nullIntScanner { return nullIntScanner{dst: dst} }
