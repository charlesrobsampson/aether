package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrInvalidToken is returned when a token is malformed, unsigned, signed
	// with the wrong secret, or carries claims Aether cannot trust.
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrExpiredToken is returned when a correctly signed token is past its exp.
	ErrExpiredToken = errors.New("auth: token expired")
)

// Claims is the JWT payload Aether accepts. Exp is a Unix timestamp in seconds.
type Claims struct {
	Sub  string `json:"sub"`
	Role Role   `json:"role"`
	Exp  int64  `json:"exp"`
}

// MarshalJSON encodes a role as its name so tokens stay readable and stable
// even if the internal enum values change.
func (r Role) MarshalJSON() ([]byte, error) {
	name, ok := roleName(r)
	if !ok {
		return nil, fmt.Errorf("auth: unknown role %d", int(r))
	}
	return json.Marshal(name)
}

// UnmarshalJSON decodes a role name and rejects anything outside the known set.
func (r *Role) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("auth: role: %w", err)
	}
	role, ok := parseRole(name)
	if !ok {
		return fmt.Errorf("auth: unknown role %q", name)
	}
	*r = role
	return nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ,omitempty"`
}

// Issue mints an HS256 token for sub with the given role and lifetime. A
// non-positive ttl produces an already-expired token, which lets callers and
// tests exercise expiry without sleeping.
func Issue(secret, sub string, role Role, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", errors.New("auth: jwt secret is empty")
	}
	if sub == "" {
		return "", errors.New("auth: jwt subject is empty")
	}
	if _, ok := roleName(role); !ok {
		return "", fmt.Errorf("auth: unknown role %d", int(role))
	}

	header, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("auth: marshal header: %w", err)
	}
	payload, err := json.Marshal(Claims{
		Sub:  sub,
		Role: role,
		Exp:  time.Now().Add(ttl).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("auth: marshal claims: %w", err)
	}

	signingInput := encodeSegment(header) + "." + encodeSegment(payload)
	return signingInput + "." + encodeSegment(sign(secret, signingInput)), nil
}

// Verify checks the HS256 signature and expiry of token. Only HS256 is
// accepted; there is deliberately no unsigned or algorithm-negotiation path.
func Verify(secret, token string) (*Claims, error) {
	if secret == "" {
		return nil, fmt.Errorf("%w: jwt secret is empty", ErrInvalidToken)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: token must have three segments", ErrInvalidToken)
	}

	headerBytes, err := decodeSegment(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: decode header: %v", ErrInvalidToken, err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("%w: parse header: %v", ErrInvalidToken, err)
	}
	if header.Alg != "HS256" {
		return nil, fmt.Errorf("%w: unsupported alg %q", ErrInvalidToken, header.Alg)
	}

	signature, err := decodeSegment(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: decode signature: %v", ErrInvalidToken, err)
	}
	if len(signature) == 0 || !hmac.Equal(signature, sign(secret, parts[0]+"."+parts[1])) {
		return nil, fmt.Errorf("%w: signature mismatch", ErrInvalidToken)
	}

	payloadBytes, err := decodeSegment(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: decode claims: %v", ErrInvalidToken, err)
	}
	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: parse claims: %v", ErrInvalidToken, err)
	}
	if claims.Sub == "" {
		return nil, fmt.Errorf("%w: subject is empty", ErrInvalidToken)
	}
	if claims.Exp <= time.Now().Unix() {
		return nil, ErrExpiredToken
	}
	return &claims, nil
}

// Authenticator verifies bearer tokens against a single shared secret. It is a
// value rather than package-level state so tests and reloaded configs never
// share a secret by accident.
type Authenticator struct {
	Secret string
}

// claimsKey is the unexported context key for verified claims. A private type
// keeps other packages from forging claims through context.WithValue.
type claimsKey struct{}

// ClaimsFromContext returns the verified claims that Middleware attached to the
// request context. Handlers use it to attribute an action to the token subject.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(claimsKey{}).(*Claims)
	return claims, ok
}

// Middleware guards next with a Bearer token. A missing, malformed, expired, or
// wrongly signed token is 401; a valid token with a role below needed is 403.
func (a Authenticator) Middleware(next http.Handler, needed Role) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			unauthorized(w)
			return
		}
		claims, err := Verify(a.Secret, token)
		if err != nil {
			unauthorized(w)
			return
		}
		if claims.Role < needed {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey{}, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

func bearerToken(header string) (string, bool) {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || fields[1] == "" {
		return "", false
	}
	return fields[1], true
}

func sign(secret, signingInput string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

func encodeSegment(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeSegment(segment string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(segment)
}

func roleName(role Role) (string, bool) {
	switch role {
	case Denied:
		return "denied", true
	case User:
		return "user", true
	case Admin:
		return "admin", true
	case Maintainer:
		return "maintainer", true
	default:
		return "", false
	}
}

func parseRole(name string) (Role, bool) {
	switch name {
	case "denied":
		return Denied, true
	case "user":
		return User, true
	case "admin":
		return Admin, true
	case "maintainer":
		return Maintainer, true
	default:
		return Denied, false
	}
}
