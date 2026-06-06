package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMimoBase          = "https://token-plan-cn.xiaomimimo.com/v1"
	defaultMimoModel         = "mimo-v2.5-pro"
	proMimoModel             = "mimo-v2.5-pro"
	visionMimoModel          = "mimo-v2.5"
	defaultProxyPort         = "9876"
	defaultResponseStoreSize = 1000
	defaultMaxConcurrent     = 4
	defaultMinIntervalMS     = 600
	describeImageToolName    = "describe_image"
	maxDescribeRounds        = 2
)

// describeImageTool 是 proxy 主动注入的"读图"工具，OpenAI Chat Completions function 格式。
// 当 mimo-v2.5-pro 收到带图请求时，会被引导调起这个工具，proxy 内部再用 mimo-v2.5 读图，
// 把文字结论作为 role=tool 回灌给 pro，让 pro 用自然语言回答 Codex。
var describeImageTool = map[string]interface{}{
	"type": "function",
	"function": map[string]interface{}{
		"name": describeImageToolName,
		"description": "Read the contents of an image and return a textual description. " +
			"Call this when the user's request includes or references an image. " +
			"Use the returned description to answer the user's question.",
		"parameters": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"image_url": map[string]interface{}{
					"type":        "string",
					"description": "URL or data URI of the image to describe",
				},
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "What to look for in the image (e.g. 'describe in detail', 'read the text')",
				},
			},
			"required": []string{"image_url", "prompt"},
		},
	},
}

var (
	mimoBase             = envOrDefault("MIMO_BASE_URL", defaultMimoBase)
	mimoKey              = strings.TrimSpace(os.Getenv("MIMO_API_KEY"))
	mimoModel            = envOrDefault("MIMO_MODEL", defaultMimoModel)
	proxyPort            = envOrDefault("MIMO_PROXY_PORT", defaultProxyPort)
	client               = &http.Client{}
	limiter              = newUpstreamLimiter()
	responseStore        = newResponseStore(envIntOrDefault("MIMO_PROXY_RESPONSE_STORE_MAX", defaultResponseStoreSize))
	skipCCSwitchSync     = strings.EqualFold(strings.TrimSpace(os.Getenv("MIMO_PROXY_SKIP_CC_SWITCH_SYNC")), "true")
	homeDir              = userHomeDir()
	ccSwitchSettingsPath = envOrDefault("CC_SWITCH_SETTINGS_PATH", filepath.Join(homeDir, ".cc-switch", "settings.json"))
	ccSwitchDBPath       = envOrDefault("CC_SWITCH_DB_PATH", filepath.Join(homeDir, ".cc-switch", "cc-switch.db"))
	codexConfigPath      = envOrDefault("CODEX_CONFIG_PATH", filepath.Join(homeDir, ".codex", "config.toml"))
	codexAuthPath        = envOrDefault("CODEX_AUTH_PATH", filepath.Join(homeDir, ".codex", "auth.json"))
)

func applyModelFlag() {
	useV25 := flag.Bool("v2.5", false, "use mimo-v2.5 for text requests")
	useV25Pro := flag.Bool("v2.5-pro", false, "use mimo-v2.5-pro for text requests")
	syncOnly := flag.Bool("sync-only", false, "sync cc switch and Codex config, then exit")
	flag.Parse()

	if *useV25 && *useV25Pro {
		log.Fatal("[CCMimoLink] --v2.5 and --v2.5-pro cannot be used together")
	}

	switch {
	case *useV25:
		mimoModel = defaultMimoModel
	case *useV25Pro:
		mimoModel = proMimoModel
	}
	mimoKey = strings.TrimSpace(os.Getenv("MIMO_API_KEY"))
	if *syncOnly {
		setupLogging()
		if err := runStartupSyncOnly(); err != nil {
			log.Fatal("[CCMimoLink] startup sync failed: ", err)
		}
		os.Exit(0)
	}
}

func envOrDefault(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func envBoolOrDefault(name string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch v {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func userHomeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	// Fallback for unusual environments.
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/tmp"
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

type UpstreamLimiter struct {
	sem      chan struct{}
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

type StoredResponse struct {
	ResponseObject  map[string]interface{}
	ChatMessages    []ChatMessage
	ProviderModel   string
	Thinking        json.RawMessage
	ParallelTools   *bool
	StoredAtUnixSec int64
}

type ResponseStore struct {
	maxEntries int
	mu         sync.Mutex
	order      []string
	items      map[string]StoredResponse
}

type CCSwitchSettings struct {
	CurrentProviderCodex string `json:"currentProviderCodex"`
}

type ccSwitchProviderRecord struct {
	ID             string
	SettingsConfig string
	Name           string
	Config         string
}

type codexConfigUpdate struct {
	Path       string
	BackupPath string
	APIKey     string
	BaseURL    string
	Updated    bool
}

type codexAuthUpdate struct {
	Path    string
	APIKey  string
	Updated bool
}

func newResponseStore(maxEntries int) *ResponseStore {
	if maxEntries <= 0 {
		maxEntries = defaultResponseStoreSize
	}
	return &ResponseStore{
		maxEntries: maxEntries,
		items:      make(map[string]StoredResponse),
	}
}

func (s *ResponseStore) Put(id string, value StoredResponse) {
	if strings.TrimSpace(id) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[id]; !exists {
		s.order = append(s.order, id)
	}
	s.items[id] = value
	for s.maxEntries > 0 && len(s.order) > s.maxEntries {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.items, oldest)
	}
}

func (s *ResponseStore) Get(id string) (StoredResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.items[id]
	return value, ok
}

func cloneChatMessages(messages []ChatMessage) []ChatMessage {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]ChatMessage, len(messages))
	copy(cloned, messages)
	return cloned
}

func summarizeReasoning(reasoning string) string {
	if len(reasoning) > 500 {
		return reasoning[:500]
	}
	return reasoning
}

func toolCallsFromResponseItems(items []map[string]interface{}) []map[string]interface{} {
	if len(items) == 0 {
		return nil
	}
	converted := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		name := stringValue(item["name"])
		arguments := stringValue(item["arguments"])
		callID := stringValue(item["call_id"])
		if callID == "" {
			callID = stringValue(item["id"])
		}
		if name == "" || callID == "" {
			continue
		}
		converted = append(converted, map[string]interface{}{
			"id":   callID,
			"type": "function",
			"function": map[string]string{
				"name":      name,
				"arguments": arguments,
			},
		})
	}
	if len(converted) == 0 {
		return nil
	}
	return converted
}

func buildStoredAssistantMessage(content string, reasoning string, toolCalls interface{}) *ChatMessage {
	if strings.TrimSpace(content) == "" && strings.TrimSpace(reasoning) == "" && toolCalls == nil {
		return nil
	}
	msg := &ChatMessage{Role: "assistant", Content: content, ReasoningContent: reasoning}
	if toolCalls != nil {
		msg.ToolCalls = toolCalls
		if strings.TrimSpace(content) == "" {
			msg.Content = ""
		}
	}
	return msg
}

func makeStoredResponse(responseObject map[string]interface{}, chatReq ChatRequest, assistantMessage *ChatMessage) StoredResponse {
	chatMessages := cloneChatMessages(chatReq.Messages)
	if assistantMessage != nil {
		chatMessages = append(chatMessages, *assistantMessage)
	}
	return StoredResponse{
		ResponseObject:  responseObject,
		ChatMessages:    chatMessages,
		ProviderModel:   chatReq.Model,
		Thinking:        cloneRawMessage(chatReq.Thinking),
		ParallelTools:   chatReq.ParallelToolCalls,
		StoredAtUnixSec: time.Now().Unix(),
	}
}

func storeResponse(id string, stored StoredResponse) {
	responseStore.Put(id, stored)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErrorResponse(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
			"type":    "invalid_request_error",
		},
	})
}

func redactSensitive(message string, inbound *http.Request) string {
	for _, secret := range []string{mimoKey, resolveMimoKey(inbound)} {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

func compactUnsupportedMessage() string {
	return "/v1/responses/compact is a Codex remote compaction control-plane request; CCMimoLink does not emulate compact and does not forward it upstream"
}

func localProxyURL() string {
	return "http://127.0.0.1:" + proxyPort + "/v1"
}

func ensureCCSwitchInstalled() error {
	for _, path := range []string{ccSwitchSettingsPath, ccSwitchDBPath, codexConfigPath} {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("required file not found: %s", path)
			}
			return err
		}
	}
	return nil
}

func loadCCSwitchSettings(path string) (CCSwitchSettings, error) {
	var settings CCSwitchSettings
	data, err := os.ReadFile(path)
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, err
	}
	if strings.TrimSpace(settings.CurrentProviderCodex) == "" {
		return settings, fmt.Errorf("cc switch currentProviderCodex is empty")
	}
	return settings, nil
}

func loadCCSwitchProvider(dbPath, providerID string) (ccSwitchProviderRecord, error) {
	var record ccSwitchProviderRecord
	cmd := exec.Command("sqlite3", "-json", dbPath, fmt.Sprintf("SELECT id, settings_config, name FROM providers WHERE id = '%s' AND app_type = 'codex';", strings.ReplaceAll(providerID, "'", "''")))
	output, err := cmd.Output()
	if err != nil {
		return record, err
	}
	var rows []struct {
		ID             string `json:"id"`
		SettingsConfig string `json:"settings_config"`
		Name           string `json:"name"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return record, err
	}
	if len(rows) == 0 {
		return record, fmt.Errorf("cc switch provider %s not found", providerID)
	}
	record.ID = rows[0].ID
	record.SettingsConfig = rows[0].SettingsConfig
	record.Name = rows[0].Name
	return record, nil
}

func loadFirstMimoProvider(dbPath string) (ccSwitchProviderRecord, error) {
	var record ccSwitchProviderRecord
	cmd := exec.Command("sqlite3", "-json", dbPath, "SELECT id, settings_config, name FROM providers WHERE app_type = 'codex' AND lower(name) LIKE '%mimo%' ORDER BY id LIMIT 1;")
	output, err := cmd.Output()
	if err != nil {
		return record, err
	}
	var rows []struct {
		ID             string `json:"id"`
		SettingsConfig string `json:"settings_config"`
		Name           string `json:"name"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return record, err
	}
	if len(rows) == 0 {
		return record, fmt.Errorf("Xiaomi MiMo provider not found in cc switch; please add it first")
	}
	record.ID = rows[0].ID
	record.SettingsConfig = rows[0].SettingsConfig
	record.Name = rows[0].Name
	return record, nil
}

func extractCCSwitchAPIKey(settingsConfig string) (string, error) {
	var payload struct {
		Auth   map[string]string `json:"auth"`
		Config string            `json:"config"`
	}
	if err := json.Unmarshal([]byte(settingsConfig), &payload); err != nil {
		return "", err
	}
	apiKey := strings.TrimSpace(payload.Auth["OPENAI_API_KEY"])
	if apiKey == "" {
		return "", fmt.Errorf("cc switch Xiaomi MiMo API key is empty; please add Xiaomi MiMo and input the API key in cc switch first")
	}
	return apiKey, nil
}

func decodeCCSwitchProviderSettings(settingsConfig string) (map[string]interface{}, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(settingsConfig), &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func backupFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	backupPath := fmt.Sprintf("%s.bak.%s", path, time.Now().Format("20060102150405"))
	if err := os.WriteFile(backupPath, data, 0o600); err != nil {
		return "", err
	}
	return backupPath, nil
}

func replaceTOMLString(content, key, value string) string {
	needle := key + " = \""
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, needle) {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + key + " = \"" + value + "\""
			return strings.Join(lines, "\n")
		}
	}
	return content
}

func replaceTOMLStringInSection(content, section, key, value string) string {
	lines := strings.Split(content, "\n")
	inSection := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inSection = trimmed == section
			continue
		}
		if !inSection {
			continue
		}
		needle := key + " = \""
		if strings.HasPrefix(trimmed, needle) {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + key + " = \"" + value + "\""
			return strings.Join(lines, "\n")
		}
	}
	return content
}

func updateCodexConfig(path, apiKey string) (codexConfigUpdate, error) {
	result := codexConfigUpdate{Path: path, APIKey: apiKey, BaseURL: localProxyURL()}
	backupPath, err := backupFile(path)
	if err != nil {
		return result, err
	}
	result.BackupPath = backupPath
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	content := string(data)
	updated := content
	updated = replaceTOMLStringInSection(updated, "[model_providers.mimo]", "base_url", localProxyURL())
	updated = replaceTOMLStringInSection(updated, "[model_providers.mimo.http_headers]", "Authorization", "Bearer local-mimo-proxy")
	updated = replaceTOMLStringInSection(updated, "[model_providers.mimo.http_headers]", "X-Mimo-Api-Key", apiKey)
	// If the [model_providers.mimo] section didn't exist, the replace above is
	// a no-op — and the user is then stuck: switching to MiMo in cc switch
	// would fail because codex has no mimo route. Insert the section if it
	// (or its http_headers sub-section) is missing, so MiMo is always
	// plug-and-play from codex as soon as cc switch adds the provider.
	if !hasTOMLSection(updated, "[model_providers.mimo]") {
		updated = appendMissingMimoSections(updated, apiKey)
	} else if !hasTOMLSection(updated, "[model_providers.mimo.http_headers]") {
		updated = appendMissingMimoSections(updated, apiKey)
	}
	if updated == content {
		return result, nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return result, err
	}
	result.Updated = true
	return result, nil
}

// hasTOMLSection reports whether `content` contains a top-level TOML section
// header exactly matching `section` (e.g. "[model_providers.mimo]"). It does
// not match prefix strings, so "[model_providers.mimo.http_headers]" will not
// be reported as a match for "[model_providers.mimo]".
func hasTOMLSection(content, section string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == section {
			return true
		}
	}
	return false
}

// appendMissingMimoSections appends a complete [model_providers.mimo] block
// (with base_url, wire_api, name, and http_headers) to the end of `content`
// if it is missing. Used to guarantee that codex always has a usable MiMo
// route after ccmimolink starts, regardless of what was in config.toml.
func appendMissingMimoSections(content, apiKey string) string {
	if hasTOMLSection(content, "[model_providers.mimo]") {
		// [model_providers.mimo] exists but http_headers sub-section is
		// missing — just append the sub-section.
		header := "\n[model_providers.mimo.http_headers]\n"
		header += "Authorization = \"Bearer local-mimo-proxy\"\n"
		header += fmt.Sprintf("X-Mimo-Api-Key = \"%s\"\n", apiKey)
		return content + header
	}
	// Whole mimo block missing — append it under a new [model_providers]
	// parent (idempotent: TOML allows the same table to be declared twice).
	block := "\n[model_providers.mimo]\n"
	block += "name = \"Xiaomi MiMo\"\n"
	block += "base_url = \"" + localProxyURL() + "\"\n"
	block += "wire_api = \"responses\"\n"
	block += "\n[model_providers.mimo.http_headers]\n"
	block += "Authorization = \"Bearer local-mimo-proxy\"\n"
	block += fmt.Sprintf("X-Mimo-Api-Key = \"%s\"\n", apiKey)
	return content + block
}

// updateCodexAuth writes the MiMo API key into the OPENAI_API_KEY field of
// `~/.codex/auth.json`, mirroring CC Switch's default behavior for third-party
// providers (see `write_codex_live_for_provider` in farion1231/cc-switch).
//
// It preserves every other field in the file — ChatGPT login tokens, MCP
// credentials, etc. — so users who log into ChatGPT for the official OpenAI
// flow don't lose their session. The function is idempotent: if the file is
// missing, an empty one is created; if the OPENAI_API_KEY already matches
// `apiKey`, the file is left untouched and `Updated` stays false.
func updateCodexAuth(path, apiKey string) (codexAuthUpdate, error) {
	result := codexAuthUpdate{Path: path, APIKey: apiKey}

	// Read existing file if present; otherwise start from an empty object.
	// We intentionally allow a missing file here — auth.json is created by
	// codex's first login, and not all MiMo-only installs have ever logged
	// in to ChatGPT.
	var payload map[string]interface{}
	if data, err := os.ReadFile(path); err == nil {
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &payload); err != nil {
				return result, fmt.Errorf("codex auth.json is not valid JSON: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if payload == nil {
		payload = map[string]interface{}{}
	}

	// Only write if the key actually differs. Avoids needless mtime churn
	// (which would also invalidate any "live" view the cc-switch UI has).
	if existing, _ := payload["OPENAI_API_KEY"].(string); existing == apiKey {
		return result, nil
	}
	payload["OPENAI_API_KEY"] = apiKey

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return result, err
	}
	// Add trailing newline so editors / `cat` don't complain.
	encoded = append(encoded, '\n')

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return result, err
	}
	result.Updated = true
	return result, nil
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func rewriteCCSwitchProxyRoute(dbPath, providerID string) error {
	provider, err := loadCCSwitchProvider(dbPath, providerID)
	if err != nil {
		return err
	}
	payload, err := decodeCCSwitchProviderSettings(provider.SettingsConfig)
	if err != nil {
		return err
	}
	configText, _ := payload["config"].(string)
	configText = strings.ReplaceAll(configText, "base_url = \"https://api.xiaomimimo.com/v1\"", "base_url = \""+localProxyURL()+"\"")
	configText = strings.ReplaceAll(configText, "base_url = \"http://127.0.0.1:9876/v1\"", "base_url = \""+localProxyURL()+"\"")
	payload["config"] = configText
	updatedJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	providerIDSQL := sqlQuote(providerID)
	settingsConfigSQL := sqlQuote(string(updatedJSON))
	localProxyURLSQL := sqlQuote(localProxyURL())
	cmd := exec.Command("sqlite3", dbPath,
		fmt.Sprintf("UPDATE providers SET settings_config = %s WHERE id = %s AND app_type = 'codex'; UPDATE provider_endpoints SET url = %s WHERE provider_id = %s AND app_type = 'codex';", settingsConfigSQL, providerIDSQL, localProxyURLSQL, providerIDSQL),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sqlite3 update failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func syncCCSwitchAndCodex() error {
	if skipCCSwitchSync {
		log.Printf("[CCMimoLink] skipping cc switch sync because MIMO_PROXY_SKIP_CC_SWITCH_SYNC=true")
		return nil
	}
	if err := ensureCCSwitchInstalled(); err != nil {
		return fmt.Errorf("cc switch is not installed or incomplete: %w", err)
	}
	// Locked to the Xiaomi MiMo provider only. We deliberately ignore
	// `currentProviderCodex` here: ccmimolink is the proxy for the MiMo
	// channel, not for whatever provider the user happens to be using in
	// Codex right now. Reading api keys / config from a non-MiMo provider
	// would silently corrupt the MiMo codex routing and credentials.
	provider, err := loadFirstMimoProvider(ccSwitchDBPath)
	if err != nil {
		return fmt.Errorf("Xiaomi MiMo provider not found in cc switch: %w", err)
	}
	apiKey, err := extractCCSwitchAPIKey(provider.SettingsConfig)
	if err != nil {
		return err
	}
	// Hard guard: the Xiaomi MiMo api key in cc switch MUST match the
	// `MIMO_API_KEY` that ccmimolink was launched with. If they differ, a
	// previous buggy revision of this code may have written a non-MiMo
	// provider's key into the MiMo record — refuse to sync, otherwise
	// we'd propagate the wrong key into codex config.toml.
	if mimoKey != "" && apiKey != mimoKey {
		return fmt.Errorf("cc switch Xiaomi MiMo OPENAI_API_KEY (%s…%s) does not match MIMO_API_KEY from launchd env (%s…%s); please open cc switch and restore the correct MiMo API key on the Xiaomi MiMo provider, then restart ccmimolink",
			maskKey(apiKey), maskKey(mimoKey), maskKey(mimoKey), maskKey(apiKey))
	}
	if err := rewriteCCSwitchProxyRoute(ccSwitchDBPath, provider.ID); err != nil {
		return err
	}
	update, err := updateCodexConfig(codexConfigPath, apiKey)
	if err != nil {
		return err
	}
	// Mirror CC Switch's default behavior for third-party providers: it
	// overwrites ~/.codex/auth.json.OPENAI_API_KEY when switching to a
	// non-official provider (see `write_codex_live_for_provider` in
	// farion1231/cc-switch). We follow the same convention so the auth.json
	// shown in the cc-switch edit dialog matches the MiMo key actually in
	// use, and so a future direct switch to official OpenAI via codex still
	// has a valid mimo-era key in the file. We only touch OPENAI_API_KEY,
	// leaving any ChatGPT login tokens in place.
	authUpdate, err := updateCodexAuth(codexAuthPath, apiKey)
	if err != nil {
		return err
	}
	log.Printf("[CCMimoLink] locked cc switch Xiaomi MiMo provider %s to local route %s (independent of currentProviderCodex)", provider.ID, localProxyURL())
	log.Printf("[CCMimoLink] backed up Codex config to %s", update.BackupPath)
	if update.Updated {
		log.Printf("[CCMimoLink] updated Codex Xiaomi MiMo headers from Xiaomi MiMo API key")
	} else {
		log.Printf("[CCMimoLink] Codex config already matched the required Xiaomi MiMo route and API key")
	}
	if authUpdate.Updated {
		log.Printf("[CCMimoLink] updated Codex auth.json OPENAI_API_KEY to Xiaomi MiMo API key (other fields preserved)")
	} else {
		log.Printf("[CCMimoLink] Codex auth.json OPENAI_API_KEY already matched the Xiaomi MiMo API key")
	}
	log.Printf("[CCMimoLink] restart cc switch and restart Codex to apply the updated Xiaomi MiMo routing and API key")
	return nil
}

// maskKey returns a short fingerprint of an api key for error messages,
// so we can compare keys in logs without exposing the full secret.
// It shows the first 6 and last 4 characters separated by "…".
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 12 {
		return "***"
	}
	return key[:6] + "…" + key[len(key)-4:]
}

func runStartupSyncOnly() error {
	return syncCCSwitchAndCodex()
}

func newUpstreamLimiter() *UpstreamLimiter {
	concurrency := envIntOrDefault("MIMO_PROXY_MAX_CONCURRENT", defaultMaxConcurrent)
	minIntervalMS := envIntOrDefault("MIMO_PROXY_MIN_INTERVAL_MS", defaultMinIntervalMS)
	if envBoolOrDefault("MIMO_PROXY_LEGACY_MODE", false) {
		concurrency = 1
		minIntervalMS = 1500
	}
	return &UpstreamLimiter{
		sem:      make(chan struct{}, concurrency),
		interval: time.Duration(minIntervalMS) * time.Millisecond,
	}
}

func (l *UpstreamLimiter) Wait(ctx context.Context) error {
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	l.mu.Lock()
	now := time.Now()
	wait := time.Duration(0)
	if now.Before(l.next) {
		wait = l.next.Sub(now)
		now = l.next
	}
	l.next = now.Add(l.interval)
	l.mu.Unlock()

	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			l.Done()
			return ctx.Err()
		}
	}
	return nil
}

func (l *UpstreamLimiter) Done() {
	select {
	case <-l.sem:
	default:
	}
}

func (l *UpstreamLimiter) Backoff(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	until := time.Now().Add(d)
	if until.After(l.next) {
		l.next = until
	}
	l.mu.Unlock()
}

func retryDelay(resp *http.Response, body []byte) time.Duration {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if seconds, err := strconv.Atoi(v); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return time.Duration(envIntOrDefault("MIMO_PROXY_429_BACKOFF_MS", 30000)) * time.Millisecond
}

func sendUpstream(req *http.Request) (*http.Response, error) {
	if err := limiter.Wait(req.Context()); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		limiter.Done()
		return nil, err
	}
	return resp, nil
}

func closeUpstream(resp *http.Response, body []byte) {
	if d := retryDelay(resp, body); d > 0 {
		log.Printf("[Proxy] upstream rate limited; backing off for %s", d)
		limiter.Backoff(d)
	}
	limiter.Done()
}

func setupLogging() {
	logPath := envOrDefault("MIMO_PROXY_LOG", "ccmimolink.log")
	if dir := filepath.Dir(logPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[CCMimoLink] file logging disabled: mkdir %s: %v", dir, err)
			return
		}
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[CCMimoLink] file logging disabled: open %s: %v", logPath, err)
		return
	}

	log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	log.Printf("[CCMimoLink] file logging enabled: %s", logPath)
}

func resolveMimoKey(inbound *http.Request) string {
	if inbound != nil {
		if key := strings.TrimSpace(inbound.Header.Get("X-Mimo-Api-Key")); key != "" {
			return key
		}
		if key := strings.TrimSpace(inbound.Header.Get("api-key")); key != "" {
			return key
		}
		if auth := strings.TrimSpace(inbound.Header.Get("Authorization")); auth != "" {
			if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
				token := strings.TrimSpace(auth[7:])
				if token != "" && token != "local-mimo-proxy" {
					return token
				}
			}
		}
	}
	return mimoKey
}

func setMimoHeaders(req *http.Request, inbound *http.Request) error {
	req.Header.Set("Content-Type", "application/json")
	key := resolveMimoKey(inbound)
	if key == "" {
		return fmt.Errorf("missing CCMimoLink upstream API key")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return nil
}

// ---- Responses API 请求 ----

type ResponsesRequest struct {
	Model              string          `json:"model,omitempty"`
	Instructions       string          `json:"instructions,omitempty"`
	Input              interface{}     `json:"input"`
	Stream             bool            `json:"stream"`
	Store              *bool           `json:"store,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
}

// ---- Chat Completions 请求 ----

type ChatMessage struct {
	Role             string      `json:"role"`
	Content          interface{} `json:"content,omitempty"`
	ToolCalls        interface{} `json:"tool_calls,omitempty"`
	ToolCallID       string      `json:"tool_call_id,omitempty"`
	ReasoningContent string      `json:"reasoning_content,omitempty"`
}

type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []ChatMessage   `json:"messages"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	Thinking            json.RawMessage `json:"thinking,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
}

type ResponseEnvelope struct {
	ResponseID      string
	ResponseObject  map[string]interface{}
	StoredResponse  StoredResponse
	ProviderMessage *ChatMessage
}

// parsedChatResp 是 executeUpstreamChat 的解析结果，覆盖 tool_calls / content /
// reasoning_content / finish_reason / usage。非流式路径靠它判断是否要触发
// describe_image 子任务循环。
type parsedChatResp struct {
	ID               string
	Content          string
	ReasoningContent string
	FinishReason     string
	ToolCalls        []parsedToolCall
	Usage            *MimoUsage
	ErrorStatus      int
	ErrorCode        string
	ErrorMessage     string
}

type parsedToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type describeImageCall struct {
	CallID    string
	ImageURL  string
	Prompt    string
	ErrorText string
}

func chatError(status int, code, message string) parsedChatResp {
	return parsedChatResp{ErrorStatus: status, ErrorCode: code, ErrorMessage: message}
}

func (p parsedChatResp) hasError() bool {
	return p.ErrorCode != ""
}

// ---- 流式 chunk ----

type ChatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          *string     `json:"content"`
			ReasoningContent *string     `json:"reasoning_content"`
			ToolCalls        interface{} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *MimoUsage `json:"usage,omitempty"`
}

type MimoUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type CodexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func convertUsage(u *MimoUsage) *CodexUsage {
	if u == nil {
		return nil
	}
	return &CodexUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
}

func addUsage(dst **MimoUsage, src *MimoUsage) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = &MimoUsage{}
	}
	(*dst).PromptTokens += src.PromptTokens
	(*dst).CompletionTokens += src.CompletionTokens
	(*dst).TotalTokens += src.TotalTokens
}

// ---- 解析 input ----

func convertImagePart(part map[string]interface{}) (map[string]interface{}, bool) {
	var url string
	switch raw := part["image_url"].(type) {
	case string:
		url = strings.TrimSpace(raw)
	case map[string]interface{}:
		if nested, ok := raw["url"].(string); ok {
			url = strings.TrimSpace(nested)
		}
	}
	if url == "" {
		if raw, ok := part["url"].(string); ok {
			url = strings.TrimSpace(raw)
		}
	}
	if url == "" {
		return nil, false
	}
	return map[string]interface{}{
		"type":      "image_url",
		"image_url": map[string]string{"url": url},
	}, true
}

func parseInput(input interface{}) ([]ChatMessage, bool) {
	if input == nil {
		return nil, false
	}
	switch v := input.(type) {
	case string:
		return []ChatMessage{{Role: "user", Content: v}}, false
	case []interface{}:
		var msgs []ChatMessage
		hasImages := false
		for _, item := range v {
			msg, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			itemType, _ := msg["type"].(string)

			switch itemType {
			case "message":
				role, _ := msg["role"].(string)
				if role == "" {
					role = "user"
				}
				var content interface{}
				switch c := msg["content"].(type) {
				case string:
					content = c
				case []interface{}:
					var textParts []string
					var richParts []interface{}
					messageHasImages := false
					for _, p := range c {
						part, ok := p.(map[string]interface{})
						if !ok {
							continue
						}
						t, _ := part["type"].(string)
						switch t {
						case "input_text", "text":
							if txt, ok := part["text"].(string); ok && txt != "" {
								textParts = append(textParts, txt)
								richParts = append(richParts, map[string]string{"type": "text", "text": txt})
							}
						case "input_image", "image_url":
							if imagePart, ok := convertImagePart(part); ok {
								richParts = append(richParts, imagePart)
								messageHasImages = true
							}
						}
					}
					if messageHasImages {
						content = richParts
						hasImages = true
					} else {
						content = strings.Join(textParts, "\n")
					}
				}
				if contentStr, ok := content.(string); ok && contentStr == "" {
					continue
				}
				if contentArr, ok := content.([]interface{}); ok && len(contentArr) == 0 {
					continue
				}
				msgs = append(msgs, ChatMessage{Role: role, Content: content})

			case "reasoning":
				var reasoningTextParts []string
				if summary, ok := msg["summary"].([]interface{}); ok {
					for _, rawSummary := range summary {
						summaryPart, ok := rawSummary.(map[string]interface{})
						if !ok {
							continue
						}
						if text := stringValue(summaryPart["text"]); text != "" {
							reasoningTextParts = append(reasoningTextParts, text)
						}
					}
				}
				reasoningText := strings.Join(reasoningTextParts, "\n")
				if reasoningText != "" {
					msgs = append(msgs, ChatMessage{Role: "assistant", Content: "", ReasoningContent: reasoningText})
				}

			case "function_call_output":
				callID, _ := msg["call_id"].(string)
				var output string
				switch o := msg["output"].(type) {
				case string:
					output = o
				default:
					b, _ := json.Marshal(o)
					output = string(b)
				}
				msgs = append(msgs, ChatMessage{
					Role:       "tool",
					Content:    output,
					ToolCallID: callID,
				})

			case "function_call":
				// 这是之前轮次的 assistant tool call，需要保留
				callID, _ := msg["call_id"].(string)
				name, _ := msg["name"].(string)
				args, _ := msg["arguments"].(string)
				msgs = append(msgs, ChatMessage{
					Role:    "assistant",
					Content: "",
					ToolCalls: []map[string]interface{}{
						{
							"id":   callID,
							"type": "function",
							"function": map[string]string{
								"name":      name,
								"arguments": args,
							},
						},
					},
				})
			}
		}
		if len(msgs) > 0 {
			return msgs, hasImages
		}
	}
	return []ChatMessage{{Role: "user", Content: fmt.Sprintf("%v", input)}}, false
}

func stripImagesForDescribe(messages []ChatMessage) (map[string]string, bool) {
	imageMap := map[string]string{}
	phIdx := 0
	for mi := range messages {
		rawParts, ok := messages[mi].Content.([]interface{})
		if !ok {
			continue
		}
		var newParts []interface{}
		textBuf := ""
		flushText := func() {
			if textBuf != "" {
				newParts = append(newParts, map[string]interface{}{"type": "text", "text": textBuf})
				textBuf = ""
			}
		}
		for _, rp := range rawParts {
			p, ok := rp.(map[string]interface{})
			if !ok {
				flushText()
				newParts = append(newParts, rp)
				continue
			}
			ptype, _ := p["type"].(string)
			switch ptype {
			case "image_url":
				realURL := ""
				if iu, ok := p["image_url"].(map[string]interface{}); ok {
					realURL = stringValue(iu["url"])
				} else if iu, ok := p["image_url"].(map[string]string); ok {
					realURL = strings.TrimSpace(iu["url"])
				}
				if realURL == "" {
					flushText()
					newParts = append(newParts, p)
					continue
				}
				placeholder := fmt.Sprintf("placeholder://image_%d", phIdx)
				imageMap[placeholder] = realURL
				phIdx++
				flushText()
				newParts = append(newParts, map[string]interface{}{
					"type": "text",
					"text": fmt.Sprintf("[image attached, call describe_image with image_url=\"%s\" to read it]", placeholder),
				})
			case "text", "input_text":
				if t, ok := p["text"].(string); ok {
					textBuf += t
				} else {
					flushText()
					newParts = append(newParts, p)
				}
			default:
				flushText()
				newParts = append(newParts, p)
			}
		}
		flushText()
		if len(newParts) > 0 {
			messages[mi].Content = newParts
		}
	}
	if len(imageMap) == 0 {
		return nil, false
	}
	return imageMap, true
}

// ---- 透传稳定字段 ----

func rawJSONPresent(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null"
}

func rawJSONArrayLen(raw json.RawMessage) int {
	if !rawJSONPresent(raw) {
		return 0
	}
	var arr []interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return len(arr)
}

func rawToolNames(raw json.RawMessage) []string {
	if !rawJSONPresent(raw) {
		return nil
	}
	var tools []map[string]interface{}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	var names []string
	for _, tool := range tools {
		if fn, ok := tool["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				names = append(names, name)
			}
			continue
		}
		if name, ok := tool["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

type normalizedTools struct {
	Converted        []map[string]interface{}
	SupportedNames   map[string]struct{}
	InputCount       int
	ForwardedCount   int
	DroppedCount     int
	DroppedToolTypes []string
	// HasDescribeImage 标记本次请求的 Converted 列表中是否含 describe_image 工具。
	// 用于 handleResponses 决定 hasImages=true 时是否仍走 v2.5-pro。
	HasDescribeImage bool
}

func stringValue(v interface{}) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func emptyToolParameters() map[string]interface{} {
	return map[string]interface{}{
		"properties": map[string]interface{}{},
		"type":       "object",
	}
}

// isNamespaceTool 判断一个工具条目是不是 namespace 形态（多子任务工具）。
// Codex 发的多子任务工具长这样：{type:"multi_agent_v1", name:"spawn_agent", ...}
// 或带 namespace 字段：{type:"function", namespace:"multi_agent_v1", name:"spawn_agent", ...}。
// 仅在 LEGACY_MODE=false 时被 convertToolsStable 调用。
func isNamespaceTool(tool map[string]interface{}) bool {
	if ns := stringValue(tool["namespace"]); ns != "" {
		return true
	}
	if t := stringValue(tool["type"]); strings.HasPrefix(t, "multi_agent") {
		return true
	}
	return false
}

// flattenNamespaceTool 把 namespace 形态的工具展平为 OpenAI Chat Completions
// function 工具：{type:"function", function:{name, description, parameters}}。
// 返回 nil 表示无法展平（缺 name 等）。
func flattenNamespaceTool(tool map[string]interface{}) map[string]interface{} {
	name := stringValue(tool["name"])
	if name == "" {
		return nil
	}
	description := stringValue(tool["description"])
	parameters := tool["parameters"]
	if parameters == nil {
		parameters = tool["input_schema"]
	}
	if parameters == nil {
		parameters = emptyToolParameters()
	}
	convertedFn := map[string]interface{}{
		"description": description,
		"name":        name,
		"parameters":  parameters,
	}
	if strict, ok := tool["strict"]; ok {
		convertedFn["strict"] = strict
	}
	return map[string]interface{}{
		"function": convertedFn,
		"type":     "function",
	}
}

func (n normalizedTools) rawMessage() json.RawMessage {
	if len(n.Converted) == 0 {
		return nil
	}
	b, _ := json.Marshal(n.Converted)
	return b
}

func (n normalizedTools) filterByNames(allowed map[string]struct{}) normalizedTools {
	if len(allowed) == 0 {
		n.Converted = nil
		n.SupportedNames = map[string]struct{}{}
		n.ForwardedCount = 0
		return n
	}
	filtered := make([]map[string]interface{}, 0, len(n.Converted))
	names := make(map[string]struct{}, len(allowed))
	for _, tool := range n.Converted {
		fn, _ := tool["function"].(map[string]interface{})
		name := stringValue(fn["name"])
		if _, ok := allowed[name]; !ok {
			continue
		}
		filtered = append(filtered, tool)
		names[name] = struct{}{}
	}
	n.Converted = filtered
	n.SupportedNames = names
	n.ForwardedCount = len(filtered)
	n.HasDescribeImage = false
	if _, ok := names[describeImageToolName]; ok {
		n.HasDescribeImage = true
	}
	return n
}

func convertToolsStable(raw json.RawMessage) normalizedTools {
	result := normalizedTools{SupportedNames: map[string]struct{}{}}
	markSupported := func(name string) {
		result.SupportedNames[name] = struct{}{}
		if name == describeImageToolName {
			result.HasDescribeImage = true
		}
	}
	if !rawJSONPresent(raw) {
		return result
	}
	var tools []map[string]interface{}
	if err := json.Unmarshal(raw, &tools); err != nil {
		result.DroppedCount = 1
		result.DroppedToolTypes = append(result.DroppedToolTypes, "invalid_tools_payload")
		return result
	}
	result.InputCount = len(tools)
	result.Converted = make([]map[string]interface{}, 0, len(tools))
	legacyMode := envBoolOrDefault("MIMO_PROXY_LEGACY_MODE", false)
	for _, tool := range tools {
		if fn, ok := tool["function"].(map[string]interface{}); ok {
			name := stringValue(fn["name"])
			if name == "" {
				result.DroppedCount++
				result.DroppedToolTypes = appendUnique(result.DroppedToolTypes, "function")
				continue
			}
			parameters := fn["parameters"]
			if parameters == nil {
				parameters = emptyToolParameters()
			}
			convertedFn := map[string]interface{}{
				"description": fn["description"],
				"name":        name,
				"parameters":  parameters,
			}
			if strict, ok := fn["strict"]; ok {
				convertedFn["strict"] = strict
			}
			result.Converted = append(result.Converted, map[string]interface{}{
				"function": convertedFn,
				"type":     "function",
			})
			markSupported(name)
			continue
		}

		toolType := stringValue(tool["type"])
		// namespace 工具展平：Codex 默认发的 multi_agent_v1 工具是 namespace 形态
		// （{type:"multi_agent_v1", name:"spawn_agent", ...}），OpenAI Chat Completions
		// 协议没有 namespace 概念。这里展平为顶层 function 工具，让 mimo-v2.5-pro
		// 能直接看到 spawn_agent / send_input / wait_agent / close_agent 等。
		// LEGACY_MODE 下保留原 drop 行为。
		if !legacyMode && toolType != "" && toolType != "function" && isNamespaceTool(tool) {
			if flat := flattenNamespaceTool(tool); flat != nil {
				result.Converted = append(result.Converted, flat)
				if fn, ok := flat["function"].(map[string]interface{}); ok {
					if n := stringValue(fn["name"]); n != "" {
						markSupported(n)
					}
				}
				continue
			}
			result.DroppedCount++
			result.DroppedToolTypes = appendUnique(result.DroppedToolTypes, toolType+"_invalid")
			continue
		}

		if toolType != "" && toolType != "function" {
			result.DroppedCount++
			result.DroppedToolTypes = appendUnique(result.DroppedToolTypes, toolType)
			continue
		}

		name := stringValue(tool["name"])
		if name == "" {
			result.DroppedCount++
			droppedType := toolType
			if droppedType == "" {
				droppedType = "unnamed_tool"
			}
			result.DroppedToolTypes = appendUnique(result.DroppedToolTypes, droppedType)
			continue
		}
		parameters := tool["parameters"]
		if parameters == nil {
			parameters = tool["input_schema"]
		}
		if parameters == nil {
			parameters = emptyToolParameters()
		}
		convertedFn := map[string]interface{}{
			"description": tool["description"],
			"name":        name,
			"parameters":  parameters,
		}
		if strict, ok := tool["strict"]; ok {
			convertedFn["strict"] = strict
		}
		result.Converted = append(result.Converted, map[string]interface{}{
			"function": convertedFn,
			"type":     "function",
		})
		markSupported(name)
	}
	result.ForwardedCount = len(result.Converted)
	return result
}

func normalizeToolChoice(raw json.RawMessage, tools normalizedTools) (json.RawMessage, normalizedTools, string) {
	if len(tools.Converted) == 0 {
		return nil, tools, "omitted:no_supported_tools"
	}
	if !rawJSONPresent(raw) {
		return nil, tools, "absent"
	}

	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, tools, "dropped:invalid_json"
	}

	marshalChoice := func(v interface{}) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}

	switch v := decoded.(type) {
	case string:
		choice := strings.TrimSpace(v)
		switch choice {
		case "auto", "required", "none":
			return marshalChoice(choice), tools, "kept:" + choice
		default:
			return nil, tools, "dropped:unknown_string"
		}
	case map[string]interface{}:
		switch stringValue(v["type"]) {
		case "function":
			name := stringValue(v["name"])
			if name == "" {
				if fn, ok := v["function"].(map[string]interface{}); ok {
					name = stringValue(fn["name"])
				}
			}
			if name == "" {
				return nil, tools, "dropped:function_without_name"
			}
			if _, ok := tools.SupportedNames[name]; !ok {
				return nil, tools, "dropped:function_not_forwarded"
			}
			return marshalChoice(map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name": name,
				},
			}), tools, "kept:function"
		case "allowed_tools":
			mode := stringValue(v["mode"])
			if mode == "" {
				mode = "auto"
			}
			if mode != "auto" && mode != "required" && mode != "none" {
				mode = "auto"
			}
			allowed := map[string]struct{}{}
			if rawTools, ok := v["tools"].([]interface{}); ok {
				for _, rawTool := range rawTools {
					toolMap, ok := rawTool.(map[string]interface{})
					if !ok {
						continue
					}
					toolType := stringValue(toolMap["type"])
					if toolType != "" && toolType != "function" {
						continue
					}
					name := stringValue(toolMap["name"])
					if name == "" {
						if fn, ok := toolMap["function"].(map[string]interface{}); ok {
							name = stringValue(fn["name"])
						}
					}
					if name == "" {
						continue
					}
					if _, ok := tools.SupportedNames[name]; ok {
						allowed[name] = struct{}{}
					}
				}
			}
			filtered := tools.filterByNames(allowed)
			if len(filtered.Converted) == 0 {
				return nil, filtered, "dropped:allowed_tools_empty"
			}
			return marshalChoice(mode), filtered, "kept:allowed_tools_" + mode
		default:
			return nil, tools, "dropped:unsupported_object"
		}
	default:
		return nil, tools, "dropped:invalid_shape"
	}
}

func mapReasoningToThinking(raw json.RawMessage) json.RawMessage {
	if !rawJSONPresent(raw) {
		return nil
	}
	return raw
}

// ---- 构建标准 Response 对象 ----

func makeResponse(id, status, model string, output []map[string]interface{}, usage *CodexUsage) map[string]interface{} {
	if model == "" {
		model = mimoModel
	}
	r := map[string]interface{}{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output":     output,
	}
	if usage != nil {
		r["usage"] = usage
	}
	return r
}

func responseObjectID(response map[string]interface{}) string {
	if response == nil {
		return ""
	}
	return stringValue(response["id"])
}

func finalizeResponseEnvelope(response map[string]interface{}, chatReq ChatRequest, assistantMessage *ChatMessage) ResponseEnvelope {
	responseID := responseObjectID(response)
	stored := makeStoredResponse(response, chatReq, assistantMessage)
	if responseID != "" {
		storeResponse(responseID, stored)
	}
	return ResponseEnvelope{
		ResponseID:      responseID,
		ResponseObject:  response,
		StoredResponse:  stored,
		ProviderMessage: assistantMessage,
	}
}

func parseTextToolCalls(content string) ([]map[string]interface{}, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.Contains(trimmed, "<tool_call>") || !strings.Contains(trimmed, "</tool_call>") {
		return nil, false
	}
	var calls []map[string]interface{}
	remaining := trimmed
	for {
		start := strings.Index(remaining, "<tool_call>")
		end := strings.Index(remaining, "</tool_call>")
		if start < 0 || end < 0 || end <= start {
			break
		}
		block := remaining[start+len("<tool_call>") : end]
		remaining = remaining[end+len("</tool_call>"):]

		fnStart := strings.Index(block, "<function=")
		if fnStart < 0 {
			continue
		}
		nameStart := fnStart + len("<function=")
		nameEnd := strings.Index(block[nameStart:], ">")
		if nameEnd < 0 {
			continue
		}
		name := strings.TrimSpace(block[nameStart : nameStart+nameEnd])
		if name == "" {
			continue
		}
		args := map[string]interface{}{}
		search := block[nameStart+nameEnd+1:]
		for {
			pStart := strings.Index(search, "<parameter=")
			if pStart < 0 {
				break
			}
			keyStart := pStart + len("<parameter=")
			keyEnd := strings.Index(search[keyStart:], ">")
			if keyEnd < 0 {
				break
			}
			key := strings.TrimSpace(search[keyStart : keyStart+keyEnd])
			valueStart := keyStart + keyEnd + 1
			valueEnd := strings.Index(search[valueStart:], "</parameter>")
			if valueEnd < 0 {
				break
			}
			args[key] = strings.TrimSpace(search[valueStart : valueStart+valueEnd])
			search = search[valueStart+valueEnd+len("</parameter>"):]
		}
		argBytes, _ := json.Marshal(args)
		callID := fmt.Sprintf("call_%d", time.Now().UnixNano()+int64(len(calls)))
		calls = append(calls, map[string]interface{}{
			"type":      "function_call",
			"id":        callID,
			"call_id":   callID,
			"name":      name,
			"arguments": string(argBytes),
			"status":    "completed",
		})
	}
	return calls, len(calls) > 0
}

// ---- 非流式 ----

// executeUpstreamChat 发送 chat completions 请求到 mimo，解析 tool_calls /
// content / reasoning_content / finish_reason / usage，返回 parsedChatResp。
func executeUpstreamChat(inbound *http.Request, chatReq ChatRequest) parsedChatResp {
	body, _ := json.Marshal(chatReq)
	req, _ := http.NewRequest("POST", mimoBase+"/chat/completions", bytes.NewReader(body))
	if err := setMimoHeaders(req, inbound); err != nil {
		return chatError(http.StatusInternalServerError, "missing_mimo_api_key", err.Error())
	}
	resp, err := sendUpstream(req)
	if err != nil {
		return chatError(http.StatusBadGateway, "upstream_request_failed", err.Error())
	}
	var respBody []byte
	defer func() { closeUpstream(resp, respBody) }()
	defer resp.Body.Close()

	respBody, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		message := redactSensitive(string(respBody), inbound)
		log.Printf("[Proxy] MiMo error %d: %s", resp.StatusCode, message)
		return chatError(resp.StatusCode, "upstream_error", message)
	}

	var chatResp struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content          string      `json:"content"`
				ReasoningContent string      `json:"reasoning_content"`
				ToolCalls        interface{} `json:"tool_calls"`
				FinishReason     string      `json:"finish_reason"`
			} `json:"message"`
		} `json:"choices"`
		Usage *MimoUsage `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return chatError(http.StatusBadGateway, "upstream_parse_failed", err.Error())
	}

	out := parsedChatResp{ID: chatResp.ID, Usage: chatResp.Usage}
	if len(chatResp.Choices) == 0 {
		return chatError(http.StatusBadGateway, "upstream_empty_response", "upstream returned no choices")
	}
	msg := chatResp.Choices[0].Message
	out.Content = msg.Content
	out.ReasoningContent = msg.ReasoningContent
	out.FinishReason = msg.FinishReason
	if msg.ToolCalls != nil {
		if tc, ok := msg.ToolCalls.([]interface{}); ok {
			for _, call := range tc {
				c, ok := call.(map[string]interface{})
				if !ok {
					continue
				}
				fn, _ := c["function"].(map[string]interface{})
				out.ToolCalls = append(out.ToolCalls, parsedToolCall{
					ID:        stringValue(c["id"]),
					Name:      stringValue(fn["name"]),
					Arguments: stringValue(fn["arguments"]),
				})
			}
		}
	}
	return out
}

// callDescribeImage 在 proxy 内部用 mimo-v2.5 跑一次 chat completions 读图，
// 返回文字描述。出错时把错误信息作为描述返回（不挂掉请求）。
// imageMap 用于把 placeholder://image_N 解析为真实图 URL（被剥图的 mimo-v2.5-pro
// 调 describe_image 时只能传 placeholder，真图在 proxy 侧还原）。
func callDescribeImage(inbound *http.Request, call describeImageCall, imageMap map[string]string) parsedChatResp {
	prompt := call.Prompt
	if prompt == "" {
		prompt = "Describe the image in detail."
	}
	realURL := call.ImageURL
	if imageMap != nil {
		if u, ok := imageMap[call.ImageURL]; ok {
			realURL = u
		} else if strings.HasPrefix(call.ImageURL, "placeholder://") {
			return chatError(http.StatusBadRequest, "describe_image_unknown_placeholder", "unknown image placeholder: "+call.ImageURL)
		}
	}
	if strings.TrimSpace(realURL) == "" {
		return chatError(http.StatusBadRequest, "describe_image_missing_image_url", "describe_image requires image_url")
	}
	visionReq := ChatRequest{
		Model: visionMimoModel,
		Messages: []ChatMessage{{
			Role: "user",
			Content: []map[string]interface{}{
				{"type": "image_url", "image_url": map[string]string{"url": realURL}},
				{"type": "text", "text": prompt},
			},
		}},
		Stream: false,
	}
	resp := executeUpstreamChat(inbound, visionReq)
	if resp.hasError() {
		return resp
	}
	desc := strings.TrimSpace(resp.Content)
	if desc == "" {
		return chatError(http.StatusBadGateway, "describe_image_empty_content", "vision model returned empty content")
	}
	resp.Content = desc
	return resp
}

// extractDescribeImageCalls 从 parsedChatResp 挑出 name==describe_image 的 tool_call。
func extractDescribeImageCalls(chatResp parsedChatResp) []describeImageCall {
	var out []describeImageCall
	for _, tc := range chatResp.ToolCalls {
		if tc.Name != describeImageToolName {
			continue
		}
		var args struct {
			ImageURL string `json:"image_url"`
			Prompt   string `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			out = append(out, describeImageCall{CallID: tc.ID, ErrorText: "invalid describe_image arguments: " + err.Error()})
			continue
		}
		out = append(out, describeImageCall{
			CallID:   tc.ID,
			ImageURL: strings.TrimSpace(args.ImageURL),
			Prompt:   args.Prompt,
		})
	}
	return out
}

// assistantToolCallsFromParsed 把 parsedToolCall 序列化为 assistant message 的
// tool_calls 字段（OpenAI Chat Completions 形态），用于回灌给下一轮 pro 请求。
func assistantToolCallsFromParsed(calls []parsedToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]interface{}{
			"id":   c.ID,
			"type": "function",
			"function": map[string]string{
				"name":      c.Name,
				"arguments": c.Arguments,
			},
		})
	}
	return out
}

// handleNonStream 运行上游 chat completions 一次，必要时拦截 describe_image
// tool_call 跑多轮（最多 maxDescribeRounds），把 mimo-v2.5 读图回执作为
// role=tool 注入下一轮 messages。imageMap 用于把 placeholder://image_N 还原
// 为真实图 URL。
func handleNonStream(w http.ResponseWriter, inbound *http.Request, chatReq ChatRequest, imageMap map[string]string) {
	envelope, errResp := runNonStream(inbound, chatReq, imageMap, "")
	if errResp != nil {
		writeErrorResponse(w, errResp.ErrorStatus, errResp.ErrorCode, errResp.ErrorMessage)
		return
	}
	writeJSON(w, http.StatusOK, envelope.ResponseObject)
}

func runNonStream(inbound *http.Request, chatReq ChatRequest, imageMap map[string]string, responseID string) (ResponseEnvelope, *parsedChatResp) {
	// describe_image 子任务循环：最多跑 maxDescribeRounds 轮。
	// 拦截 pro emit 的 describe_image tool_call，proxy 内部用 mimo-v2.5 读图，
	// 把回执作为 role=tool 注入下一轮 messages，让 pro 给自然语言回答。
	// 中间轮（pro 调 describe_image 那轮）的 tool_call 链路在 proxy 内部消化，
	// 最终 Responses API 序列化时只暴露给 Codex 最后一轮的输出。
	workingReq := chatReq
	workingReq.Stream = false
	var totalUsage *MimoUsage
	for round := 0; round < maxDescribeRounds; round++ {
		chatResp := executeUpstreamChat(inbound, workingReq)
		addUsage(&totalUsage, chatResp.Usage)
		if chatResp.hasError() {
			return ResponseEnvelope{}, &chatResp
		}

		descCalls := extractDescribeImageCalls(chatResp)
		if len(descCalls) == 0 {
			chatResp.Usage = totalUsage
			return buildNonStreamEnvelope(workingReq, chatResp, responseID), nil
		}

		workingReq.Messages = append(workingReq.Messages, ChatMessage{
			Role:      "assistant",
			Content:   "",
			ToolCalls: assistantToolCallsFromParsed(chatResp.ToolCalls),
		})
		for _, dc := range descCalls {
			var desc string
			var usage *MimoUsage
			if dc.ErrorText != "" {
				desc = dc.ErrorText
			} else {
				descResp := callDescribeImage(inbound, dc, imageMap)
				addUsage(&totalUsage, descResp.Usage)
				usage = descResp.Usage
				if descResp.hasError() {
					desc = descResp.ErrorCode + ": " + descResp.ErrorMessage
				} else {
					desc = descResp.Content
				}
			}
			log.Printf("[Proxy] describe_image: call_id=%s desc_len=%d usage=%v", dc.CallID, len(desc), usage)
			workingReq.Messages = append(workingReq.Messages, ChatMessage{
				Role:       "tool",
				Content:    desc,
				ToolCallID: dc.CallID,
			})
		}
	}

	log.Printf("[Proxy] describe_image exceeded max rounds=%d", maxDescribeRounds)
	errResp := chatError(http.StatusBadGateway, "describe_image_max_rounds", "describe_image did not converge within max rounds")
	return ResponseEnvelope{}, &errResp
}

func buildNonStreamEnvelope(chatReq ChatRequest, chatResp parsedChatResp, responseID string) ResponseEnvelope {
	var output []map[string]interface{}
	var assistantMessage *ChatMessage

	if responseID == "" {
		responseID = chatResp.ID
	}
	if responseID == "" {
		responseID = fmt.Sprintf("resp-%d", time.Now().UnixMilli())
	}

	// reasoning
	if chatResp.ReasoningContent != "" {
		output = append(output, map[string]interface{}{
			"type":    "reasoning",
			"summary": []map[string]string{{"type": "summary_text", "text": summarizeReasoning(chatResp.ReasoningContent)}},
		})
	}

	// tool_calls → function_call output items
	var toolCallsOutput []map[string]interface{}
	for _, tc := range chatResp.ToolCalls {
		toolCallsOutput = append(toolCallsOutput, map[string]interface{}{
			"type":      "function_call",
			"call_id":   tc.ID,
			"name":      tc.Name,
			"arguments": tc.Arguments,
		})
	}
	output = append(output, toolCallsOutput...)

	// content：含 XML 兜底
	if chatResp.Content != "" {
		if calls, ok := parseTextToolCalls(chatResp.Content); ok {
			output = append(output, calls...)
			assistantMessage = buildStoredAssistantMessage("", chatResp.ReasoningContent, toolCallsFromResponseItems(calls))
		} else {
			output = append(output, map[string]interface{}{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]string{{"type": "output_text", "text": chatResp.Content}},
			})
			assistantMessage = buildStoredAssistantMessage(chatResp.Content, chatResp.ReasoningContent, toolCallsFromResponseItems(toolCallsOutput))
		}
	} else {
		assistantMessage = buildStoredAssistantMessage("", chatResp.ReasoningContent, toolCallsFromResponseItems(toolCallsOutput))
	}

	result := makeResponse(responseID, "completed", chatReq.Model, output, convertUsage(chatResp.Usage))
	return finalizeResponseEnvelope(result, chatReq, assistantMessage)
}

func writeImageUnsupportedResponse(w http.ResponseWriter, stream bool, model string) {
	message := "mimo-v2.5-pro does not accept inline image input. Route image tasks to explicit model mimo-v2.5, or send text-only input to mimo-v2.5-pro."
	if !stream {
		writeErrorResponse(w, http.StatusBadRequest, "image_not_supported_by_model", message)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorResponse(w, http.StatusInternalServerError, "streaming_not_supported", "Streaming not supported")
		return
	}
	streamID := fmt.Sprintf("resp-%d", time.Now().UnixMilli())
	sendEvent := func(eventType string, data map[string]interface{}) {
		data["type"] = eventType
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	errResp := chatError(http.StatusBadRequest, "image_not_supported_by_model", message)
	sendEvent("response.created", map[string]interface{}{
		"response": makeResponse(streamID, "in_progress", model, []map[string]interface{}{}, nil),
	})
	sendEvent("response.completed", map[string]interface{}{
		"response": streamErrorResponse(streamID, model, &errResp),
	})
}

func streamErrorResponse(id, model string, errResp *parsedChatResp) map[string]interface{} {
	response := makeResponse(id, "failed", model, []map[string]interface{}{}, nil)
	response["error"] = map[string]interface{}{
		"code":    errResp.ErrorCode,
		"message": errResp.ErrorMessage,
		"type":    "invalid_request_error",
	}
	return response
}

func handleStreamFromNonStream(w http.ResponseWriter, inbound *http.Request, chatReq ChatRequest, imageMap map[string]string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorResponse(w, http.StatusInternalServerError, "streaming_not_supported", "Streaming not supported")
		return
	}

	streamID := fmt.Sprintf("resp-%d", time.Now().UnixMilli())
	sendEvent := func(eventType string, data map[string]interface{}) {
		data["type"] = eventType
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	sendEvent("response.created", map[string]interface{}{
		"response": makeResponse(streamID, "in_progress", chatReq.Model, []map[string]interface{}{}, nil),
	})

	envelope, errResp := runNonStream(inbound, chatReq, imageMap, streamID)
	if errResp != nil {
		response := streamErrorResponse(streamID, chatReq.Model, errResp)
		sendEvent("response.completed", map[string]interface{}{"response": response})
		return
	}

	if output, ok := envelope.ResponseObject["output"].([]map[string]interface{}); ok {
		for outputIndex, item := range output {
			sendEvent("response.output_item.added", map[string]interface{}{"output_index": outputIndex, "item": item})
			if itemType := stringValue(item["type"]); itemType == "message" {
				text := ""
				if content, ok := item["content"].([]map[string]string); ok && len(content) > 0 {
					text = content[0]["text"]
				}
				sendEvent("response.content_part.added", map[string]interface{}{
					"output_index":  outputIndex,
					"content_index": 0,
					"part":          map[string]string{"type": "output_text", "text": ""},
				})
				if text != "" {
					sendEvent("response.output_text.delta", map[string]interface{}{
						"output_index":  outputIndex,
						"content_index": 0,
						"delta":         text,
					})
				}
				sendEvent("response.output_text.done", map[string]interface{}{"output_index": outputIndex, "content_index": 0})
				sendEvent("response.content_part.done", map[string]interface{}{
					"output_index":  outputIndex,
					"content_index": 0,
					"part":          map[string]string{"type": "output_text"},
				})
			}
			sendEvent("response.output_item.done", map[string]interface{}{"output_index": outputIndex, "item": item})
		}
	}

	sendEvent("response.completed", map[string]interface{}{
		"response": envelope.ResponseObject,
	})
}

// ---- 流式 ----

func handleStream(w http.ResponseWriter, inbound *http.Request, chatReq ChatRequest) {
	chatReq.Stream = true
	body, _ := json.Marshal(chatReq)
	req, _ := http.NewRequest("POST", mimoBase+"/chat/completions", bytes.NewReader(body))
	if err := setMimoHeaders(req, inbound); err != nil {
		writeErrorResponse(w, http.StatusInternalServerError, "missing_mimo_api_key", err.Error())
		return
	}

	resp, err := sendUpstream(req)
	if err != nil {
		writeErrorResponse(w, http.StatusBadGateway, "upstream_request_failed", err.Error())
		return
	}
	var respBody []byte
	defer func() { closeUpstream(resp, respBody) }()
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ = io.ReadAll(resp.Body)
		log.Printf("[Proxy] MiMo stream error %d: %s", resp.StatusCode, string(respBody))
		writeErrorResponse(w, resp.StatusCode, "upstream_error", string(respBody))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorResponse(w, http.StatusInternalServerError, "streaming_not_supported", "Streaming not supported")
		return
	}

	streamID := fmt.Sprintf("resp-%d", time.Now().UnixMilli())

	sendEvent := func(eventType string, data map[string]interface{}) {
		data["type"] = eventType
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	sendEvent("response.created", map[string]interface{}{
		"response": makeResponse(streamID, "in_progress", chatReq.Model, []map[string]interface{}{}, nil),
	})

	var reasoningBuf, contentBuf string
	var lastUsage *MimoUsage
	var toolCallsBuf []map[string]interface{}
	outputIndex := 0
	reasoningSent := false
	contentStarted := false

	toolCallStates := map[int]*struct {
		id   string
		name string
		args string
	}{}

	sendReasoning := func() {
		if reasoningSent || reasoningBuf == "" {
			return
		}
		reasoningSent = true
		item := map[string]interface{}{
			"type":    "reasoning",
			"summary": []map[string]string{{"type": "summary_text", "text": summarizeReasoning(reasoningBuf)}},
		}
		sendEvent("response.output_item.added", map[string]interface{}{"output_index": outputIndex, "item": item})
		sendEvent("response.output_item.done", map[string]interface{}{"output_index": outputIndex, "item": item})
		outputIndex++
	}

	startContent := func() {
		if contentStarted {
			return
		}
		contentStarted = true
		item := map[string]interface{}{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]string{{"type": "output_text", "text": ""}},
		}
		sendEvent("response.output_item.added", map[string]interface{}{"output_index": outputIndex, "item": item})
		sendEvent("response.content_part.added", map[string]interface{}{
			"output_index":  outputIndex,
			"content_index": 0,
			"part":          map[string]string{"type": "output_text", "text": ""},
		})
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			break
		}

		var chunk ChatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			if chunk.Usage != nil {
				lastUsage = chunk.Usage
			}
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta
		if chunk.Usage != nil {
			lastUsage = chunk.Usage
		}

		if delta.ReasoningContent != nil {
			reasoningBuf += *delta.ReasoningContent
		}

		if delta.ToolCalls != nil {
			if tcArr, ok := delta.ToolCalls.([]interface{}); ok {
				for _, tc := range tcArr {
					tcMap, ok := tc.(map[string]interface{})
					if !ok {
						continue
					}
					idx := 0
					if i, ok := tcMap["index"].(float64); ok {
						idx = int(i)
					}
					state, exists := toolCallStates[idx]
					if !exists {
						state = &struct {
							id   string
							name string
							args string
						}{}
						toolCallStates[idx] = state
					}
					if id, ok := tcMap["id"].(string); ok && id != "" {
						state.id = id
					}
					if fn, ok := tcMap["function"].(map[string]interface{}); ok {
						if n, ok := fn["name"].(string); ok && n != "" {
							state.name = n
						}
						if a, ok := fn["arguments"].(string); ok && a != "" {
							state.args += a
						}
					}
				}
			}
		}

		if delta.Content != nil && *delta.Content != "" {
			sendReasoning()
			startContent()
			sendEvent("response.output_text.delta", map[string]interface{}{
				"output_index":  outputIndex,
				"content_index": 0,
				"delta":         *delta.Content,
			})
			contentBuf += *delta.Content
		}

		if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
			sendReasoning()
			indices := make([]int, 0, len(toolCallStates))
			for idx := range toolCallStates {
				indices = append(indices, idx)
			}
			sort.Ints(indices)
			for _, idx := range indices {
				state := toolCallStates[idx]
				item := map[string]interface{}{
					"type":      "function_call",
					"id":        state.id,
					"call_id":   state.id,
					"name":      state.name,
					"arguments": state.args,
					"status":    "completed",
				}
				sendEvent("response.output_item.added", map[string]interface{}{"output_index": outputIndex, "item": item})
				sendEvent("response.output_item.done", map[string]interface{}{"output_index": outputIndex, "item": item})
				toolCallsBuf = append(toolCallsBuf, item)
				outputIndex++
			}
		}
	}

	sendReasoning()

	textToolCalls, contentIsToolCalls := parseTextToolCalls(contentBuf)
	if contentStarted {
		sendEvent("response.output_text.done", map[string]interface{}{
			"output_index":  outputIndex,
			"content_index": 0,
		})
		sendEvent("response.content_part.done", map[string]interface{}{
			"output_index":  outputIndex,
			"content_index": 0,
			"part":          map[string]string{"type": "output_text"},
		})
		sendEvent("response.output_item.done", map[string]interface{}{
			"output_index": outputIndex,
			"item": map[string]interface{}{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]string{{"type": "output_text"}},
			},
		})
		outputIndex++
	}
	if contentIsToolCalls {
		for _, item := range textToolCalls {
			sendEvent("response.output_item.added", map[string]interface{}{"output_index": outputIndex, "item": item})
			sendEvent("response.output_item.done", map[string]interface{}{"output_index": outputIndex, "item": item})
			toolCallsBuf = append(toolCallsBuf, item)
			outputIndex++
		}
	}

	output := []map[string]interface{}{}
	if reasoningSent {
		output = append(output, map[string]interface{}{
			"type":    "reasoning",
			"summary": []map[string]string{{"type": "summary_text", "text": summarizeReasoning(reasoningBuf)}},
		})
	}
	output = append(output, toolCallsBuf...)
	if contentBuf != "" && !contentIsToolCalls {
		output = append(output, map[string]interface{}{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]string{{"type": "output_text", "text": contentBuf}},
		})
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[Proxy] Stream scanner error: %v", err)
	}
	log.Printf("[Proxy] Stream done: reasoning=%d content=%d tools=%d", len(reasoningBuf), len(contentBuf), len(toolCallsBuf))

	storedToolCalls := toolCallsFromResponseItems(toolCallsBuf)
	if contentIsToolCalls {
		storedToolCalls = toolCallsFromResponseItems(textToolCalls)
	}
	assistantMessage := buildStoredAssistantMessage(contentBuf, reasoningBuf, storedToolCalls)
	result := makeResponse(streamID, "completed", chatReq.Model, output, convertUsage(lastUsage))
	envelope := finalizeResponseEnvelope(result, chatReq, assistantMessage)

	sendEvent("response.completed", map[string]interface{}{
		"response": envelope.ResponseObject,
	})
}

func handleCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeErrorResponse(w, http.StatusNotImplemented, "compact_not_supported", compactUnsupportedMessage())
}

func handleGetResponse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	responseID := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		writeErrorResponse(w, http.StatusNotFound, "response_not_found", "response not found")
		return
	}
	stored, ok := responseStore.Get(responseID)
	if !ok || stored.ResponseObject == nil {
		writeErrorResponse(w, http.StatusNotFound, "response_not_found", "response not found")
		return
	}
	writeJSON(w, http.StatusOK, stored.ResponseObject)
}

// ---- 主路由 ----

func handleResponses(w http.ResponseWriter, r *http.Request) {
	rawBody, _ := io.ReadAll(r.Body)
	var req ResponsesRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		http.Error(w, "Invalid JSON", 400)
		return
	}

	toolNames := rawToolNames(req.Tools)
	instructionsLen := len(req.Instructions)
	legacyMode := envBoolOrDefault("MIMO_PROXY_LEGACY_MODE", false)
	if legacyMode {
		log.Printf("[Proxy] LEGACY MODE on: namespace flatten=off, describe_image=off, concurrency=1/1500ms")
	}
	log.Printf("[Proxy] tools=%v instructions_len=%d", toolNames, instructionsLen)

	messages, hasImages := parseInput(req.Input)
	if req.PreviousResponseID != "" {
		stored, ok := responseStore.Get(req.PreviousResponseID)
		if !ok {
			writeErrorResponse(w, http.StatusNotFound, "response_not_found", "previous_response_id not found")
			return
		}
		messages = append(cloneChatMessages(stored.ChatMessages), messages...)
		if !rawJSONPresent(req.Reasoning) {
			req.Reasoning = cloneRawMessage(stored.Thinking)
		}
		if req.ParallelToolCalls == nil && stored.ParallelTools != nil {
			parallel := *stored.ParallelTools
			req.ParallelToolCalls = &parallel
		}
	}
	maxTokens := req.MaxOutputTokens
	if maxTokens < 16384 {
		maxTokens = 32768
	}

	temp := req.Temperature
	topP := req.TopP

	chatToolsMeta := convertToolsStable(req.Tools)
	chatToolChoice, chatToolsMeta, toolChoiceState := normalizeToolChoice(req.ToolChoice, chatToolsMeta)
	chatTools := chatToolsMeta.rawMessage()

	chatThinking := mapReasoningToThinking(req.Reasoning)

	selectedModel := resolveRequestModel(req.Model, hasImages)
	if rawModel := strings.TrimSpace(req.Model); rawModel != "" {
		if _, err := resolveModelName(rawModel); err != nil {
			log.Printf("[Proxy] aliasing unsupported request model %q to %s", rawModel, selectedModel)
		}
	}
	if hasImages && selectedModel != visionMimoModel {
		log.Printf("[Proxy] image input detected; downgrading request model from %s to %s", selectedModel, visionMimoModel)
		selectedModel = visionMimoModel
	}

	chatReq := ChatRequest{
		Model:               selectedModel,
		Messages:            messages,
		MaxCompletionTokens: maxTokens,
		Stream:              req.Stream,
		Temperature:         temp,
		TopP:                topP,
		Tools:               chatTools,
		ToolChoice:          chatToolChoice,
		Thinking:            chatThinking,
		ParallelToolCalls:   req.ParallelToolCalls,
	}

	if req.Instructions != "" {
		hasSystem := false
		for _, m := range messages {
			if m.Role == "system" {
				hasSystem = true
				break
			}
		}
		if !hasSystem {
			chatReq.Messages = append([]ChatMessage{{Role: "system", Content: req.Instructions}}, chatReq.Messages...)
		}
	}

	log.Printf(
		"[Proxy] %s | stream=%v | previous_response_id=%t | parallel_tool_calls=%v | tools_in=%d | tools_forwarded=%d | tools_dropped=%d | dropped_types=%v | tool_choice=%s | images=%v | model=%s",
		r.URL.Path,
		req.Stream,
		req.PreviousResponseID != "",
		req.ParallelToolCalls,
		chatToolsMeta.InputCount,
		chatToolsMeta.ForwardedCount,
		chatToolsMeta.DroppedCount,
		chatToolsMeta.DroppedToolTypes,
		toolChoiceState,
		hasImages,
		selectedModel,
	)

	if req.Stream {
		handleStream(w, r, chatReq)
	} else {
		handleNonStream(w, r, chatReq, nil)
	}
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequest("GET", mimoBase+"/models", nil)
	if err := setMimoHeaders(req, r); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	resp, err := sendUpstream(req)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	var respBody []byte
	defer func() { closeUpstream(resp, respBody) }()
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	respBody, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		http.Error(w, string(respBody), resp.StatusCode)
		return
	}
	w.Write(respBody)
}

// ---------------------------------------------------------------------------
// First-class CLI subcommands (model set/status/restart, sync)
// Replaces the external switch-mimo.sh shell script.
// ---------------------------------------------------------------------------

// validModels lists accepted model names (exact match).
var validModels = map[string]bool{
	"mimo-v2.5-pro": true,
	"mimo-v2.5":     true,
}

// modelAliases maps short names to full model identifiers.
var modelAliases = map[string]string{
	"pro":          "mimo-v2.5-pro",
	"v2.5":         "mimo-v2.5",
	"gpt-5":        "mimo-v2.5-pro",
	"gpt-5.4":      "mimo-v2.5-pro",
	"gpt-5.5":      "mimo-v2.5-pro",
	"gpt-5-mini":   "mimo-v2.5-pro",
	"gpt-5.4-mini": "mimo-v2.5-pro",
	"gpt-5.5-mini": "mimo-v2.5-pro",
}

func resolveModelName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if validModels[name] {
		return name, nil
	}
	if full, ok := modelAliases[name]; ok {
		return full, nil
	}
	return "", fmt.Errorf("unknown model %q; valid: mimo-v2.5-pro, mimo-v2.5 (aliases: pro, v2.5)", name)
}

func resolveRequestModel(raw string, hasImages bool) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		if hasImages {
			return visionMimoModel
		}
		return mimoModel
	}
	if resolved, err := resolveModelName(name); err == nil {
		if hasImages && strings.HasPrefix(name, "gpt-5") {
			return visionMimoModel
		}
		return resolved
	}
	if hasImages {
		return visionMimoModel
	}
	return mimoModel
}

// findPlist locates the launchd plist for ccmimolink / mimo_proxy.
// Priority: PLIST_PATH env var → known names → glob *mimo-proxy*.plist.
func findPlist() (string, error) {
	if p := strings.TrimSpace(os.Getenv("PLIST_PATH")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("PLIST_PATH=%s: %w", p, err)
		}
		return p, nil
	}

	home := homeDir
	dir := filepath.Join(home, "Library", "LaunchAgents")

	// Prefer well-known names first.
	for _, name := range []string{
		"com.ccmimolink.mimo-proxy.plist",
		"com.shuiyang.mimo-proxy.plist",
	} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	// Fall back to any *mimo-proxy*.plist (newest first).
	matches, _ := filepath.Glob(filepath.Join(dir, "*mimo-proxy.plist"))
	if len(matches) > 0 {
		sort.Slice(matches, func(i, j int) bool {
			si, _ := os.Stat(matches[i])
			sj, _ := os.Stat(matches[j])
			return si.ModTime().After(sj.ModTime())
		})
		return matches[0], nil
	}

	return "", fmt.Errorf("no mimo-proxy plist found in %s; set PLIST_PATH env var", dir)
}

func plistLabel(plistPath string) string {
	out, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :Label", plistPath).Output()
	if err == nil {
		if label := strings.TrimSpace(string(out)); label != "" {
			return label
		}
	}
	return strings.TrimSuffix(filepath.Base(plistPath), ".plist")
}

func readPlistModel(plistPath string) string {
	out, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :EnvironmentVariables:MIMO_MODEL", plistPath).Output()
	if err != nil {
		return "<unset>"
	}
	return strings.TrimSpace(string(out))
}

func setPlistModel(plistPath, model string) error {
	out, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Set :EnvironmentVariables:MIMO_MODEL "+model, plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("PlistBuddy set failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func plistBinaryName(plistPath string) string {
	out, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :ProgramArguments:0", plistPath).Output()
	if err == nil {
		return filepath.Base(strings.TrimSpace(string(out)))
	}
	return "mimo_proxy"
}

func pgrepFind(name string) string {
	out, err := exec.Command("pgrep", "-f", name).Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[0])
}

func launchdObservedModel(domain, label string) string {
	out, err := exec.Command("launchctl", "print", domain+"/"+label).Output()
	if err != nil {
		return ""
	}
	inEnv := false
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "environment = {") {
			inEnv = true
			continue
		}
		if inEnv && strings.Contains(trimmed, "}") {
			break
		}
		if inEnv && strings.Contains(trimmed, "MIMO_MODEL") {
			parts := strings.SplitN(trimmed, "=>", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// launchdRestart performs bootout + bootstrap — the ONLY reliable way to make
// launchd re-read EnvironmentVariables from the plist. A plain `kickstart -k`
// reuses the cached service context and will NOT pick up the new MIMO_MODEL.
func launchdRestart(plistPath, label string) error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())

	// Step 1: bootout (ignore error — service might not be running).
	exec.Command("launchctl", "bootout", domain+"/"+label).CombinedOutput()

	// Step 2: bootstrap.
	if out, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// Step 3: wait for process to appear (up to ~2.5 s).
	binaryName := plistBinaryName(plistPath)
	for i := 0; i < 5; i++ {
		time.Sleep(500 * time.Millisecond)
		if pgrepFind(binaryName) != "" {
			break
		}
	}

	// Step 4: verify launchd actually loaded the new env var.
	observed := launchdObservedModel(domain, label)
	plisted := readPlistModel(plistPath)
	if observed != "" && observed != plisted {
		return fmt.Errorf("plist says %s but launchd still shows %s; service may not have fully restarted", plisted, observed)
	}

	return nil
}

func printModelStatus() {
	plistPath, err := findPlist()
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
		return
	}
	label := plistLabel(plistPath)
	binaryName := plistBinaryName(plistPath)
	domain := fmt.Sprintf("gui/%d", os.Getuid())

	fmt.Printf("  plist          : %s\n", plistPath)
	fmt.Printf("  label          : %s\n", label)
	fmt.Printf("  MIMO_MODEL     : %s\n", readPlistModel(plistPath))

	pid := pgrepFind(binaryName)
	if pid != "" {
		fmt.Printf("  process (pid)  : %s\n", pid)
		if out, err := exec.Command("ps", "-p", pid, "-o", "etime=").Output(); err == nil {
			if etime := strings.TrimSpace(string(out)); etime != "" {
				fmt.Printf("  uptime         : %s\n", etime)
			}
		}
	} else {
		fmt.Printf("  process        : <not running>\n")
	}

	if observed := launchdObservedModel(domain, label); observed != "" {
		fmt.Printf("  MIMO_MODEL(live): %s\n", observed)
	}
}

// ---------------------------------------------------------------------------
// Subcommand dispatch
// ---------------------------------------------------------------------------

func runSubcommand(args []string) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "model":
		return runModelSubcommand(args[1:])
	case "sync":
		return runSyncSubcommand()
	case "help", "--help", "-h":
		printSubcommandHelp()
		return true, 0
	default:
		// Not a recognized subcommand → fall through to legacy flag parsing.
		return false, 0
	}
}

func runModelSubcommand(args []string) (bool, int) {
	if len(args) == 0 {
		fmt.Println("Usage: ccmimolink model {set|status|restart}")
		fmt.Println()
		fmt.Println("  set <model> [--restart|-r]   Set MIMO_MODEL in plist (optionally restart)")
		fmt.Println("  status                       Show current model, process, and plist info")
		fmt.Println("  restart                      Restart the launchd service")
		fmt.Println()
		fmt.Println("  Valid models: mimo-v2.5-pro, mimo-v2.5 (aliases: pro, v2.5)")
		return true, 0
	}
	switch args[0] {
	case "set":
		return runModelSet(args[1:])
	case "status":
		printModelStatus()
		return true, 0
	case "restart":
		return runModelRestart()
	default:
		fmt.Printf("Unknown model subcommand: %q\n", args[0])
		fmt.Println("Valid: set, status, restart")
		return true, 1
	}
}

func runModelSet(args []string) (bool, int) {
	if len(args) == 0 {
		fmt.Println("Usage: ccmimolink model set <model> [--restart|-r]")
		fmt.Println("  Valid models: mimo-v2.5-pro, mimo-v2.5 (aliases: pro, v2.5)")
		return true, 1
	}

	resolved, err := resolveModelName(args[0])
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return true, 1
	}

	wantRestart := false
	for _, a := range args[1:] {
		switch a {
		case "--restart", "-r":
			wantRestart = true
		default:
			fmt.Printf("Unknown flag: %q\n", a)
			return true, 1
		}
	}

	plistPath, err := findPlist()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return true, 1
	}

	if err := setPlistModel(plistPath, resolved); err != nil {
		fmt.Printf("Error: %v\n", err)
		return true, 1
	}

	fmt.Printf("✓ plist updated: MIMO_MODEL = %s\n", resolved)

	if wantRestart {
		label := plistLabel(plistPath)
		fmt.Printf("→ restarting service %s ...\n", label)
		if err := launchdRestart(plistPath, label); err != nil {
			fmt.Printf("Error: %v\n", err)
			return true, 1
		}
		fmt.Println("✓ service restarted")
		printModelStatus()
	} else {
		fmt.Printf("→ run './ccmimolink model restart' or './ccmimolink model set %s --restart' to apply\n", resolved)
	}

	return true, 0
}

func runModelRestart() (bool, int) {
	plistPath, err := findPlist()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return true, 1
	}

	label := plistLabel(plistPath)
	fmt.Printf("→ restarting service %s ...\n", label)

	if err := launchdRestart(plistPath, label); err != nil {
		fmt.Printf("Error: %v\n", err)
		return true, 1
	}

	fmt.Println("✓ service restarted")
	printModelStatus()
	return true, 0
}

func runSyncSubcommand() (bool, int) {
	setupLogging()
	if err := syncCCSwitchAndCodex(); err != nil {
		fmt.Printf("Error: sync failed: %v\n", err)
		return true, 1
	}
	fmt.Println("✓ sync completed (cc switch + codex config + auth.json)")
	return true, 0
}

func printSubcommandHelp() {
	fmt.Println("ccmimolink — Claude Code MiMo proxy with OpenAI-compatible API")
	fmt.Println()
	fmt.Println("Subcommands:")
	fmt.Println("  model set <name> [--restart|-r]  Set MIMO_MODEL in plist")
	fmt.Println("  model status                     Show current model, process, plist info")
	fmt.Println("  model restart                    Restart the launchd service")
	fmt.Println("  sync                             Sync cc switch + codex config + auth.json")
	fmt.Println()
	fmt.Println("Legacy flags (for direct non-launchd runs):")
	fmt.Println("  --v2.5          Use mimo-v2.5 for text requests")
	fmt.Println("  --v2.5-pro      Use mimo-v2.5-pro for text requests")
	fmt.Println("  --sync-only     Sync cc switch and codex config, then exit")
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	// First-class subcommands take precedence over legacy flags. This lets
	// users do `./ccmimolink model set mimo-v2.5-pro` to switch models and
	// restart the launchd service without needing the external
	// switch-mimo.sh helper.
	if handled, exitCode := runSubcommand(os.Args[1:]); handled {
		os.Exit(exitCode)
	}

	applyModelFlag()
	setupLogging()
	if err := syncCCSwitchAndCodex(); err != nil {
		log.Fatal("[CCMimoLink] startup sync failed: ", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", 405)
			return
		}
		handleResponses(w, r)
	})
	mux.HandleFunc("/v1/responses/compact", handleCompact)
	mux.HandleFunc("/v1/responses/", handleGetResponse)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	addr := "127.0.0.1:" + proxyPort
	log.Printf("[CCMimoLink] http://%s/v1", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
