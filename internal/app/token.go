package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
)

// defaultTokenTTL is deliberately long enough for an operator to use a minted
// token interactively, but not unbounded.
const defaultTokenTTL = 24 * time.Hour

// RunToken implements `aether token --sub <phone> --role <role>`: it prints an
// HS256 JWT signed with the configured secret. Operators and tests therefore
// need no external issuer (plan §3.4).
func RunToken(args []string) error {
	flags := flag.NewFlagSet("token", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	sub := flags.String("sub", "", "token subject (phone number)")
	roleName := flags.String("role", "admin", "role: user, admin, or maintainer")
	ttl := flags.Duration("ttl", defaultTokenTTL, "token lifetime")
	configDir := flags.String("config-dir", "", "config root (default AETHER_CONFIG_DIR or ~/.config/aether)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("app: token: %w", err)
	}
	if strings.TrimSpace(*sub) == "" {
		return errors.New("app: token: --sub is required")
	}

	role, ok := roleFromName(*roleName)
	if !ok {
		return fmt.Errorf("app: token: unknown role %q", *roleName)
	}

	root := strings.TrimSpace(*configDir)
	if root == "" {
		root = config.ResolveRoot()
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return fmt.Errorf("app: token: %w", err)
	}
	if cfg.System.Auth.JWTSecret == "" {
		return errors.New("app: token: jwt secret is empty")
	}

	token, err := auth.Issue(cfg.System.Auth.JWTSecret, strings.TrimSpace(*sub), role, *ttl)
	if err != nil {
		return fmt.Errorf("app: token: %w", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, token); err != nil {
		return fmt.Errorf("app: token: write output: %w", err)
	}
	return nil
}

// roleFromName maps the CLI spelling onto auth.Role.
func roleFromName(name string) (auth.Role, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "denied":
		return auth.Denied, true
	case "user":
		return auth.User, true
	case "admin":
		return auth.Admin, true
	case "maintainer":
		return auth.Maintainer, true
	default:
		return auth.Denied, false
	}
}
