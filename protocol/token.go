package protocol

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Token scopes. A runner token posts its run's events and reads and writes blobs; a viewer token only
// watches its run; a coordinator token also posts the run's plan and its verdict. A board token (run
// BoardRun) watches the board of every run and nothing else. A pool token (run = the pool's name) asks its
// pool for the next unit and nothing else; each unit it is handed carries its own run token.
const (
	ScopeRunner      = "runner"
	ScopeViewer      = "viewer"
	ScopeCoordinator = "coordinator"
	ScopeBoard       = "board"
	ScopePool        = "pool"
	// ScopePublish writes the public store (blobs and refs) and nothing else: a gate box's token.
	ScopePublish = "publish"
	// ScopePublishCandidate writes the public store's blobs and refs in a namespace ending -candidate only
	// (refs/build-candidate, refs/gocache-candidate), never refs/build or refs/gocache, which only main's own gate
	// writes (@system_adamic_developer_tools, Oct 9): what a candidate built can only ever mislead another candidate.
	ScopePublishCandidate = "publish-candidate"
	// ScopeSubmit submits a change to main and reads changes, and nothing else. Its run claim is the owner's
	// username: the wire takes a change only when the body's owner matches it (docs/contracts.md, the API).
	ScopeSubmit = "submit"
)

// BoardRun is the run a board token names.
const BoardRun = "board"

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
	if !RunIdPattern.MatchString(claims.Run) || claims.Expires <= 0 {
		return "", fmt.Errorf("a token needs a run id the wire accepts and an expiry")
	}
	if !knownScope(claims.Scope) {
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

func knownScope(scope string) bool {
	return scope == ScopeRunner || scope == ScopeViewer || scope == ScopeCoordinator || scope == ScopeBoard || scope == ScopePool || scope == ScopePublish || scope == ScopePublishCandidate || scope == ScopeSubmit
}

// ReadTokenSecret reads the HMAC key from a file such as ~/.loom/token-secret. The key is the file's text
// as it stands, surrounding whitespace trimmed: hex digits are signed as text, never decoded to bytes,
// because the Worker's LOOM_TOKEN_SECRET holds the same text and signs with its UTF-8 bytes.
func ReadTokenSecret(path string) ([]byte, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	secret := bytes.TrimSpace(text)
	if len(secret) == 0 {
		return nil, fmt.Errorf("%s holds no token secret", path)
	}
	return secret, nil
}

// VerifyToken checks the signature first, then the claims exactly as the Worker does: the keys run, scope
// and expires spelled in lowercase and nothing else, a non-empty run, a known scope, and not expired.
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
	// Go's decoder matches keys without regard to case; the Worker doesn't, so the keys are checked first.
	var keys map[string]json.RawMessage
	if err := Decode(bytes.NewReader(payload), &keys); err != nil {
		return TokenClaims{}, fmt.Errorf("malformed token claims: %w", err)
	}
	for _, key := range []string{"run", "scope", "expires"} {
		if _, found := keys[key]; !found || len(keys) != 3 {
			return TokenClaims{}, fmt.Errorf("malformed token claims: exactly run, scope and expires")
		}
	}
	var claims TokenClaims
	if err := Decode(bytes.NewReader(payload), &claims); err != nil {
		return TokenClaims{}, fmt.Errorf("malformed token claims: %w", err)
	}
	if claims.Run == "" || !knownScope(claims.Scope) || claims.Expires <= 0 {
		return TokenClaims{}, fmt.Errorf("malformed token claims: a run, a known scope and an expiry")
	}
	if now.Unix() >= claims.Expires {
		return TokenClaims{}, fmt.Errorf("token expired")
	}
	return claims, nil
}
