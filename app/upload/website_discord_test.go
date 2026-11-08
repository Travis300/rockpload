package upload

import (
	"bytes"
	"io"
	"mime/multipart"
	"testing"
)

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

// The bot reading the channel matches these keys exactly, so the JSON is
// compared byte for byte.
func TestDiscordUploaderFileSaysWhoUploaded(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	replayUpload := ReplayUpload{
		PlayerName: "wzpig",
		PlayerIDs:  []string{"Epic|abc|0", "Steam|76561198440303298|0"},
	}
	if err := writeDiscordUploader(writer, replayUpload); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	part, err := multipart.NewReader(&body, writer.Boundary()).NextPart()
	if err != nil {
		t.Fatalf("no uploader part: %v", err)
	}
	if part.FormName() != "files[2]" || part.FileName() != "uploader.json" {
		t.Errorf("part is %q / %q, want files[2] / uploader.json",
			part.FormName(), part.FileName())
	}
	got, _ := io.ReadAll(part)
	want := `{"PlayerName":"wzpig","PlayerIDs":["Epic|abc|0","Steam|76561198440303298|0"]}`
	if string(got) != want {
		t.Errorf("uploader.json = %s, want %s", got, want)
	}
}

func TestDiscordUploaderFileLeftOutWithoutIDs(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writeDiscordUploader(writer, ReplayUpload{PlayerName: "wzpig"}); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	if _, err := multipart.NewReader(&body, writer.Boundary()).NextPart(); err != io.EOF {
		t.Errorf("want no parts, got err %v", err)
	}
}
