package upload

import "testing"

func TestIsDiscordWebhook(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://discord.com/api/webhooks/123/abc", true},
		{"https://discordapp.com/api/webhooks/123/abc", true},
		{"https://canary.discord.com/api/webhooks/123/abc", true},
		{"https://DISCORD.com/api/webhooks/123/abc", true},
		{"http://discord.com/api/webhooks/123/abc", false},
		{"https://discord.com/channels/1/2", false},
		{"https://evil.example/api/webhooks/123/abc", false},
		{"https://discord.com.evil.example/api/webhooks/123/abc", false},
		{"https://ballchasing.com/api/v2/upload", false},
		{"http://localhost:3000/upload", false},
		{"not a url", false},
	}
	for _, c := range cases {
		if got := isDiscordWebhook(c.url); got != c.want {
			t.Errorf("isDiscordWebhook(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}
