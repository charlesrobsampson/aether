package sdk

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ConvertArgs turns the raw strings returned by MatchCommand into the typed
// argument map posted to a satellite (plan §3.6). It applies each ArgDef.Type:
//
//   - "string"   -> string
//   - "int"      -> int
//   - "enum"     -> string, validated against ArgDef.Enum
//   - "datetime" -> time.Time in UTC, interpreted in loc
//
// A missing argument, an invalid value, an unknown type, or a nil location is
// an error. Arguments are emitted in the order declared by spec.Args; raw keys
// with no ArgDef are ignored.
func ConvertArgs(spec CommandSpec, raw map[string]string, loc *time.Location) (map[string]any, error) {
	if loc == nil {
		return nil, fmt.Errorf("sdk: ConvertArgs: nil location")
	}

	now := time.Now()
	out := make(map[string]any, len(spec.Args))

	for _, arg := range spec.Args {
		value, ok := raw[arg.Name]
		if !ok {
			return nil, fmt.Errorf("sdk: ConvertArgs: missing argument %q", arg.Name)
		}

		switch arg.Type {
		case "string":
			out[arg.Name] = strings.TrimSpace(value)

		case "int":
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("sdk: ConvertArgs: argument %q is not an int: %w", arg.Name, err)
			}
			out[arg.Name] = n

		case "enum":
			v := strings.TrimSpace(value)
			if !enumContains(arg.Enum, v) {
				return nil, fmt.Errorf("sdk: ConvertArgs: argument %q value %q is not one of %v", arg.Name, v, arg.Enum)
			}
			out[arg.Name] = v

		case "datetime":
			t, err := toUTC(value, loc, now)
			if err != nil {
				return nil, fmt.Errorf("sdk: ConvertArgs: argument %q: %w", arg.Name, err)
			}
			out[arg.Name] = t

		default:
			return nil, fmt.Errorf("sdk: ConvertArgs: argument %q has unknown type %q", arg.Name, arg.Type)
		}
	}

	return out, nil
}

func enumContains(enum []string, value string) bool {
	for _, allowed := range enum {
		if value == allowed {
			return true
		}
	}
	return false
}
