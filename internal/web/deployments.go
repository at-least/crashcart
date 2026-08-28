package web

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Deployment is one entry of DEPLOYMENTS:
//
//	DEPLOYMENTS="iOS|https://ios.example.com|key1,Android|https://android.example.com|key2"
//
// Each is a separate CrashCart instance. When set, `/` renders a portal
// with per-platform cards linking into each instance's `/<slug>/dashboard`.
type Deployment struct {
	Name string
	URL  string // origin, no trailing slash
	Key  string // API key for server-side stats fetches ("" = none)
	Slug string // URL segment: "iOS" → "ios"
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// ParseDeployments parses the env string.
func ParseDeployments(raw string) []Deployment {
	var out []Deployment
	seen := map[string]bool{}
	for i, part := range strings.Split(raw, ",") {
		fields := strings.Split(part, "|")
		for j := range fields {
			fields[j] = strings.TrimSpace(fields[j])
		}
		if len(fields) < 2 || fields[0] == "" || fields[1] == "" {
			continue
		}
		d := Deployment{Name: fields[0], URL: strings.TrimRight(fields[1], "/")}
		if len(fields) > 2 {
			d.Key = fields[2]
		}
		slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(d.Name), "-"), "-")
		if slug == "" {
			slug = "p" + strconv.Itoa(i+1)
		}
		if seen[slug] {
			slug += "-" + strconv.Itoa(i+1)
		}
		seen[slug] = true
		d.Slug = slug
		out = append(out, d)
	}
	return out
}

// SelfIndex finds the deployment whose origin matches the request, or -1.
func SelfIndex(deps []Deployment, scheme, host string) int {
	origin := strings.ToLower(scheme + "://" + host)
	for i, d := range deps {
		u, err := url.Parse(d.URL)
		if err != nil {
			continue
		}
		if strings.ToLower(u.Scheme+"://"+u.Host) == origin {
			return i
		}
	}
	return -1
}
