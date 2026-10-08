package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Token scopes. A runner token posts its run's events and reads and writes blobs; a viewer token only
// watches its run; a coordinator token also posts the run's plan and its verdict.
const (
	ScopeRunner      = "runner"
	ScopeViewer      = "viewer"
	ScopeCoordinator = "coordinator"
)

// TokenClaims is what a token says: one run, one scope, an expiry in Unix seconds.
type TokenClaims struct {
	Run     string `json:"run"`
	Scope   string `json:"scope"`
	Expires int64  `json:"expires"`
}

var encoding = base64.RawURLEncoding

// MintToken signs claims as <base64url(claims JSON)>.<base64url(HMAC-SHA256(secret, first part))>.
// The Worker (wire/) verifies the same bytes; TestTokenVector pins them.
func MintToken(secret []byte, claims TokenClaims) (string, error) {
	if claims.Run == "" || claims.Expires <= 0 {
		return "", fmt.Errorf("a token needs a run and an expiry")
	}
	switch claims.Scope {
	case ScopeRunner, ScopeViewer, ScopeCoordinator:
	default:
		return "", fmt.Errorf("unknown token scope %q", claims.Scope)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	first := encoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(first))
	return first + "." + encoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyToken checks the signature and expiry and returns the claims.
func VerifyToken(secret []byte, token string, now time.Time) (TokenClaims, error) {
	first, signature, found := strings.Cut(token, ".")
	if !found {
		return TokenClaims{}, fmt.Errorf("malformed token")
	}
	given, err := encoding.DecodeString(signature)
	if err != nil {
		return TokenClaims{}, fmt.Errorf("malformed token signature")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(first))
	if !hmac.Equal(given, mac.Sum(nil)) {
		return TokenClaims{}, fmt.Errorf("bad token signature")
	}
	payload, err := encoding.DecodeString(first)
	if err != nil {
		return TokenClaims{}, fmt.Errorf("malformed token claims")
	}
	var claims TokenClaims
	if err := Decode(strings.NewReader(string(payload)), &claims); err != nil {
		return TokenClaims{}, fmt.Errorf("malformed token claims: %w", err)
	}
	if now.Unix() >= claims.Expires {
		return TokenClaims{}, fmt.Errorf("token expired")
	}
	return claims, nil
}
