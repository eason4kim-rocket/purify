package compiler

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/araddon/dateparse"
)

type transformFunc func(string) (string, error)

type namedTransform struct {
	name  string
	apply transformFunc
}

var transformRegistry = map[string]transformFunc{
	"trim": func(value string) (string, error) {
		return strings.TrimSpace(value), nil
	},
	"collapse_ws": func(value string) (string, error) {
		return strings.Join(strings.Fields(value), " "), nil
	},
	"lower": func(value string) (string, error) {
		return strings.ToLower(value), nil
	},
	"parse_number": func(value string) (string, error) {
		return canonicalNumber(value)
	},
	"parse_date": func(value string) (string, error) {
		return canonicalDate(value)
	},
	"currency_amount": func(value string) (string, error) {
		number, err := currencyNumber(value)
		if err != nil {
			return "", err
		}
		return canonicalNumber(number)
	},
}

func convertValue(value, valueType string) (any, error) {
	switch valueType {
	case TypeString:
		return value, nil
	case TypeNumber:
		number, err := canonicalNumber(value)
		if err != nil {
			return nil, err
		}
		return json.Number(number), nil
	case TypeBoolean:
		return parseBoolean(value)
	case TypeDate:
		return canonicalDate(value)
	default:
		return nil, fmt.Errorf("unsupported type %q", valueType)
	}
}

func canonicalNumber(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\u00a0", "")
	value = strings.ReplaceAll(value, "\u202f", "")
	value = strings.ReplaceAll(value, "\u2212", "-")
	value = strings.ReplaceAll(value, "'", "")
	value = strings.ReplaceAll(value, "’", "")
	value = strings.ReplaceAll(value, " ", "")
	if value == "" {
		return "", errors.New("number is empty")
	}

	negativeParentheses := strings.HasPrefix(value, "(") && strings.HasSuffix(value, ")")
	if negativeParentheses {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	if strings.HasPrefix(value, "+") {
		value = value[1:]
	}
	if negativeParentheses {
		if strings.HasPrefix(value, "-") {
			return "", errors.New("number has conflicting signs")
		}
		value = "-" + value
	}

	value = normalizeSeparators(value)
	value = normalizeJSONNumber(value)
	if value == "" || !json.Valid([]byte(value)) {
		return "", fmt.Errorf("invalid number %q", value)
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return "", fmt.Errorf("invalid number %q: %w", value, err)
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return "", fmt.Errorf("number %q is not finite", value)
	}
	return value, nil
}

func normalizeSeparators(value string) string {
	comma := strings.LastIndexByte(value, ',')
	dot := strings.LastIndexByte(value, '.')
	switch {
	case comma >= 0 && dot >= 0:
		if comma > dot {
			value = strings.ReplaceAll(value, ".", "")
			value = strings.ReplaceAll(value, ",", ".")
			return value
		}
		return strings.ReplaceAll(value, ",", "")
	case comma >= 0:
		if strings.Count(value, ",") > 1 || separatorLooksGrouped(value, comma) {
			return strings.ReplaceAll(value, ",", "")
		}
		return strings.ReplaceAll(value, ",", ".")
	case dot >= 0 && strings.Count(value, ".") > 1:
		last := strings.LastIndexByte(value, '.')
		if allEarlierGroups(value, '.', last) {
			return strings.ReplaceAll(value, ".", "")
		}
		return strings.ReplaceAll(value[:last], ".", "") + value[last:]
	default:
		return value
	}
}

func separatorLooksGrouped(value string, separator int) bool {
	digitsAfter := len(value) - separator - 1
	if exponent := strings.IndexAny(value[separator+1:], "eE"); exponent >= 0 {
		digitsAfter = exponent
	}
	integer := strings.TrimLeft(value[:separator], "+-0")
	return digitsAfter == 3 && integer != ""
}

func allEarlierGroups(value string, separator byte, last int) bool {
	unsigned := strings.TrimLeft(value[:last], "+-")
	parts := strings.Split(unsigned, string(separator))
	if len(parts) < 2 || len(parts[0]) == 0 || len(parts[0]) > 3 {
		return false
	}
	for _, part := range parts[1:] {
		if len(part) != 3 {
			return false
		}
	}
	return len(value)-last-1 == 3
}

func normalizeJSONNumber(value string) string {
	sign := ""
	if strings.HasPrefix(value, "-") {
		sign = "-"
		value = value[1:]
	}

	exponent := ""
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		exponent = value[index:]
		value = value[:index]
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return ""
	}
	integer := strings.TrimLeft(parts[0], "0")
	if integer == "" {
		integer = "0"
	}
	value = integer
	if len(parts) == 2 {
		value += "." + parts[1]
	}
	return sign + value + exponent
}

func canonicalDate(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("date is empty")
	}
	parsed, err := dateparse.ParseIn(value, time.UTC)
	if err != nil {
		return "", fmt.Errorf("invalid date %q: %w", value, err)
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

func parseBoolean(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "y", "on":
		return true, nil
	case "false", "0", "no", "n", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", value)
	}
}

func currencyNumber(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("currency amount is empty")
	}
	parenthesized := strings.HasPrefix(value, "(") && strings.HasSuffix(value, ")")
	if parenthesized {
		value = value[1 : len(value)-1]
	}

	var number strings.Builder
	started := false
	digits := 0
	negative := false
	for _, r := range value {
		if !started {
			switch {
			case r == '-' || r == '\u2212':
				negative = true
			case r == '+':
				// A leading positive sign does not affect the amount.
			case unicode.IsDigit(r):
				started = true
				digits++
				number.WriteRune(r)
			case r == '.' || r == ',':
				started = true
				number.WriteRune('0')
				number.WriteRune(r)
			}
			continue
		}

		switch {
		case unicode.IsDigit(r):
			digits++
			number.WriteRune(r)
		case r == '.' || r == ',' || r == '\'' || r == '’' || unicode.IsSpace(r):
			number.WriteRune(r)
		default:
			if digits > 0 {
				goto complete
			}
		}
	}

complete:
	if digits == 0 {
		return "", fmt.Errorf("currency amount %q contains no number", value)
	}
	result := strings.TrimSpace(number.String())
	if parenthesized || negative {
		result = "-" + result
	}
	return result, nil
}
