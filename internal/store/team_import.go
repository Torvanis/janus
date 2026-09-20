package store

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

const MaxTeamImportBytes = 2 << 20
const MaxTeamImportRows = 1000

type TeamImportRow struct {
	Row               int      `json:"row"`
	TeamName          string   `json:"team_name"`
	TeamAdmin         string   `json:"team_admin"`
	AdditionalMembers []string `json:"additional_members"`
	Errors            []string `json:"errors"`
	Warnings          []string `json:"warnings,omitempty"`
	leadID            string
	memberIDs         []string
}

type TeamImportPreview struct {
	Rows  []TeamImportRow `json:"rows"`
	Valid bool            `json:"valid"`
}

// PreviewTeamImport validates against existing identities without writing data.
func (s *Store) PreviewTeamImport(ctx context.Context, text string) (*TeamImportPreview, error) {
	p, err := parseTeamImport(text)
	if err != nil {
		return nil, err
	}
	teams, err := s.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, team := range teams {
		names[strings.ToLower(strings.TrimSpace(team.Name))] = true
	}
	for i := range p.Rows {
		row := &p.Rows[i]
		if names[strings.ToLower(row.TeamName)] {
			row.Errors = append(row.Errors, "team name already exists")
		}
		emails := append([]string{row.TeamAdmin}, row.AdditionalMembers...)
		for j, email := range emails {
			if email == "" {
				continue
			}
			users, err := s.query(ctx, `SELECT id, is_active FROM app_user WHERE LOWER(email) = ?`, email)
			if err != nil {
				return nil, err
			}
			var id string
			var active, count int
			for users.Next() {
				if err := users.Scan(&id, &active); err != nil {
					_ = users.Close()
					return nil, err
				}
				count++
			}
			err = users.Err()
			_ = users.Close()
			if err != nil {
				return nil, err
			}
			switch {
			case count == 0:
				row.Errors = append(row.Errors, "email not found: "+email)
			case count > 1:
				row.Errors = append(row.Errors, "ambiguous email: "+email)
			case active != 1:
				row.Errors = append(row.Errors, "inactive email: "+email)
			default:
				if j == 0 {
					row.leadID = id
				} else {
					row.memberIDs = append(row.memberIDs, id)
				}
			}
		}
		if len(row.Errors) > 0 {
			p.Valid = false
		}
	}
	return p, nil
}

// ImportTeamsCSV revalidates within the same transaction as all writes.
func (s *Store) ImportTeamsCSV(ctx context.Context, text string) ([]*Team, error) {
	teams := []*Team{}
	err := s.modelTx(ctx, func(tx *Store) error {
		preview, err := tx.PreviewTeamImport(ctx, text)
		if err != nil {
			return err
		}
		if !preview.Valid {
			return fmt.Errorf("CSV contains invalid rows; preview and correct them before importing")
		}
		for _, row := range preview.Rows {
			team, err := tx.CreateTeam(ctx, row.TeamName, row.leadID)
			if err != nil {
				return err
			}
			for _, id := range row.memberIDs {
				if err := tx.AddTeamMember(ctx, team.ID, id, "member", "manual", ""); err != nil {
					return err
				}
			}
			team, err = tx.TeamByID(ctx, team.ID)
			if err != nil {
				return err
			}
			teams = append(teams, team)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return teams, nil
}

func parseTeamImport(text string) (*TeamImportPreview, error) {
	if len(text) > MaxTeamImportBytes {
		return nil, fmt.Errorf("CSV exceeds 2 MiB limit")
	}
	reader := csv.NewReader(strings.NewReader(text))
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("invalid CSV header: %w", err)
	}
	if len(header) != 3 || header[0] != "Team_name" || header[1] != "Team_admin" || header[2] != "Additional_members" {
		return nil, fmt.Errorf("expected exact headers Team_name,Team_admin,Additional_members")
	}
	names := map[string]int{}
	p := &TeamImportPreview{Rows: []TeamImportRow{}, Valid: true}
	for {
		fields, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		line, _ := reader.FieldPos(0)
		row := TeamImportRow{Row: line, TeamName: strings.TrimSpace(fields[0]), TeamAdmin: strings.ToLower(strings.TrimSpace(fields[1])), AdditionalMembers: []string{}, Errors: []string{}}
		seen := map[string]bool{row.TeamAdmin: true}
		for _, email := range strings.Split(fields[2], ";") {
			email = strings.ToLower(strings.TrimSpace(email))
			if email == "" {
				continue
			}
			if seen[email] {
				row.Warnings = append(row.Warnings, "duplicate email omitted: "+email)
				continue
			}
			seen[email] = true
			row.AdditionalMembers = append(row.AdditionalMembers, email)
		}
		if row.TeamName == "" {
			row.Errors = append(row.Errors, "team name is required")
		}
		if row.TeamAdmin == "" {
			row.Errors = append(row.Errors, "team admin email is required")
		}
		key := strings.ToLower(row.TeamName)
		if first, exists := names[key]; exists {
			row.Errors = append(row.Errors, "duplicate team name in CSV")
			p.Rows[first].Errors = append(p.Rows[first].Errors, "duplicate team name in CSV")
		} else {
			names[key] = len(p.Rows)
		}
		if len(row.Errors) > 0 {
			p.Valid = false
		}
		p.Rows = append(p.Rows, row)
		if len(p.Rows) > MaxTeamImportRows {
			return nil, fmt.Errorf("CSV exceeds 1000 row limit")
		}
	}
	if len(p.Rows) == 0 {
		return nil, fmt.Errorf("CSV must contain at least one team")
	}
	return p, nil
}
