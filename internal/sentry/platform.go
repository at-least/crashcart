package sentry

import "strings"

// Platform families a project can declare. SDKs report a raw `platform`
// ("java", "cocoa", "other", …) that only makes sense together with the SDK
// name; a family is the thing a user actually means by "the iOS app".
var Families = []string{"ios", "android", "flutter", "react-native", "web", "backend", "other"}

// IsFamily reports whether s is a declared platform family.
func IsFamily(s string) bool {
	for _, f := range Families {
		if f == s {
			return true
		}
	}
	return false
}

// Family maps an event's raw platform + SDK name to a family.
func Family(platform, sdkName string) string {
	sdk := strings.ToLower(sdkName)
	switch {
	case strings.Contains(sdk, "flutter"):
		return "flutter"
	case strings.Contains(sdk, "react-native") || strings.Contains(sdk, "reactnative"):
		return "react-native"
	case strings.Contains(sdk, ".android") || strings.HasSuffix(sdk, "android"):
		return "android"
	}
	switch strings.ToLower(platform) {
	case "cocoa", "ios", "macos", "swift", "objc", "apple":
		return "ios"
	case "android":
		return "android"
	case "javascript":
		if sdk == "" || strings.Contains(sdk, "browser") || strings.Contains(sdk, "react") || strings.Contains(sdk, "vue") ||
			strings.Contains(sdk, "angular") || strings.Contains(sdk, "svelte") || strings.Contains(sdk, "nextjs") || strings.Contains(sdk, "astro") {
			return "web"
		}
		return "backend" // javascript platform from a server SDK (electron main, deno)
	case "java":
		return "backend" // plain JVM; the Android SDK was matched above
	case "node", "go", "python", "csharp", "ruby", "php", "elixir", "rust", "perl", "native", "powershell", "dotnet":
		return "backend"
	case "other":
		if strings.Contains(sdk, "dart") {
			return "backend"
		}
	}
	return "other"
}

// Accepts reports whether a project declared as family should expect events
// of the given family. Cross-platform families also carry their host
// platforms' native unhandled; "other" accepts anything.
func Accepts(project, event string) bool {
	if project == "" || project == "other" || project == event {
		return true
	}
	switch project {
	case "flutter":
		return event == "ios" || event == "android" || event == "other"
	case "react-native":
		return event == "ios" || event == "android" || event == "web"
	}
	return false
}
