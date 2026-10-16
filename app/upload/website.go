package upload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LEX0RE/rockpload/app/config"
	"github.com/LEX0RE/rockpload/app/rocket_network"
	"github.com/LEX0RE/rockpload/app/tools/logger"
)

type presignedStatusType string

const (
	presignedStatusAttempts = 3
	presignedStatusDelay    = 500 * time.Millisecond

	presignedStatusRejected  presignedStatusType = "rejected"
	presignedStatusProcessed presignedStatusType = "processed"
)

type Website struct {
	config         *config.StorageConfig
	version        string
	knownPlaylists []config.PlaylistFilterEntry
}

type presignedUploadSession struct {
	FileID    string `json:"fileId"`
	FileKey   string `json:"fileKey"`
	UploadURL string `json:"uploadUrl"`
}

type presignedUploadStatus struct {
	ID              string              `json:"id"`
	Status          presignedStatusType `json:"status"`
	RejectionReason *string             `json:"rejectionReason"`
}

func NewWebsite(config *config.StorageConfig, version string, knownPlaylists []config.PlaylistFilterEntry) *Website {
	return &Website{config: config, version: version, knownPlaylists: knownPlaylists}
}

func (w *Website) UploadReplay(filePath string, replayUpload ReplayUpload) error {
	logger.FuncDebug()

	if !w.config.Enabled {
		return nil
	}

	if w.config.TokenStyle != config.NoToken && strings.TrimSpace(w.config.Token) == "" {
		return fmt.Errorf("a token is required to upload to %s", w.config.Name)
	}

	if w.config.PingStyle != config.PingDisabled {
		if err := w.Ping(); err != nil {
			return fmt.Errorf("%s ping failed: %w", w.config.Name, err)
		}
	}

	switch w.config.UploadStyle {
	case config.PresignedSessionUpload:
		return w.uploadPresignedSession(filePath, replayUpload)
	default:
		fallthrough
	case config.MultipartUpload:
		return w.uploadMultipart(filePath, replayUpload)
	}
}

func (w *Website) uploadMultipart(filePath string, replayUpload ReplayUpload) error {
	logger.FuncDebug()

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("Failed to open file: %w", err)
	}
	defer file.Close()

	target := w.config.URL + w.config.ReplayPath
	discord := isDiscordWebhook(target)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Discord's Execute Webhook only keeps attachments, so the version field
	// is left out there and the match goes in as a file of its own.
	if w.version != "" && !discord {
		if err := writer.WriteField("rockploadVersion", w.version); err != nil {
			logger.Rlogger.Warn("Failed to add version field to multipart", "error", err)
		}
	}

	if replayUpload.Replay.Match.MatchGUID != "" {
		jsonData, err := json.Marshal(replayUpload.Replay.Match)
		if err != nil {
			return fmt.Errorf("Failed to marshal match to JSON: %w", err)
		}

		if discord {
			matchPart, err := writer.CreateFormFile(discordMatchField, discordMatchFileName)
			if err == nil {
				_, err = matchPart.Write(jsonData)
			}
			if err != nil {
				return fmt.Errorf("Failed to add match file to multipart: %w", err)
			}
		} else {
			err = writer.WriteField("match", string(jsonData))
			if err != nil {
				return fmt.Errorf("Failed to add JSON field to multipart: %w", err)
			}
		}
	}

	replayField := "file"
	if discord {
		replayField = discordReplayField
	}

	fileName := replayUploadFileName(filePath, w.config.TemplateName, replayUpload, w.knownPlaylists)
	part, err := writer.CreateFormFile(replayField, fileName)
	if err != nil {
		return err
	}

	_, err = io.Copy(part, file)
	if err != nil {
		return err
	}

	err = writer.Close()
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", target, &body)
	if err != nil {
		return err
	}

	if w.config.TokenStyle != config.NoToken && w.config.Token != "" {
		req.Header.Set("Authorization", w.authHeader())
	}

	q := req.URL.Query()
	for key, value := range w.config.URIParams {
		q.Add(key, value)
	}
	if discord {
		// Makes Discord answer 200 with the created message, and report a
		// message it could not save as an error rather than a silent 204.
		q.Set("wait", "true")
	}
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if discord {
		// Discord never answers 201, and has no duplicate status: a webhook
		// accepts every post, so de-duplication is left to whoever reads the
		// channel.
		switch resp.StatusCode {
		case 200, 204:
			logger.Rlogger.Debug("Upload successful")
			return nil
		default:
			return fmt.Errorf("Upload failed: %s\n%s", resp.Status, string(respBody))
		}
	}

	switch resp.StatusCode {
	case 201:
		logger.Rlogger.Debug("Upload successful")

		w.handleRename(fileName, respBody)
	case 409:
		logger.Rlogger.Debug("Duplicate upload")
	default:
		return fmt.Errorf("Upload failed: %s\n%s", resp.Status, string(respBody))
	}

	return nil
}

// Discord webhook storages: a storage whose URL is a Discord webhook
// (https://discord.com/api/webhooks/{id}/{token}) has its uploads shaped the
// way Discord's Execute Webhook expects -- the replay and the match JSON as
// attachments, and 200/204 as success -- so a bot reading the channel can
// collect replays with no server of its own.
const (
	discordReplayField    = "files[0]"
	discordMatchField     = "files[1]"
	discordMatchFileName  = "match.json"
	discordWebhookPathTag = "/api/webhooks/"
)

var discordWebhookHosts = map[string]bool{
	"discord.com":        true,
	"discordapp.com":     true,
	"ptb.discord.com":    true,
	"canary.discord.com": true,
}

func isDiscordWebhook(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	return discordWebhookHosts[strings.ToLower(parsed.Hostname())] &&
		strings.HasPrefix(parsed.Path, discordWebhookPathTag)
}

func (w *Website) uploadPresignedSession(filePath string, replayUpload ReplayUpload) error {
	logger.FuncDebug()

	session, err := w.createUploadSession()
	if err != nil {
		return err
	}

	logger.Rlogger.Debug("Upload session created",
		slog.Any("fileId", session.FileID),
		slog.Any("fileKey", session.FileKey),
		slog.Any("matchGUID", replayUpload.Replay.Match.MatchGUID))

	if err := w.putReplay(session.UploadURL, filePath); err != nil {
		return err
	}

	w.logIngestion(session.FileID)

	return nil
}

func (w *Website) createUploadSession() (*presignedUploadSession, error) {
	logger.FuncDebug()

	req, err := http.NewRequest("POST", w.config.URL+w.config.ReplayPath, nil)
	if err != nil {
		return nil, err
	}

	if w.config.TokenStyle != config.NoToken && w.config.Token != "" {
		req.Header.Set("Authorization", w.authHeader())
	}

	q := req.URL.Query()
	for key, value := range w.config.URIParams {
		q.Add(key, value)
	}
	req.URL.RawQuery = q.Encode()

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read upload session response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("failed to create upload session: %s\n%s", resp.Status, string(body))
	}

	var session presignedUploadSession
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("failed to parse upload session response: %w", err)
	}

	if session.UploadURL == "" {
		return nil, fmt.Errorf("upload session is missing an upload url")
	}

	return &session, nil
}

func (w *Website) putReplay(uploadURL string, filePath string) error {
	logger.FuncDebug()

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to read file size: %w", err)
	}

	req, err := http.NewRequest("PUT", uploadURL, file)
	if err != nil {
		return err
	}

	req.ContentLength = info.Size()

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("replay upload failed: %s\n%s", resp.Status, string(body))
	}

	logger.Rlogger.Debug("Upload successful")

	return nil
}

func (w *Website) logIngestion(fileID string) {
	logger.FuncDebug()

	for attempt := range presignedStatusAttempts {
		if attempt > 0 {
			time.Sleep(presignedStatusDelay)
		}

		status, err := w.uploadStatus(fileID)
		if err != nil {
			logger.Rlogger.Debug("Failed to read upload status", slog.Any("fileId", fileID), slog.Any("err", err))
			continue
		}

		switch status.Status {
		case presignedStatusRejected:
			reason := ""
			if status.RejectionReason != nil {
				reason = *status.RejectionReason
			}

			logger.Rlogger.Error("Replay was rejected", slog.Any("fileId", fileID), slog.Any("reason", reason))

			return
		case presignedStatusProcessed:
			logger.Rlogger.Debug("Replay was processed", slog.Any("fileId", fileID))

			return
		}
	}

	logger.Rlogger.Debug("Replay is still being processed", slog.Any("fileId", fileID))
}

func (w *Website) uploadStatus(fileID string) (*presignedUploadStatus, error) {
	logger.FuncDebug()

	req, err := http.NewRequest("GET", w.config.URL+w.config.PingPath+fileID, nil)
	if err != nil {
		return nil, err
	}

	if w.config.TokenStyle != config.NoToken && w.config.Token != "" {
		req.Header.Set("Authorization", w.authHeader())
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read upload status response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("failed to read upload status: %s\n%s", resp.Status, string(body))
	}

	var status presignedUploadStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("failed to parse upload status response: %w", err)
	}

	return &status, nil
}

func (w *Website) UploadLive(liveStats *rocket_network.LiveStats) error {
	logger.FuncDebug()

	if w.config.LiveStyle == config.LiveDisabled {
		return nil
	}

	payload := struct {
		*rocket_network.LiveStats
		RockploadVersion string `json:"rockploadVersion,omitempty"`
	}{LiveStats: liveStats, RockploadVersion: w.version}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("Failed to marshal live data: %w", err)
	}

	req, err := http.NewRequest("POST", w.config.URL+w.config.LivePath, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("Failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if w.config.TokenStyle != config.NoToken && w.config.Token != "" {
		req.Header.Set("Authorization", w.authHeader())
	}

	q := req.URL.Query()
	for key, value := range w.config.URIParams {
		q.Add(key, value)
	}
	req.URL.RawQuery = q.Encode()

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Live data upload failed: %s\n%s", resp.Status, string(respBody))
	}

	logger.Rlogger.Debug("Live data upload successful")

	return nil
}

func (w *Website) Ping() error {
	logger.FuncDebug()

	if w.config.PingStyle == config.PingDisabled {
		return nil
	}

	switch w.config.PingStyle {
	case config.PingNotFoundIsValid:
		return w.pingNotFoundIsValid()
	default:
		fallthrough
	case config.PingRequiresOK:
		return w.pingRequiresOK()
	}
}

func (w *Website) pingRequiresOK() error {
	logger.FuncDebug()

	req, err := http.NewRequest("GET", w.config.URL+w.config.PingPath, nil)
	if err != nil {
		return err
	}

	if w.config.TokenStyle != config.NoToken && w.config.Token != "" {
		req.Header.Set("Authorization", w.authHeader())
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("Token is invalid: %s\n%s", resp.Status, string(body))
}

func (w *Website) GetConfig() *config.StorageConfig {
	return w.config
}

func (w *Website) handleRename(fileName string, respBody []byte) {
	switch w.config.RenameStyle {
	case config.RenamePatchTitle:
		w.patchTitle(fileName, respBody)
	default:
	}
}

func (w *Website) patchTitle(fileName string, respBody []byte) {
	var data struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(respBody, &data); err != nil {
		logger.Rlogger.Debug("Upload successful but failed to parse response id", "err", err)
		return
	}

	title := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	patchPath := w.config.URL + w.config.RenamePath + data.ID

	patchBody, _ := json.Marshal(map[string]string{"title": title})
	patchReq, err := http.NewRequest("PATCH", patchPath, bytes.NewReader(patchBody))
	if err != nil {
		logger.Rlogger.Debug("Failed to create patch request", "err", err)
		return
	}

	patchReq.Header.Set("Content-Type", "application/json")
	if w.config.TokenStyle != config.NoToken && w.config.Token != "" {
		patchReq.Header.Set("Authorization", w.authHeader())
	}

	patchClient := &http.Client{}
	patchResp, err := patchClient.Do(patchReq)
	if err != nil {
		logger.Rlogger.Debug("Failed to patch replay title", "err", err)
		return
	}
	defer patchResp.Body.Close()

	if patchResp.StatusCode >= 200 && patchResp.StatusCode < 300 {
		logger.Rlogger.Debug("Upload successful and title updated")
		return
	}

	b, _ := io.ReadAll(patchResp.Body)
	logger.Rlogger.Debug("Upload successful but patch failed", "status", patchResp.Status, "body", string(b))
}

func (w *Website) pingNotFoundIsValid() error {
	logger.FuncDebug()

	req, err := http.NewRequest("GET", w.config.URL+w.config.PingPath+w.config.PingProbeID, nil)
	if err != nil {
		return err
	}

	if w.config.TokenStyle != config.NoToken && w.config.Token != "" {
		req.Header.Set("Authorization", w.authHeader())
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)

	return fmt.Errorf("token is invalid: %s\n%s", resp.Status, string(body))
}

func (w *Website) authHeader() string {
	if w.config.TokenStyle == config.BearerToken {
		token := strings.TrimSpace(w.config.Token)
		token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer"))

		return "Bearer " + token
	}

	return w.config.Token
}
