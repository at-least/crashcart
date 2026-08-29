package sentry

import "testing"

func TestFamily(t *testing.T) {
	cases := []struct{ platform, sdk, want string }{
		{"java", "sentry.java.android", "android"},
		{"java", "sentry.java", "backend"},
		{"cocoa", "sentry.cocoa", "ios"},
		{"other", "sentry.dart.flutter", "flutter"},
		{"other", "sentry.dart", "backend"},
		{"javascript", "sentry.javascript.browser", "web"},
		{"javascript", "sentry.javascript.react-native", "react-native"},
		{"node", "sentry.javascript.bun", "backend"},
		{"native", "sentry.rust", "backend"},
		{"native", "sentry.native", "backend"},
		{"python", "sentry.python", "backend"},
		{"", "", "other"},
	}
	for _, c := range cases {
		if got := Family(c.platform, c.sdk); got != c.want {
			t.Errorf("Family(%q, %q) = %q, want %q", c.platform, c.sdk, got, c.want)
		}
	}
	if !Accepts("flutter", "android") || Accepts("ios", "android") || !Accepts("", "android") || !Accepts("other", "web") {
		t.Error("Accepts rules")
	}
}
