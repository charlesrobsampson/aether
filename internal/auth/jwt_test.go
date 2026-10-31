package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const jwtTestSecret = "test-secret-not-for-production"

func TestJWT(t *testing.T) {
	t.Run("issue and verify", func(t *testing.T) {
		token, err := Issue(jwtTestSecret, "+15555550101", Admin, time.Hour)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}

		claims, err := Verify(jwtTestSecret, token)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if claims.Sub != "+15555550101" {
			t.Errorf("Sub = %q, want %q", claims.Sub, "+15555550101")
		}
		if claims.Role != Admin {
			t.Errorf("Role = %v, want %v", claims.Role, Admin)
		}
		if claims.Exp <= time.Now().Unix() {
			t.Errorf("Exp = %d, want a future timestamp", claims.Exp)
		}

		// The wire format carries the role by name, not as the Go enum value.
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatalf("token has %d parts, want 3", len(parts))
		}
		header := decodeJWTPart(t, parts[0])
		if got := header["alg"]; got != "HS256" {
			t.Errorf("alg = %v, want HS256", got)
		}
		payload := decodeJWTPart(t, parts[1])
		if got := payload["role"]; got != "admin" {
			t.Errorf("role = %v, want admin", got)
		}
		if got := payload["sub"]; got != "+15555550101" {
			t.Errorf("sub = %v, want +15555550101", got)
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		token, err := Issue(jwtTestSecret, "+15555550101", Admin, -time.Minute)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if _, err := Verify(jwtTestSecret, token); !errors.Is(err, ErrExpiredToken) {
			t.Fatalf("Verify error = %v, want ErrExpiredToken", err)
		}
	})

	t.Run("wrong secret is rejected", func(t *testing.T) {
		token, err := Issue(jwtTestSecret, "+15555550101", Admin, time.Hour)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if _, err := Verify("another-secret", token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Verify error = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("alg none is rejected", func(t *testing.T) {
		token := unsignedToken(t, "none", map[string]any{
			"sub":  "+15555550101",
			"role": "admin",
			"exp":  time.Now().Add(time.Hour).Unix(),
		})
		if _, err := Verify(jwtTestSecret, token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Verify error = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("unknown role is rejected", func(t *testing.T) {
		token := signedToken(t, jwtTestSecret, "HS256", map[string]any{
			"sub":  "+15555550101",
			"role": "superuser",
			"exp":  time.Now().Add(time.Hour).Unix(),
		})
		if _, err := Verify(jwtTestSecret, token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Verify error = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("malformed tokens are rejected", func(t *testing.T) {
		valid, err := Issue(jwtTestSecret, "+15555550101", Admin, time.Hour)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		parts := strings.Split(valid, ".")

		tests := []struct {
			name  string
			token string
		}{
			{name: "empty", token: ""},
			{name: "one part", token: "abc"},
			{name: "two parts", token: parts[0] + "." + parts[1]},
			{name: "four parts", token: valid + ".extra"},
			{name: "bad header encoding", token: "!!!." + parts[1] + "." + parts[2]},
			{name: "bad payload encoding", token: parts[0] + ".!!!." + parts[2]},
			{name: "bad signature encoding", token: parts[0] + "." + parts[1] + ".!!!"},
			{name: "empty signature", token: parts[0] + "." + parts[1] + "."},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := Verify(jwtTestSecret, tc.token); !errors.Is(err, ErrInvalidToken) {
					t.Fatalf("Verify(%q) error = %v, want ErrInvalidToken", tc.token, err)
				}
			})
		}
	})

	t.Run("middleware", func(t *testing.T) {
		adminToken := mustIssue(t, jwtTestSecret, "+15555550101", Admin, time.Hour)
		maintainerToken := mustIssue(t, jwtTestSecret, "+15555550103", Maintainer, time.Hour)
		expiredToken := mustIssue(t, jwtTestSecret, "+15555550101", Admin, -time.Minute)
		wrongSecretToken := mustIssue(t, "another-secret", "+15555550101", Admin, time.Hour)

		tests := []struct {
			name       string
			header     string
			needed     Role
			wantStatus int
			wantCalled bool
		}{
			{name: "valid admin", header: "Bearer " + adminToken, needed: Admin, wantStatus: http.StatusOK, wantCalled: true},
			{name: "admin satisfies lower role", header: "Bearer " + adminToken, needed: User, wantStatus: http.StatusOK, wantCalled: true},
			{name: "maintainer satisfies admin", header: "Bearer " + maintainerToken, needed: Admin, wantStatus: http.StatusOK, wantCalled: true},
			{name: "wrong role", header: "Bearer " + adminToken, needed: Maintainer, wantStatus: http.StatusForbidden, wantCalled: false},
			{name: "missing header", header: "", needed: Admin, wantStatus: http.StatusUnauthorized, wantCalled: false},
			{name: "wrong scheme", header: "Basic " + adminToken, needed: Admin, wantStatus: http.StatusUnauthorized, wantCalled: false},
			{name: "bearer without token", header: "Bearer", needed: Admin, wantStatus: http.StatusUnauthorized, wantCalled: false},
			{name: "bearer with extra field", header: "Bearer " + adminToken + " extra", needed: Admin, wantStatus: http.StatusUnauthorized, wantCalled: false},
			{name: "malformed token", header: "Bearer not-a-jwt", needed: Admin, wantStatus: http.StatusUnauthorized, wantCalled: false},
			{name: "expired token", header: "Bearer " + expiredToken, needed: Admin, wantStatus: http.StatusUnauthorized, wantCalled: false},
			{name: "wrong secret", header: "Bearer " + wrongSecretToken, needed: Admin, wantStatus: http.StatusUnauthorized, wantCalled: false},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				called := false
				next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					w.WriteHeader(http.StatusOK)
				})

				req := httptest.NewRequest(http.MethodPost, "/message", nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				rec := httptest.NewRecorder()

				Authenticator{Secret: jwtTestSecret}.Middleware(next, tc.needed).ServeHTTP(rec, req)

				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
				}
				if called != tc.wantCalled {
					t.Fatalf("next called = %v, want %v", called, tc.wantCalled)
				}
			})
		}
	})

	t.Run("issuer rejects empty inputs", func(t *testing.T) {
		if _, err := Issue("", "+15555550101", Admin, time.Hour); err == nil {
			t.Fatal("Issue with an empty secret succeeded")
		}
		if _, err := Issue(jwtTestSecret, "", Admin, time.Hour); err == nil {
			t.Fatal("Issue with an empty subject succeeded")
		}
		if _, err := Issue(jwtTestSecret, "+15555550101", Role(99), time.Hour); err == nil {
			t.Fatal("Issue with an unknown role succeeded")
		}
	})
}

func mustIssue(t *testing.T, secret, sub string, role Role, ttl time.Duration) string {
	t.Helper()
	token, err := Issue(secret, sub, role, ttl)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return token
}

func decodeJWTPart(t *testing.T, part string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode part: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal part: %v", err)
	}
	return out
}

func unsignedToken(t *testing.T, alg string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": alg, "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + "."
}

func signedToken(t *testing.T, secret, alg string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": alg, "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
