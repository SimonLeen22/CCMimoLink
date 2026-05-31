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
	defaultMimoModel         = "mimo-v2.5"
	proMimoModel             = "mimo-v2.5-pro"
	visionMimoModel          = "mimo-v2.5"
	defaultProxyPort         = "9876"
	defaultResponseStoreSize = 1000
)

var (
	mimoBase      = envOrDefault("MIMO_BASE_URL", defaultMimoBase)
	mimoKey       = strings.TrimSpace(os.Getenv("MIMO_API_KEY"))
	mimoModel     = envOrDefault("MIMO_MODEL", defaultMimoModel)
	proxyPort     = envOrDefault("MIMO_PROXY_PORT", defaultProxyPort)
	client        = &http.Client{}
	limiter       = newUpstreamLimiter()
	responseStore = newResponseStore(envIntOrDefault("MIMO_PROXY_RESPONSE_STORE_MAX", defaultResponseStoreSize))
	skipCCSwitchSync = strings.EqualFold(strings.TrimSpace(os.Getenv("MIMO_PROXY_SKIP_CC_SWITCH_SYNC")), "true")
	ccSwitchSettingsPath = envOrDefault("CC_SWITCH_SETTINGS_PATH", filepath.Join(envOrDefault("HOME", "/Users/shuiyang"), ".cc-switch", "settings.json"))
	ccSwitchDBPath = envOrDefault("CC_SWITCH_DB_PATH", filepath.Join(envOrDefault("HOME", "/Users/shuiyang"), ".cc-switch", "cc-switch.db"))
	codexConfigPath = envOrDefault("CODEX_CONFIG_PATH", filepath.Join(envOrDefault("HOME", "/Users/shuiyang"), ".codex", "config.toml"))
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
	if updated == content {
		return result, nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
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
	settings, err := loadCCSwitchSettings(ccSwitchSettingsPath)
	if err != nil {
		return err
	}
	provider, err := loadCCSwitchProvider(ccSwitchDBPath, settings.CurrentProviderCodex)
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(provider.Name), "mimo") {
		fallback, fallbackErr := loadFirstMimoProvider(ccSwitchDBPath)
		if fallbackErr != nil {
			return fmt.Errorf("current cc switch Codex provider is %q, not Xiaomi MiMo; please configure Xiaomi MiMo in cc switch first", provider.Name)
		}
		log.Printf("[CCMimoLink] current cc switch Codex provider is %q; using Xiaomi MiMo provider %q instead", provider.Name, fallback.ID)
		provider = fallback
	}
	apiKey, err := extractCCSwitchAPIKey(provider.SettingsConfig)
	if err != nil {
		return err
	}
	if err := rewriteCCSwitchProxyRoute(ccSwitchDBPath, provider.ID); err != nil {
		return err
	}
	update, err := updateCodexConfig(codexConfigPath, apiKey)
	if err != nil {
		return err
	}
	log.Printf("[CCMimoLink] synced cc switch Xiaomi MiMo provider %s to local route %s", provider.ID, localProxyURL())
	log.Printf("[CCMimoLink] backed up Codex config to %s", update.BackupPath)
	if update.Updated {
		log.Printf("[CCMimoLink] updated Codex Xiaomi MiMo headers from cc switch API key")
	} else {
		log.Printf("[CCMimoLink] Codex config already matched the required Xiaomi MiMo route and API key")
	}
	log.Printf("[CCMimoLink] restart cc switch and restart Codex to apply the updated Xiaomi MiMo routing and API key")
	return nil
}

func runStartupSyncOnly() error {
	return syncCCSwitchAndCodex()
}

func newUpstreamLimiter() *UpstreamLimiter {
	concurrency := envIntOrDefault("MIMO_PROXY_MAX_CONCURRENT", 1)
	minIntervalMS := envIntOrDefault("MIMO_PROXY_MIN_INTERVAL_MS", 1500)
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
	return n
}

func convertToolsStable(raw json.RawMessage) normalizedTools {
	result := normalizedTools{SupportedNames: map[string]struct{}{}}
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
			result.SupportedNames[name] = struct{}{}
			continue
		}

		toolType := stringValue(tool["type"])
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
		result.SupportedNames[name] = struct{}{}
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

func handleNonStream(w http.ResponseWriter, inbound *http.Request, chatReq ChatRequest) {
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

	respBody, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Printf("[Proxy] MiMo error %d: %s", resp.StatusCode, string(respBody))
		writeErrorResponse(w, resp.StatusCode, "upstream_error", string(respBody))
		return
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
	json.Unmarshal(respBody, &chatResp)

	var output []map[string]interface{}
	var assistantMessage *ChatMessage
	if len(chatResp.Choices) > 0 {
		msg := chatResp.Choices[0].Message

		if msg.ReasoningContent != "" {
			output = append(output, map[string]interface{}{
				"type":    "reasoning",
				"summary": []map[string]string{{"type": "summary_text", "text": summarizeReasoning(msg.ReasoningContent)}},
			})
		}

		var toolCallsOutput []map[string]interface{}
		if msg.ToolCalls != nil {
			if tc, ok := msg.ToolCalls.([]interface{}); ok {
				for _, call := range tc {
					c, ok := call.(map[string]interface{})
					if !ok {
						continue
					}
					fn, _ := c["function"].(map[string]interface{})
					toolCallsOutput = append(toolCallsOutput, map[string]interface{}{
						"type":      "function_call",
						"call_id":   c["id"],
						"name":      fn["name"],
						"arguments": fn["arguments"],
					})
				}
			}
		}
		output = append(output, toolCallsOutput...)

		messageContent := msg.Content
		if msg.Content != "" {
			if calls, ok := parseTextToolCalls(msg.Content); ok {
				output = append(output, calls...)
				assistantMessage = buildStoredAssistantMessage("", msg.ReasoningContent, toolCallsFromResponseItems(calls))
			} else {
				output = append(output, map[string]interface{}{
					"type":    "message",
					"role":    "assistant",
					"content": []map[string]string{{"type": "output_text", "text": msg.Content}},
				})
				assistantMessage = buildStoredAssistantMessage(messageContent, msg.ReasoningContent, toolCallsFromResponseItems(toolCallsOutput))
			}
		} else {
			assistantMessage = buildStoredAssistantMessage("", msg.ReasoningContent, toolCallsFromResponseItems(toolCallsOutput))
		}
	}

	result := makeResponse(chatResp.ID, "completed", chatReq.Model, output, convertUsage(chatResp.Usage))
	envelope := finalizeResponseEnvelope(result, chatReq, assistantMessage)
	writeJSON(w, http.StatusOK, envelope.ResponseObject)
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

	selectedModel := mimoModel
	if hasImages {
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
		handleNonStream(w, r, chatReq)
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

func main() {
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
