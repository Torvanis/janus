package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/torvanis/janus/internal/crypto"
)

// SettingDirectory is the system_setting key holding the LDAP directory
// configuration (Business: ldap). One directory per gateway.
const SettingDirectory = "auth.directory"

// Directory is the persisted LDAP configuration. BindPasswordEnc is AES-GCM
// under the gateway key; the API exposes only HasBindPassword.
type Directory struct {
	URL             string    `json:"url"`
	StartTLS        bool      `json:"start_tls"`
	SkipVerify      bool      `json:"skip_verify"`
	BindDN          string    `json:"bind_dn"`
	BindPasswordEnc string    `json:"bind_password_enc,omitempty"`
	BaseDN          string    `json:"base_dn"`
	UserFilter      string    `json:"user_filter"`
	EmailAttr       string    `json:"email_attr"`
	NameAttr        string    `json:"name_attr"`
	GroupsAttr      string    `json:"groups_attr"`
	GroupFilter     string    `json:"group_filter"`
	GroupNameAttr   string    `json:"group_name_attr"`
	AdminGroups     []string  `json:"admin_groups"`
	Enabled         bool      `json:"enabled"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ErrNoDirectory means nothing is configured yet.
var ErrNoDirectory = errors.New("no directory is configured")

// Directory loads the configuration; ErrNoDirectory when absent.
func (s *Store) Directory(ctx context.Context) (*Directory, error) {
	raw, updated, err := s.systemSetting(ctx, SettingDirectory)
	if errors.Is(err, ErrNotFound) || (err == nil && raw == "") {
		return nil, ErrNoDirectory
	}
	if err != nil {
		return nil, err
	}
	var d Directory
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, err
	}
	d.UpdatedAt = updated
	if d.AdminGroups == nil {
		d.AdminGroups = []string{}
	}
	return &d, nil
}

// SaveDirectory writes the configuration. An empty bindPassword keeps the
// previously stored one.
func (s *Store) SaveDirectory(ctx context.Context, c *crypto.Cipher, d Directory, bindPassword string) (*Directory, error) {
	if bindPassword != "" {
		enc, err := c.Encrypt(bindPassword)
		if err != nil {
			return nil, err
		}
		d.BindPasswordEnc = enc
	} else if prev, err := s.Directory(ctx); err == nil {
		d.BindPasswordEnc = prev.BindPasswordEnc
	}
	if d.AdminGroups == nil {
		d.AdminGroups = []string{}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if err := s.putSystemSetting(ctx, SettingDirectory, string(raw)); err != nil {
		return nil, err
	}
	return s.Directory(ctx)
}

// DeleteDirectory removes the configuration; directory users keep their
// accounts but cannot sign in through it any more.
func (s *Store) DeleteDirectory(ctx context.Context) error {
	return s.deleteSystemSetting(ctx, SettingDirectory)
}

// BindPassword decrypts the stored service-account password.
func (d *Directory) BindPassword(c *crypto.Cipher) (string, error) {
	if d.BindPasswordEnc == "" {
		return "", nil
	}
	return c.Decrypt(d.BindPasswordEnc)
}
