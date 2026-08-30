package ingest

import "regexp"

var (
	emailRe = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
	// Digit groups joined by separators (or a leading +/parenthesised area
	// code). A bare digit run is NOT matched — epoch timestamps, order ids
	// and numeric error codes must survive redaction.
	phoneRe = regexp.MustCompile(`(?:\+\d{1,3}[-.\s]?)?(?:\(\d{3}\)|\d{3})[-.\s]\d{3,4}[-.\s]?\d{4}`)
	cardRe  = regexp.MustCompile(`\d{4}[-\s]\d{4}[-\s]\d{4}[-\s]\d{4}`)
	digit   = regexp.MustCompile(`\d`)
)

// ipAddressField matches the SDK's user.ip_address (and any other
// ip_address key) in a raw payload: an IP is not caught by the text rules
// (no separators a phone number has), and it is the datum redaction is
// most often turned on for.
var ipAddressField = regexp.MustCompile(`"ip_address"\s*:\s*"[^"]*"`)

// RedactRaw scrubs a raw event document: the text rules over the whole
// body, plus the ip_address fields.
func RedactRaw(s string) string {
	return ipAddressField.ReplaceAllString(RedactText(s), `"ip_address":"[redacted]"`)
}

// RedactText scrubs emails, card numbers and phone-like sequences.
func RedactText(s string) string {
	s = emailRe.ReplaceAllString(s, "[REDACTED]")
	s = replaceBounded(s, cardRe)
	s = replaceBounded(s, phoneRe)
	return s
}

// replaceBounded emulates (?<!\d)…(?!\d) — Go's RE2 has no lookaround.
func replaceBounded(s string, re *regexp.Regexp) string {
	locs := re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var out []byte
	last := 0
	for _, l := range locs {
		start, end := l[0], l[1]
		if (start > 0 && digit.MatchString(s[start-1:start])) || (end < len(s) && digit.MatchString(s[end:end+1])) {
			continue
		}
		out = append(out, s[last:start]...)
		out = append(out, "[REDACTED]"...)
		last = end
	}
	out = append(out, s[last:]...)
	return string(out)
}

// RedactUserID keeps only the first and last 4 characters of long ids.
func RedactUserID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) > 8 {
		return id[:4] + "****" + id[len(id)-4:]
	}
	return "****"
}

// RedactTags blanks sensitive keys and scrubs the rest.
func RedactTags(tags map[string]string) map[string]string {
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		switch k {
		case "email", "phone", "password":
			out[k] = "[REDACTED]"
		default:
			out[k] = RedactText(v)
		}
	}
	return out
}
