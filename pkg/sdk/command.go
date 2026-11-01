// Package sdk provides the contract shared by Aether and its satellites:
// command matching, typed argument conversion, timezone handling, webhook
// parsing, outbound responses, sessions, and RFC6902 patches.
package sdk

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// CommandSpec describes one satellite command and the patterns that select it.
// Patterns include the trigger and are anchored in full, per plan §3.6.
type CommandSpec struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Patterns []string `json:"patterns"`
	Args     []ArgDef `json:"args,omitempty"`
}

// ArgDef declares one typed argument extracted from a command pattern.
// Type is one of "string", "int", "enum", or "datetime".
type ArgDef struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Enum []string `json:"enum,omitempty"`
}

// compiledSpec holds the regexes for a pattern list. It is cached by pattern
// content so a spec is compiled once, not once per message.
type compiledSpec struct {
	regexps []*regexp.Regexp
}

var (
	compiledSpecs sync.Map // cache key -> *compiledSpec
	compileMu     sync.Mutex
)

// MatchCommand matches input against spec.Patterns in declaration order and
// returns the raw named groups of the first match. Patterns include the trigger,
// are anchored at both ends, treat whitespace runs as \s+, and compile each
// <arg> to a lazy named group. An empty or invalid pattern never matches.
func MatchCommand(input string, spec CommandSpec) (map[string]string, bool) {
	compiled := compileSpec(spec.Patterns)
	for _, re := range compiled.regexps {
		if re == nil {
			continue
		}

		match := re.FindStringSubmatch(input)
		if match == nil {
			continue
		}

		args := make(map[string]string, len(re.SubexpNames()))
		for i, name := range re.SubexpNames() {
			if i == 0 || name == "" {
				continue
			}
			args[name] = match[i]
		}
		return args, true
	}
	return nil, false
}

// compileSpec returns the cached compiled form for patterns, compiling it on
// first use. The cache is keyed by exact pattern content and order.
func compileSpec(patterns []string) *compiledSpec {
	key := patternCacheKey(patterns)
	if cached, ok := compiledSpecs.Load(key); ok {
		return cached.(*compiledSpec)
	}

	compileMu.Lock()
	defer compileMu.Unlock()

	if cached, ok := compiledSpecs.Load(key); ok {
		return cached.(*compiledSpec)
	}

	spec := &compiledSpec{regexps: make([]*regexp.Regexp, len(patterns))}
	for i, pattern := range patterns {
		re, err := compilePattern(pattern)
		if err != nil {
			// Invalid patterns are inert. Registration is responsible for
			// rejecting them before they reach the router.
			continue
		}
		spec.regexps[i] = re
	}
	compiledSpecs.Store(key, spec)
	return spec
}

// patternCacheKey encodes the pattern list unambiguously, including empty and
// embedded separator characters.
func patternCacheKey(patterns []string) string {
	var b strings.Builder
	for _, pattern := range patterns {
		b.WriteString(strconv.Itoa(len(pattern)))
		b.WriteByte(':')
		b.WriteString(pattern)
	}
	return b.String()
}

// compilePattern turns one human-readable pattern into an anchored regexp.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	expr, err := patternExpression(pattern)
	if err != nil {
		return nil, err
	}
	return regexp.Compile(expr)
}

// patternExpression builds ^…$ from a pattern such as
// "remind me <msg> in <num> <unit>".
func patternExpression(pattern string) (string, error) {
	var b strings.Builder
	b.WriteString("^")
	seen := make(map[string]bool)

	for i := 0; i < len(pattern); {
		if pattern[i] == '<' {
			end := strings.IndexByte(pattern[i:], '>')
			if end < 0 {
				// An unmatched '<' is ordinary text.
				b.WriteString(literalExpression(pattern[i:]))
				break
			}

			name := pattern[i+1 : i+end]
			if !validArgName(name) {
				return "", fmt.Errorf("sdk: invalid argument name %q in pattern %q", name, pattern)
			}
			if seen[name] {
				return "", fmt.Errorf("sdk: duplicate argument %q in pattern %q", name, pattern)
			}
			seen[name] = true

			b.WriteString("(?P<")
			b.WriteString(name)
			b.WriteString(">.+?)")
			i += end + 1
			continue
		}

		next := strings.IndexByte(pattern[i:], '<')
		if next < 0 {
			b.WriteString(literalExpression(pattern[i:]))
			break
		}
		b.WriteString(literalExpression(pattern[i : i+next]))
		i += next
	}

	b.WriteString("$")
	return b.String(), nil
}

// literalExpression escapes literal text and collapses whitespace runs to \s+.
func literalExpression(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			for i < len(s) {
				next, n := utf8.DecodeRuneInString(s[i:])
				if !unicode.IsSpace(next) {
					break
				}
				i += n
			}
			b.WriteString(`\s+`)
			continue
		}

		b.WriteString(regexp.QuoteMeta(string(r)))
		i += size
	}
	return b.String()
}

// validArgName accepts names that Go's regexp package can use as a named group.
func validArgName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
