package license

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// ActivationPrefix marks a portal activation code: one copy-paste value that
// carries both the signed license and its sync token, so an administrator
// never has to find and paste two separate strings.
//
// Wire format: JANUS-ACTIVATION-1.<base64url(JSON {"license","sync_token"})>
//
// The code is a transport bundle only. The license inside is still verified
// against the embedded public keys, and carrying a token never enables sync.
const ActivationPrefix = "JANUS-ACTIVATION-1"

// Activation is the decoded content of an activation code.
type Activation struct {
	License   string `json:"license"`
	SyncToken string `json:"sync_token"`
}

// EncodeActivation bundles a signed license and its raw sync token.
func EncodeActivation(signed, syncToken string) string {
	b, _ := json.Marshal(Activation{License: signed, SyncToken: syncToken})
	return ActivationPrefix + "." + base64.RawURLEncoding.EncodeToString(b)
}

// ParseActivation decodes an activation code. It does not verify the license
// signature; callers must still Verify the returned License.
func ParseActivation(code string) (Activation, error) {
	code = strings.TrimSpace(code)
	prefix, payload, ok := strings.Cut(code, ".")
	if !ok || prefix != ActivationPrefix || payload == "" || len(code) > 64<<10 {
		return Activation{}, errors.New("not a Janus activation code")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Activation{}, errors.New("activation code is damaged; copy it again")
	}
	var a Activation
	if err := json.Unmarshal(raw, &a); err != nil {
		return Activation{}, errors.New("activation code is damaged; copy it again")
	}
	a.License, a.SyncToken = strings.TrimSpace(a.License), strings.TrimSpace(a.SyncToken)
	if !strings.HasPrefix(a.License, Prefix+".") {
		return Activation{}, errors.New("activation code carries no license")
	}
	if a.SyncToken == "" || len(a.SyncToken) > 4096 || strings.ContainsAny(a.SyncToken, "\r\n\t .") {
		return Activation{}, errors.New("activation code carries no valid sync token")
	}
	return a, nil
}
