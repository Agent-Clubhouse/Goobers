package podauth

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/launchreceipt"
)

const launchMACDomain = "goobers/launch-receipt/v1\x00"

// MintLaunchGrant signs a receipt-bound capability under a separate MAC domain.
func (s *SignedKey) MintLaunchGrant(g launchreceipt.Grant, ttl time.Duration) (string, error) {
	if !validLaunchGrant(g) || ttl <= 0 || ttl > launchreceipt.MaxTTL {
		return "", launchreceipt.ErrInvalid
	}
	g.Expires = s.now().Add(ttl).Unix()
	raw, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return launchreceipt.TokenPrefix + payload + "." + s.sign(launchMACDomain+payload), nil
}

// VerifyLaunchGrant checks signature, bounded claims, and short-lived expiry.
func (s *SignedKey) VerifyLaunchGrant(token string) (launchreceipt.Grant, error) {
	rest, ok := strings.CutPrefix(token, launchreceipt.TokenPrefix)
	if !ok || len(token) > 1024 {
		return launchreceipt.Grant{}, launchreceipt.ErrInvalid
	}
	payload, mac, ok := strings.Cut(rest, ".")
	if !ok || subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(launchMACDomain+payload))) != 1 {
		return launchreceipt.Grant{}, launchreceipt.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return launchreceipt.Grant{}, launchreceipt.ErrInvalid
	}
	var g launchreceipt.Grant
	if json.Unmarshal(raw, &g) != nil || !validLaunchGrant(g) || g.Expires <= s.now().Unix() || g.Expires > s.now().Add(launchreceipt.MaxTTL).Unix() {
		return launchreceipt.Grant{}, launchreceipt.ErrInvalid
	}
	return g, nil
}

func validLaunchGrant(g launchreceipt.Grant) bool {
	id, ok := strings.CutPrefix(g.AttemptID, "sta_")
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	return ok && err == nil && len(decoded) == 32 && launchreceipt.ValidDigest(g.Digest)
}
