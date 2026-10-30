package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Janus Chat administration.
//
// Instance-wide chat settings live in one system_setting row (SettingChat);
// the friendly name and description a model shows in the chat picker live
// ON THE GRANT (user ruling: "chat name and description become an optional
// setting in a grant"), so the same model can read "Fast & private" for one
// team and "Qwen 27B" for another, and reports keep the catalog name.

var chatAdminMigration = migration{name: "0062_chat_admin", stmt: []string{
	`ALTER TABLE model_grant ADD COLUMN chat_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE model_grant ADD COLUMN chat_description TEXT NOT NULL DEFAULT ''`,
}, down: []string{
	`ALTER TABLE model_grant DROP COLUMN chat_description`,
	`ALTER TABLE model_grant DROP COLUMN chat_name`,
}}

// SettingChat is the system_setting key for Janus Chat administration.
const SettingChat = "chat"

// Chat access modes.
const (
	ChatAccessEveryone   = "everyone"
	ChatAccessRestricted = "restricted"
)

// Chat model categories offered in the admin (free text is refused so the
// picker groups stay tidy; "" = uncategorised, shown under the default group).
var ChatCategories = []string{"general", "fast", "reasoning", "coding", "vision", "local", "creative"}

// Limits.
const (
	ChatGrantNameMax        = 60
	ChatGrantDescriptionMax = 200
	ChatStarterPromptsMax   = 8
	ChatStarterPromptMax    = 200
	ChatOrgInstructionsMax  = 8000
)

// ChatAccess restricts who may use Chat (Business). Everyone = every signed-in
// user; restricted = only the listed groups, teams and people.
type ChatAccess struct {
	Mode    string   `json:"mode"`
	Groups  []string `json:"groups"`
	Teams   []string `json:"teams"`
	Users   []string `json:"users"`
	Message string   `json:"message"`
}

// ChatModelSetting is how one model (by public name) appears in the picker.
type ChatModelSetting struct {
	Hidden      bool   `json:"hidden,omitempty"`
	Category    string `json:"category,omitempty"`
	Order       int    `json:"order,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
	// Images says whether Chat sends this model the images people attach:
	// "" = automatic (its modalities, else a guess from its name), "yes"
	// or "no". Self-hosted models are often named so the guess is wrong.
	Images string `json:"images,omitempty"`
	// Audio / Video: the same for sound and video files people attach.
	Audio string `json:"audio,omitempty"`
	Video string `json:"video,omitempty"`
}

// ChatAdminSettings is the instance-wide Chat configuration.
type ChatAdminSettings struct {
	// Enabled turns Chat on for the instance. JANUS_CHAT_PREVIEW=false is a
	// hard off above it.
	Enabled bool       `json:"enabled"`
	Access  ChatAccess `json:"access"`
	// DefaultModel is the picker's starting model for someone who has not
	// chosen one yet (public model name).
	DefaultModel string `json:"default_model"`
	// TemporaryTTLHours is how long an idle temporary chat is kept.
	TemporaryTTLHours int  `json:"temporary_ttl_hours"`
	SharesEnabled     bool `json:"shares_enabled"`
	ExportEnabled     bool `json:"export_enabled"`
	// PersonalPlans lets people chat with their own connected vendor plans.
	PersonalPlans bool `json:"personal_plans"`
	// OrgInstructions lead every chat's system message (before the user's).
	OrgInstructions string   `json:"org_instructions"`
	StarterPrompts  []string `json:"starter_prompts"`
	// ToolsOff lists Chat tools turned off for everyone (ChatTools; tools
	// are on by default).
	ToolsOff []string `json:"tools_off"`
	// Search is the Janus-run web search for models whose provider has
	// none (local models). Off by default.
	Search ChatSearchSettings `json:"search"`
	// ImageModel is the model Chat makes images with when a chat model
	// calls create_image ("" = the first image model the person may use).
	// A personal plan's own image model is used first for plan chats.
	ImageModel string `json:"image_model"`
	// VideoModel / MusicModel: the models create_video / create_music
	// use ("" = the first one the person may use; a plan's own first).
	VideoModel string `json:"video_model"`
	MusicModel string `json:"music_model"`
	// TranscribeModel turns attached sound into text for chat models that
	// do not hear it; DescribeModel (a model that watches video) does the
	// same for videos. "" = the first one the person may use.
	TranscribeModel string `json:"transcribe_model"`
	DescribeModel   string `json:"describe_model"`
	// Models curates the picker, keyed by public model name.
	Models    map[string]ChatModelSetting `json:"models"`
	UpdatedAt time.Time                   `json:"updated_at"`
}

// DefaultChatAdminSettings is what an instance gets before an admin saves:
// exactly the behaviour Chat had before administration existed.
func DefaultChatAdminSettings() ChatAdminSettings {
	return ChatAdminSettings{
		Enabled:           true,
		Access:            ChatAccess{Mode: ChatAccessEveryone, Groups: []string{}, Teams: []string{}, Users: []string{}},
		TemporaryTTLHours: int(ChatTemporaryTTL / time.Hour),
		SharesEnabled:     true,
		ExportEnabled:     true,
		PersonalPlans:     true,
		StarterPrompts:    []string{},
		ToolsOff:          []string{},
		Models:            map[string]ChatModelSetting{},
	}
}

// Validate normalises and checks the settings.
func (c *ChatAdminSettings) Validate() error {
	switch c.Access.Mode {
	case "":
		c.Access.Mode = ChatAccessEveryone
	case ChatAccessEveryone, ChatAccessRestricted:
	default:
		return &ValidationError{Field: "access.mode", Message: "access must be everyone or restricted"}
	}
	c.ToolsOff = CleanChatTools(c.ToolsOff)
	c.ImageModel = strings.TrimSpace(c.ImageModel)
	c.VideoModel = strings.TrimSpace(c.VideoModel)
	c.MusicModel = strings.TrimSpace(c.MusicModel)
	c.TranscribeModel = strings.TrimSpace(c.TranscribeModel)
	c.DescribeModel = strings.TrimSpace(c.DescribeModel)
	if err := c.Search.normalize(); err != nil {
		return err
	}
	c.Access.Groups = cleanIDs(c.Access.Groups)
	c.Access.Teams = cleanIDs(c.Access.Teams)
	c.Access.Users = cleanIDs(c.Access.Users)
	c.Access.Message = strings.TrimSpace(c.Access.Message)
	if len(c.Access.Message) > 300 {
		return &ValidationError{Field: "access.message", Message: "the message for people without access is limited to 300 characters"}
	}
	if c.Access.Mode == ChatAccessRestricted && len(c.Access.Groups)+len(c.Access.Teams)+len(c.Access.Users) == 0 {
		return &ValidationError{Field: "access", Message: "choose at least one group, team or person, or allow everyone"}
	}
	if c.TemporaryTTLHours < 1 || c.TemporaryTTLHours > 24*30 {
		return &ValidationError{Field: "temporary_ttl_hours", Message: "temporary chats must be kept between 1 hour and 30 days"}
	}
	c.DefaultModel = strings.TrimSpace(c.DefaultModel)
	c.OrgInstructions = strings.TrimSpace(c.OrgInstructions)
	if len(c.OrgInstructions) > ChatOrgInstructionsMax {
		return &ValidationError{Field: "org_instructions", Message: fmt.Sprintf("instructions are limited to %d characters", ChatOrgInstructionsMax)}
	}
	prompts := make([]string, 0, len(c.StarterPrompts))
	for _, p := range c.StarterPrompts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > ChatStarterPromptMax {
			return &ValidationError{Field: "starter_prompts", Message: fmt.Sprintf("each suggestion is limited to %d characters", ChatStarterPromptMax)}
		}
		prompts = append(prompts, p)
	}
	if len(prompts) > ChatStarterPromptsMax {
		return &ValidationError{Field: "starter_prompts", Message: fmt.Sprintf("at most %d suggestions", ChatStarterPromptsMax)}
	}
	c.StarterPrompts = prompts
	models := map[string]ChatModelSetting{}
	for name, m := range c.Models {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		m.Category = strings.ToLower(strings.TrimSpace(m.Category))
		if m.Category != "" && !contains(ChatCategories, m.Category) {
			return &ValidationError{Field: "models", Message: fmt.Sprintf("unknown category %q for %s", m.Category, name)}
		}
		for _, f := range []struct {
			what string
			v    *string
		}{{"images", &m.Images}, {"audio", &m.Audio}, {"video", &m.Video}} {
			*f.v = strings.ToLower(strings.TrimSpace(*f.v))
			if *f.v != "" && *f.v != "yes" && *f.v != "no" {
				return &ValidationError{Field: "models", Message: fmt.Sprintf("%s for %s must be yes, no or empty (automatic)", f.what, name)}
			}
		}
		if m.Order < 0 || m.Order > 999 {
			return &ValidationError{Field: "models", Message: "order must be between 0 and 999"}
		}
		if m == (ChatModelSetting{}) {
			continue
		}
		models[name] = m
	}
	c.Models = models
	return nil
}

func cleanIDs(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ChatAdminSettings reads the settings (defaults when never saved).
func (s *Store) ChatAdminSettings(ctx context.Context) (ChatAdminSettings, error) {
	raw, updated, err := s.systemSetting(ctx, SettingChat)
	if errors.Is(err, ErrNotFound) {
		return DefaultChatAdminSettings(), nil
	}
	if err != nil {
		return ChatAdminSettings{}, err
	}
	out := DefaultChatAdminSettings()
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return ChatAdminSettings{}, fmt.Errorf("decode %s setting: %w", SettingChat, err)
	}
	if out.Models == nil {
		out.Models = map[string]ChatModelSetting{}
	}
	if out.StarterPrompts == nil {
		out.StarterPrompts = []string{}
	}
	if out.Access.Mode == "" {
		out.Access.Mode = ChatAccessEveryone
	}
	for _, list := range []*[]string{&out.Access.Groups, &out.Access.Teams, &out.Access.Users} {
		if *list == nil {
			*list = []string{}
		}
	}
	out.UpdatedAt = updated
	return out, nil
}

// SetChatAdminSettings validates and stores the settings.
func (s *Store) SetChatAdminSettings(ctx context.Context, c ChatAdminSettings) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.UpdatedAt = time.Time{}
	encoded, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.putSystemSetting(ctx, SettingChat, string(encoded))
}

// SetGrantChatLabel sets (or clears, with "") a grant's chat name and
// description.
func (s *Store) SetGrantChatLabel(ctx context.Context, grantID, name, description string) (*Grant, error) {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if len([]rune(name)) > ChatGrantNameMax {
		return nil, &ValidationError{Field: "chat_name", Message: fmt.Sprintf("a chat name is limited to %d characters", ChatGrantNameMax)}
	}
	if len([]rune(description)) > ChatGrantDescriptionMax {
		return nil, &ValidationError{Field: "chat_description", Message: fmt.Sprintf("a chat description is limited to %d characters", ChatGrantDescriptionMax)}
	}
	var id string
	if err := s.queryRow(ctx, `SELECT id FROM model_grant WHERE id = ?`, grantID).Scan(&id); err != nil {
		return nil, ErrNotFound
	}
	if err := s.exec(ctx, `UPDATE model_grant SET chat_name = ?, chat_description = ? WHERE id = ?`, name, description, grantID); err != nil {
		return nil, err
	}
	grants, err := s.ListGrants(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		if g.ID == grantID {
			return g, nil
		}
	}
	return nil, ErrNotFound
}

// ChatLabel is the picker label a grant gives a model.
type ChatLabel struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ChatLabelsFor returns, per granted model id, the chat name/description of
// the MOST SPECIFIC grant that applies to the subject and carries one, in
// the same scope rules as GrantedIDsFor: in a team context the team grant
// beats all-teams; otherwise a direct grant beats a group grant beats
// all-users. Name and description resolve independently, so a group can
// name a model and the all-users grant still describe it.
func (s *Store) ChatLabelsFor(ctx context.Context, subject GrantSubject) (map[string]ChatLabel, error) {
	type tier struct {
		where string
		args  []any
	}
	var tiers []tier // most general first; later tiers override
	if subject.TeamID != "" {
		tiers = []tier{
			{`grantee_type = ?`, []any{GranteeAllTeams}},
			{`grantee_type = ? AND grantee_id = ?`, []any{GranteeTeam, subject.TeamID}},
		}
	} else if !subject.IsServiceToken() {
		tiers = []tier{{`grantee_type = ?`, []any{GranteeAllUsers}}}
		if len(subject.GroupIDs) > 0 {
			tiers = append(tiers, tier{`grantee_type = ? AND grantee_id IN (` + placeholders(len(subject.GroupIDs)) + `)`,
				append([]any{GranteeGroup}, toArgs(subject.GroupIDs)...)})
		}
		tiers = append(tiers, tier{`grantee_type = ? AND grantee_id = ?`, []any{GranteeUser, subject.UserID}})
	}
	out := map[string]ChatLabel{}
	for _, t := range tiers {
		rows, err := s.query(ctx, `SELECT model_id, chat_name, chat_description FROM model_grant WHERE (`+t.where+`) AND (chat_name <> '' OR chat_description <> '') ORDER BY created_at`, t.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, name, desc string
			if err := rows.Scan(&id, &name, &desc); err != nil {
				_ = rows.Close()
				return nil, err
			}
			l := out[id]
			if name != "" {
				l.Name = name
			}
			if desc != "" {
				l.Description = desc
			}
			out[id] = l
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
