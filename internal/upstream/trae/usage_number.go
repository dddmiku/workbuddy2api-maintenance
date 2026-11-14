package trae

import (
	"encoding/json"
	"strconv"
	"strings"
)

// exactUsageInteger accepts JSON decimal/exponent spellings of a nonnegative
// int64 without rounding. Work and allocation remain bounded by the literal's
// length, even for inputs such as 1e999999999999999999999.
func exactUsageInteger(value json.Number) (int64, bool) {
	s := value.String()
	if s == "" || !json.Valid([]byte(s)) || strings.TrimSpace(s) != s {
		return 0, false
	}
	negative := s[0] == '-'
	if negative {
		s = s[1:]
	}
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	mantissa, exponentText := s, "0"
	if index := strings.IndexAny(s, "eE"); index >= 0 {
		mantissa, exponentText = s[:index], s[index+1:]
	}
	fractionDigits := 0
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		fractionDigits = len(mantissa) - index - 1
		mantissa = mantissa[:index] + mantissa[index+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return 0, !negative
	}
	if negative {
		return 0, false
	}
	exponent, err := strconv.ParseInt(exponentText, 10, 64)
	if err != nil || exponent < int64(fractionDigits)-int64(len(digits)) || exponent > int64(fractionDigits)+19 {
		return 0, false
	}
	scale := exponent - int64(fractionDigits)
	if scale < 0 {
		cut := int64(len(digits)) + scale
		if cut <= 0 || strings.Trim(digits[cut:], "0") != "" {
			return 0, false
		}
		digits = digits[:cut]
	} else {
		if int64(len(digits))+scale > 19 {
			return 0, false
		}
		digits += strings.Repeat("0", int(scale))
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	return n, err == nil
}
