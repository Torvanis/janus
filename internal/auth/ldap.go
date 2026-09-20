package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// LDAPConfig describes one directory (Active Directory, OpenLDAP, FreeIPA,
// Authentik outpost). One directory per gateway; admins configure it in
// Admin → Provisioning → Directory.
type LDAPConfig struct {
	URL           string // ldap://host:389 or ldaps://host:636
	StartTLS      bool
	SkipVerify    bool
	BindDN        string // service account used to search; "" = anonymous
	BindPassword  string
	BaseDN        string
	UserFilter    string // e.g. (&(objectClass=person)(|(mail={login})(sAMAccountName={login})))
	EmailAttr     string // mail
	NameAttr      string // displayName / cn
	GroupsAttr    string // memberOf ("" = group search)
	GroupFilter   string // e.g. (&(objectClass=group)(member={dn})) when GroupsAttr is empty
	GroupNameAttr string // cn
	Timeout       time.Duration
}

// LDAPUser is what a successful bind yields; the DN is the stable subject.
type LDAPUser struct {
	DN     string
	Email  string
	Name   string
	Groups []string
}

// ErrLDAPBadCredentials is the same message the local path uses, so the
// login page cannot tell which store rejected the attempt.
var ErrLDAPBadCredentials = errors.New("email or password is incorrect")

// ErrLDAPUnavailable is surfaced (with the underlying cause in logs) when
// the directory itself cannot be reached — distinct from a wrong password.
var ErrLDAPUnavailable = errors.New("the directory could not be reached")

func (c LDAPConfig) dial(ctx context.Context) (*ldap.Conn, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: c.SkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // operator opt-in for self-signed labs
	conn, err := ldap.DialURL(c.URL, ldap.DialWithTLSConfig(tlsCfg), ldap.DialWithDialer(&net.Dialer{Timeout: timeout}))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLDAPUnavailable, err)
	}
	conn.SetTimeout(timeout)
	if c.StartTLS && !strings.HasPrefix(strings.ToLower(c.URL), "ldaps://") {
		if err := conn.StartTLS(tlsCfg); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%w: STARTTLS: %v", ErrLDAPUnavailable, err)
		}
	}
	return conn, nil
}

func (c LDAPConfig) serviceBind(conn *ldap.Conn) error {
	if c.BindDN == "" {
		return conn.UnauthenticatedBind("")
	}
	return conn.Bind(c.BindDN, c.BindPassword)
}

// Authenticate finds the user with UserFilter ({login} substituted), binds
// as that DN with the supplied password, then reads attributes and groups.
func (c LDAPConfig) Authenticate(ctx context.Context, login, password string) (*LDAPUser, error) {
	if strings.TrimSpace(login) == "" || password == "" {
		return nil, ErrLDAPBadCredentials
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := c.serviceBind(conn); err != nil {
		return nil, fmt.Errorf("%w: service bind: %v", ErrLDAPUnavailable, err)
	}
	entry, err := c.findUser(conn, login)
	if err != nil {
		return nil, err
	}
	// Bind as the user proves the password. A fresh connection is not
	// required: rebinding replaces the identity for the rest of the session.
	if err := conn.Bind(entry.DN, password); err != nil {
		var lerr *ldap.Error
		if errors.As(err, &lerr) && lerr.ResultCode == ldap.LDAPResultInvalidCredentials {
			return nil, ErrLDAPBadCredentials
		}
		return nil, fmt.Errorf("%w: user bind: %v", ErrLDAPUnavailable, err)
	}
	// Back to the service account for the group lookup (the user may not be
	// allowed to read group membership).
	if err := c.serviceBind(conn); err != nil {
		return nil, fmt.Errorf("%w: rebind: %v", ErrLDAPUnavailable, err)
	}
	u := &LDAPUser{DN: entry.DN, Email: strings.ToLower(entry.GetAttributeValue(c.attr(c.EmailAttr, "mail"))), Name: entry.GetAttributeValue(c.attr(c.NameAttr, "displayName"))}
	if u.Name == "" {
		u.Name = entry.GetAttributeValue("cn")
	}
	u.Groups, err = c.groups(conn, entry)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// Lookup is the admin "Test" path: service bind + user search, no password.
func (c LDAPConfig) Lookup(ctx context.Context, login string) (*LDAPUser, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := c.serviceBind(conn); err != nil {
		return nil, fmt.Errorf("%w: service bind: %v", ErrLDAPUnavailable, err)
	}
	if strings.TrimSpace(login) == "" {
		return &LDAPUser{}, nil
	}
	entry, err := c.findUser(conn, login)
	if err != nil {
		return nil, err
	}
	u := &LDAPUser{DN: entry.DN, Email: strings.ToLower(entry.GetAttributeValue(c.attr(c.EmailAttr, "mail"))), Name: entry.GetAttributeValue(c.attr(c.NameAttr, "displayName"))}
	u.Groups, err = c.groups(conn, entry)
	return u, err
}

func (c LDAPConfig) attr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func (c LDAPConfig) findUser(conn *ldap.Conn, login string) (*ldap.Entry, error) {
	filter := c.UserFilter
	if strings.TrimSpace(filter) == "" {
		filter = "(&(objectClass=person)(|(mail={login})(uid={login})(sAMAccountName={login})))"
	}
	filter = strings.ReplaceAll(filter, "{login}", ldap.EscapeFilter(login))
	attrs := []string{"dn", "cn", c.attr(c.EmailAttr, "mail"), c.attr(c.NameAttr, "displayName")}
	if c.GroupsAttr != "" {
		attrs = append(attrs, c.GroupsAttr)
	}
	res, err := conn.Search(ldap.NewSearchRequest(c.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false, filter, attrs, nil))
	if err != nil {
		return nil, fmt.Errorf("%w: user search: %v", ErrLDAPUnavailable, err)
	}
	if len(res.Entries) != 1 {
		// zero or ambiguous: both are "not you" from the login page's view
		return nil, ErrLDAPBadCredentials
	}
	return res.Entries[0], nil
}

func (c LDAPConfig) groups(conn *ldap.Conn, entry *ldap.Entry) ([]string, error) {
	nameAttr := c.attr(c.GroupNameAttr, "cn")
	if c.GroupsAttr != "" {
		out := []string{}
		for _, dn := range entry.GetAttributeValues(c.GroupsAttr) {
			out = append(out, rdnValue(dn, nameAttr))
		}
		return out, nil
	}
	if strings.TrimSpace(c.GroupFilter) == "" {
		return []string{}, nil
	}
	filter := strings.ReplaceAll(c.GroupFilter, "{dn}", ldap.EscapeFilter(entry.DN))
	res, err := conn.Search(ldap.NewSearchRequest(c.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, filter, []string{nameAttr}, nil))
	if err != nil {
		return nil, fmt.Errorf("%w: group search: %v", ErrLDAPUnavailable, err)
	}
	out := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		if v := e.GetAttributeValue(nameAttr); v != "" {
			out = append(out, v)
		} else {
			out = append(out, rdnValue(e.DN, nameAttr))
		}
	}
	return out, nil
}

// rdnValue extracts "Janus Admins" from "CN=Janus Admins,OU=Groups,DC=…".
func rdnValue(dn, attr string) string {
	parsed, err := ldap.ParseDN(dn)
	if err != nil || len(parsed.RDNs) == 0 {
		return dn
	}
	for _, a := range parsed.RDNs[0].Attributes {
		if strings.EqualFold(a.Type, attr) {
			return a.Value
		}
	}
	return parsed.RDNs[0].Attributes[0].Value
}
