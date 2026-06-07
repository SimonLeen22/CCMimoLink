package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════
// Anthropic Messages Adapter
// ═══════════════════════════════════════════════════════════════════════════
//
// 核心修复：convertOpenAIMessagesToAnthropic()
// OpenAI 的 tool call 续接格式:
//   {role:"assistant", tool_calls:[...]}
//   {role:"tool", tool_call_id:"...", content:"..."}
// 必须转换为 Anthropic 格式:
//   {role:"assistant", content:[{type:"tool_use", id:"...", name:"...", input:{...}}]}
//   {role:"user", content:[{type:"tool_result", tool_use_id:"...", content:"..."}]}
// 否则 MiMo 遇到 role:tool 会截断消息解析，只读 ~13 tokens，导致模型乱回答。

var anthropicVersion = "2023-06-01"

// normalizeToolCalls accepts BOTH concrete types that flow through the proxy:
//
//	[]map[string]interface{}  — produced by parseInput + toolCallsFromResponseItems (PRODUCTION)
//	[]interface{}             — produced by some unmarshals + the older unit tests
func normalizeToolCalls(tc interface{}) []map[string]interface{} {
	switch v := tc.(type) {
	case []map[string]interface{}:
		return v
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(v))
		for _, e := range v {
			if m, ok := e.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// toolCallNameArgsID tolerates function maps of type map[string]string OR
// map[string]interface{} (both occur in this codebase).
func toolCallNameArgsID(tc map[string]interface{}) (name, args, id string) {
	id, _ = tc["id"].(string)
	switch fn := tc["function"].(type) {
	case map[string]interface{}:
		name, _ = fn["name"].(string)
		args, _ = fn["arguments"].(string)
	case map[string]string:
		name = fn["name"]
		args = fn["arguments"]
	}
	return
}

func contentAsString(c interface{}) string {
	if s, ok := c.(string); ok {
		return s
	}
	return ""
}

// ── Diagnostic dumps (env-gated, off by default) ─────────────────────────────
// Set MIMO_DEBUG_DUMP=<dir> to capture the exact upstream request body and a
// tee of MiMo's raw SSE response per stream. Used to diagnose /goal hangs.

func debugDumpDir() string {
	return strings.TrimSpace(os.Getenv("MIMO_DEBUG_DUMP"))
}

func debugDumpUpstreamBody(body []byte) string {
	dir := debugDumpDir()
	if dir == "" {
		return ""
	}
	tag := time.Now().Format("20060102_150405.000")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "req_"+tag+".json"), body, 0o600)
	log.Printf("[DebugDump] wrote upstream request req_%s.json (%d bytes)", tag, len(body))
	return tag
}

// debugTeeUpstreamResponse wraps resp.Body so every byte MiMo streams is also
// written to resp_<tag>.sse. Returns a closer (nil when dumping is disabled).
func debugTeeUpstreamResponse(resp *http.Response, tag string) func() {
	dir := debugDumpDir()
	if dir == "" || tag == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "resp_"+tag+".sse"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.TeeReader(resp.Body, f), resp.Body}
	return func() { _ = f.Close() }
}

func anthropicUpstreamURL() string {
	// mimoBase = "https://token-plan-cn.xiaomimimo.com/v1"
	// Anthropic endpoint = "https://token-plan-cn.xiaomimimo.com/anthropic/v1/messages"
	// 需要去掉 mimoBase 末尾的 "/v1"，再拼 "/anthropic/v1/messages"
	base := strings.TrimRight(mimoBase, "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/anthropic/v1/messages"
}

// convertOpenAIMessagesToAnthropic converts OpenAI chat/completions format messages
// to Anthropic Messages API format.
//
// Key invariant enforced: strict user/assistant alternation with NO consecutive
// same-role messages. Parallel tool calls (multiple consecutive assistant messages
// each with one tool_use) are MERGED into a single assistant message with multiple
// tool_use blocks. Consecutive user messages (e.g. environment_context + prompt) are
// coalesced into one. This is the fix for the /goal hang.
func convertOpenAIMessagesToAnthropic(messages []ChatMessage) []map[string]interface{} {
	var result []map[string]interface{}
	var curRole string
	var curBlocks []map[string]interface{}

	flush := func() {
		if len(curBlocks) == 0 {
			curRole = ""
			return
		}
		var content interface{}
		// Single text block → emit as a plain string (keeps the common case clean
		// and matches Anthropic's string-content shorthand).
		if len(curBlocks) == 1 && curBlocks[0]["type"] == "text" {
			content = curBlocks[0]["text"]
		} else {
			content = curBlocks
		}
		result = append(result, map[string]interface{}{"role": curRole, "content": content})
		curRole = ""
		curBlocks = nil
	}

	// addText appends text for `role`, coalescing into a trailing text block so
	// consecutive text (e.g. environment_context + prompt) becomes one block.
	addText := func(role, text string) {
		if text == "" {
			return
		}
		if curRole != "" && curRole != role {
			flush()
		}
		curRole = role
		if n := len(curBlocks); n > 0 && curBlocks[n-1]["type"] == "text" {
			curBlocks[n-1]["text"] = curBlocks[n-1]["text"].(string) + "\n" + text
			return
		}
		curBlocks = append(curBlocks, map[string]interface{}{"type": "text", "text": text})
	}

	addBlock := func(role string, block map[string]interface{}) {
		if curRole != "" && curRole != role {
			flush()
		}
		curRole = role
		curBlocks = append(curBlocks, block)
	}

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			// carried separately via the `system` field

		case "user":
			switch c := msg.Content.(type) {
			case string:
				addText("user", c)
			case []interface{}:
				for _, p := range c {
					pm, ok := p.(map[string]interface{})
					if !ok {
						continue
					}
					if t, _ := pm["type"].(string); t == "text" {
						if s, _ := pm["text"].(string); s != "" {
							addText("user", s)
						}
					} else {
						// image / other rich part — pass through best-effort
						addBlock("user", pm)
					}
				}
			default:
				if s := contentAsString(msg.Content); s != "" {
					addText("user", s)
				}
			}

		case "assistant":
			if t := contentAsString(msg.Content); t != "" {
				addText("assistant", t)
			}
			for _, tc := range normalizeToolCalls(msg.ToolCalls) {
				name, argsStr, id := toolCallNameArgsID(tc)
				if name == "" || id == "" {
					continue
				}
				var input interface{}
				if argsStr != "" {
					_ = json.Unmarshal([]byte(argsStr), &input)
				}
				if input == nil {
					input = map[string]interface{}{}
				}
				addBlock("assistant", map[string]interface{}{
					"type": "tool_use", "id": id, "name": name, "input": input,
				})
			}
			// reasoning-only messages contribute nothing → naturally dropped

		case "tool":
			// 空 tool_use_id 会让 Anthropic 返回 400，直接跳过
			if msg.ToolCallID == "" {
				continue
			}
			content := ""
			if s, ok := msg.Content.(string); ok {
				content = s
			} else if msg.Content != nil {
				b, _ := json.Marshal(msg.Content)
				content = string(b)
			}
			addBlock("user", map[string]interface{}{
				"type": "tool_result", "tool_use_id": msg.ToolCallID, "content": content,
			})
		}
	}
	flush()
	return result
}

// convertToolsToAnthropicFormat 把 OpenAI function-tool 格式转换为 Anthropic 格式
func convertToolsToAnthropicFormat(chatTools json.RawMessage) []interface{} {
	if !rawJSONPresent(chatTools) {
		return nil
	}
	var tools []map[string]interface{}
	if err := json.Unmarshal(chatTools, &tools); err != nil {
		return nil
	}
	var result []interface{}
	for _, tool := range tools {
		var name, description string
		var parameters interface{}

		if fn, ok := tool["function"].(map[string]interface{}); ok {
			name = stringValue(fn["name"])
			description = stringValue(fn["description"])
			parameters = fn["parameters"]
		} else {
			name = stringValue(tool["name"])
			description = stringValue(tool["description"])
			parameters = tool["parameters"]
			if parameters == nil {
				parameters = tool["input_schema"]
			}
		}
		if name == "" {
			continue
		}
		if parameters == nil {
			parameters = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		result = append(result, map[string]interface{}{
			"name":         name,
			"description":  description,
			"input_schema": parameters,
		})
	}
	return result
}

// ── Anthropic 非流式处理 ─────────────────────────────────────────────────────

func handleAnthropicNonStream(w http.ResponseWriter, inbound *http.Request, chatReq ChatRequest) {
	anthroReq := buildAnthropicRequest(chatReq)
	chatResp := executeUpstreamAnthropic(inbound, anthroReq)
	if chatResp.hasError() {
		writeErrorResponse(w, chatResp.ErrorStatus, chatResp.ErrorCode, chatResp.ErrorMessage)
		return
	}
	envelope := buildNonStreamEnvelope(chatReq, chatResp, "")
	writeJSON(w, http.StatusOK, envelope.ResponseObject)
}

// ── Anthropic 流式处理 ───────────────────────────────────────────────────────

func handleAnthropicStream(w http.ResponseWriter, inbound *http.Request, chatReq ChatRequest) {
	anthroReq := buildAnthropicRequest(chatReq)
	anthroReq["stream"] = true

	body, _ := json.Marshal(anthroReq)
	url := anthropicUpstreamURL()
	log.Printf("[Anthropic] STREAM POST %s body_len=%d", url, len(body))

	dumpTag := debugDumpUpstreamBody(body)

	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	if err := setAnthropicHeaders(req, inbound); err != nil {
		writeErrorResponse(w, http.StatusInternalServerError, "missing_mimo_api_key", err.Error())
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := sendUpstream(req)
	if err != nil {
		writeErrorResponse(w, http.StatusBadGateway, "upstream_request_failed", err.Error())
		return
	}
	defer func() { closeUpstream(resp, nil) }()

	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		msg := string(respBody)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		log.Printf("[Anthropic] stream error %d: %s", resp.StatusCode, msg)
		writeErrorResponse(w, resp.StatusCode, "upstream_error", msg)
		return
	}
	resp.Body = wrapIdleTimeout(resp.Body)
	defer resp.Body.Close()
	// Tee MiMo's raw SSE response to a dump file so a hang (MiMo holding the
	// stream open with no bytes) is distinguishable from a parser drop.
	if closer := debugTeeUpstreamResponse(resp, dumpTag); closer != nil {
		defer closer()
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

	// 解析 Anthropic SSE 响应
	var reasoningBuf, contentBuf string
	var lastUsage *MimoUsage
	var toolCallsBuf []map[string]interface{}
	outputIndex := 0
	contentStarted := false
	reasoningSent := false
	var curToolID, curToolName, curToolArgs string

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
			"output_index": outputIndex, "content_index": 0,
			"part": map[string]string{"type": "output_text", "text": ""},
		})
	}

	flushToolCall := func() {
		if curToolID == "" || curToolName == "" {
			return
		}
		if curToolArgs == "" {
			curToolArgs = "{}"
		}
		sendReasoning()
		item := map[string]interface{}{
			"type": "function_call", "id": curToolID, "call_id": curToolID,
			"name": curToolName, "arguments": curToolArgs, "status": "completed",
		}
		sendEvent("response.output_item.added", map[string]interface{}{"output_index": outputIndex, "item": item})
		sendEvent("response.output_item.done", map[string]interface{}{"output_index": outputIndex, "item": item})
		toolCallsBuf = append(toolCallsBuf, item)
		outputIndex++
		curToolID, curToolName, curToolArgs = "", "", ""
	}

	// 处理完整响应（MiMo 非标准模式：整个 JSON 在一个 data: 行）
	handleFullResponse := func(fullResp map[string]interface{}) {
		if u, ok := fullResp["usage"].(map[string]interface{}); ok {
			inT, _ := u["input_tokens"].(float64)
			outT, _ := u["output_tokens"].(float64)
			lastUsage = &MimoUsage{PromptTokens: int(inT), CompletionTokens: int(outT), TotalTokens: int(inT + outT)}
		}
		blocks, _ := fullResp["content"].([]interface{})
		for _, b := range blocks {
			block, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			switch block["type"] {
			case "thinking":
				if t, ok := block["thinking"].(string); ok && t != "" {
					reasoningBuf += t
				}
			case "text":
				if t, ok := block["text"].(string); ok && t != "" {
					sendReasoning()
					startContent()
					sendEvent("response.output_text.delta", map[string]interface{}{
						"output_index": outputIndex, "content_index": 0, "delta": t,
					})
					contentBuf += t
				}
			case "tool_use":
				sendReasoning()
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				args, _ := json.Marshal(block["input"])
				item := map[string]interface{}{
					"type": "function_call", "id": id, "call_id": id,
					"name": name, "arguments": string(args), "status": "completed",
				}
				sendEvent("response.output_item.added", map[string]interface{}{"output_index": outputIndex, "item": item})
				sendEvent("response.output_item.done", map[string]interface{}{"output_index": outputIndex, "item": item})
				toolCallsBuf = append(toolCallsBuf, item)
				outputIndex++
			}
		}
	}

	// 标准 SSE 事件处理
	processSSEEvent := func(eventType string, payload map[string]interface{}) {
		switch eventType {
		case "message_start":
			if msg, ok := payload["message"].(map[string]interface{}); ok {
				if u, ok := msg["usage"].(map[string]interface{}); ok {
					inT, _ := u["input_tokens"].(float64)
					outT, _ := u["output_tokens"].(float64)
					lastUsage = &MimoUsage{PromptTokens: int(inT), CompletionTokens: int(outT)}
				}
			}
		case "content_block_start":
			block, _ := payload["content_block"].(map[string]interface{})
			if block != nil {
				switch block["type"] {
				case "tool_use":
					flushToolCall()
					curToolID, _ = block["id"].(string)
					curToolName, _ = block["name"].(string)
					curToolArgs = ""
				}
			}
		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]interface{})
			if delta == nil {
				return
			}
			switch delta["type"] {
			case "thinking_delta":
				if t, ok := delta["thinking"].(string); ok {
					reasoningBuf += t
				}
			case "text_delta":
				if t, ok := delta["text"].(string); ok && t != "" {
					sendReasoning()
					startContent()
					sendEvent("response.output_text.delta", map[string]interface{}{
						"output_index": outputIndex, "content_index": 0, "delta": t,
					})
					contentBuf += t
				}
			case "input_json_delta":
				if p, ok := delta["partial_json"].(string); ok {
					curToolArgs += p
				}
			}
		case "content_block_stop":
			if curToolID != "" {
				flushToolCall()
			}
		case "message_delta":
			if u, ok := payload["usage"].(map[string]interface{}); ok {
				outT, _ := u["output_tokens"].(float64)
				if lastUsage == nil {
					lastUsage = &MimoUsage{}
				}
				lastUsage.CompletionTokens = int(outT)
				lastUsage.TotalTokens = lastUsage.PromptTokens + lastUsage.CompletionTokens
			}
		}
	}

	// ── SSE 解析主循环 ──
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	var curEventType string
	var curData strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			if curEventType != "" && curData.Len() > 0 {
				var payload map[string]interface{}
				if json.Unmarshal([]byte(curData.String()), &payload) == nil {
					processSSEEvent(curEventType, payload)
				}
			}
			curEventType = line[7:]
			curData.Reset()
		} else if strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "data: ") {
			dataStr := line
			if strings.HasPrefix(dataStr, "data: ") {
				dataStr = dataStr[6:]
			} else {
				dataStr = dataStr[5:]
			}
			if curEventType != "" {
				// 标准 SSE 模式：累积 data
				curData.WriteString(dataStr)
			} else {
				// MiMo 非标准模式：data 行本身是完整 JSON
				var fullResp map[string]interface{}
				if json.Unmarshal([]byte(dataStr), &fullResp) == nil {
					if _, hasContent := fullResp["content"]; hasContent {
						log.Printf("[Anthropic] Non-SSE mode: full response in single data line")
						handleFullResponse(fullResp)
						goto streamDone
					}
				}
				curData.WriteString(dataStr)
			}
		} else if line == "" {
			if curEventType != "" && curData.Len() > 0 {
				var payload map[string]interface{}
				if json.Unmarshal([]byte(curData.String()), &payload) == nil {
					processSSEEvent(curEventType, payload)
				}
			}
			curEventType = ""
			curData.Reset()
		}
	}

streamDone:
	flushToolCall()
	sendReasoning()

	// scanner.Err() 非 nil 说明流被 idle-timeout 强制关闭或其他 IO 错误，
	// 响应已截断，返回 failed 而非 completed。
	if err := scanner.Err(); err != nil {
		log.Printf("[Anthropic] Stream scanner error (truncated): %v", err)
		errResp := chatError(http.StatusBadGateway, "upstream_stream_error", err.Error())
		sendEvent("response.completed", map[string]interface{}{"response": streamErrorResponse(streamID, chatReq.Model, &errResp)})
		return
	}

	if contentStarted {
		sendEvent("response.output_text.done", map[string]interface{}{"output_index": outputIndex, "content_index": 0})
		sendEvent("response.content_part.done", map[string]interface{}{"output_index": outputIndex, "content_index": 0, "part": map[string]string{"type": "output_text"}})
		sendEvent("response.output_item.done", map[string]interface{}{"output_index": outputIndex, "item": map[string]interface{}{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text"}}}})
		outputIndex++
	}

	output := []map[string]interface{}{}
	if reasoningSent {
		output = append(output, map[string]interface{}{"type": "reasoning", "summary": []map[string]string{{"type": "summary_text", "text": summarizeReasoning(reasoningBuf)}}})
	}
	output = append(output, toolCallsBuf...)
	if contentBuf != "" {
		output = append(output, map[string]interface{}{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": contentBuf}}})
	}

	log.Printf("[Anthropic] Stream done: reasoning=%d content=%d tools=%d", len(reasoningBuf), len(contentBuf), len(toolCallsBuf))

	assistantMessage := buildStoredAssistantMessage(contentBuf, reasoningBuf, toolCallsFromResponseItems(toolCallsBuf))
	result := makeResponse(streamID, "completed", chatReq.Model, output, convertUsage(lastUsage))
	envelope := finalizeResponseEnvelope(result, chatReq, assistantMessage)
	sendEvent("response.completed", map[string]interface{}{"response": envelope.ResponseObject})
}

// ── 构建请求 ────────────────────────────────────────────────────────────────

func buildAnthropicRequest(chatReq ChatRequest) map[string]interface{} {
	// 转换消息格式 — 核心修复
	anthroMsgs := convertOpenAIMessagesToAnthropic(chatReq.Messages)

	// 提取 system 指令，多条 system 消息用 "\n\n" 合并
	var systemParts []string
	for _, m := range chatReq.Messages {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok && s != "" {
				systemParts = append(systemParts, s)
			}
		}
	}
	systemText := strings.Join(systemParts, "\n\n")

	// 转换工具
	var tools []interface{}
	if rawJSONPresent(chatReq.Tools) {
		tools = convertToolsToAnthropicFormat(chatReq.Tools)
	}

	// ── max_tokens 修正 ──
	// Anthropic Messages API 的 max_tokens 是整个输出上限（含 thinking），
	// 不是仅指 completion tokens。传 32768 给 MiMo 上游太保守，thinking 吃掉
	// 大头后留给 text 的不够，导致模型截断乱答。
	// 对于 MiMo 的 Anthropic 端点，给一个充裕的上限。
	maxTokens := chatReq.MaxCompletionTokens
	if maxTokens <= 0 || maxTokens < 16384 {
		maxTokens = 65536
	}

	req := map[string]interface{}{
		"model":      chatReq.Model,
		"max_tokens": maxTokens,
		"messages":   anthroMsgs,
	}
	if systemText != "" {
		req["system"] = systemText
	}
	if len(tools) > 0 {
		req["tools"] = tools
		// tool_choice: 仅在有工具时传递，映射 OpenAI→Anthropic 格式
		if rawJSONPresent(chatReq.ToolChoice) {
			var tc interface{}
			if json.Unmarshal(chatReq.ToolChoice, &tc) == nil {
				switch v := tc.(type) {
				case string:
					switch v {
					case "auto":
						req["tool_choice"] = map[string]interface{}{"type": "auto"}
					case "required":
						req["tool_choice"] = map[string]interface{}{"type": "any"}
					// "none" → 省略
					}
				case map[string]interface{}:
					if stringValue(v["type"]) == "function" {
						name := stringValue(v["name"])
						if name == "" {
							if fn, ok := v["function"].(map[string]interface{}); ok {
								name = stringValue(fn["name"])
							}
						}
						if name != "" {
							req["tool_choice"] = map[string]interface{}{"type": "tool", "name": name}
						}
					}
				}
			}
		}
	}
	// 传递 thinking 配置 — 仅在没有工具调用历史时传递。
	// 当历史中存在 tool_result 时，前一轮的 thinking 块已被丢弃（无签名无法回放），
	// 此时传 thinking=true 会让 MiMo 进入不一致状态并 hang 住。
	hasToolHistory := false
	for _, m := range chatReq.Messages {
		if m.Role == "tool" {
			hasToolHistory = true
			break
		}
	}
	if rawJSONPresent(chatReq.Thinking) && !hasToolHistory {
		req["thinking"] = json.RawMessage(chatReq.Thinking)
	}
	if chatReq.Temperature != nil {
		req["temperature"] = *chatReq.Temperature
	}
	if chatReq.TopP != nil {
		req["top_p"] = *chatReq.TopP
	}

	log.Printf("[Anthropic] buildRequest: model=%s tools=%d msgs_in=%d msgs_out=%d max_tokens=%d system_len=%d thinking=%v tool_history=%v",
		chatReq.Model, len(tools), len(chatReq.Messages), len(anthroMsgs), maxTokens, len(systemText), rawJSONPresent(chatReq.Thinking), hasToolHistory)

	return req
}

// ── 发送 Anthropic 请求 ──────────────────────────────────────────────────────

func setAnthropicHeaders(req *http.Request, inbound *http.Request) error {
	req.Header.Set("Content-Type", "application/json")
	key := resolveMimoKey(inbound)
	if key == "" {
		return fmt.Errorf("missing upstream API key")
	}
	req.Header.Set("api-key", key)
	req.Header.Set("anthropic-version", anthropicVersion)
	return nil
}

func executeUpstreamAnthropic(inbound *http.Request, anthroReq map[string]interface{}) parsedChatResp {
	body, _ := json.Marshal(anthroReq)
	url := anthropicUpstreamURL()

	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	if err := setAnthropicHeaders(req, inbound); err != nil {
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
		msg := string(respBody)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		log.Printf("[Anthropic] error %d: %s", resp.StatusCode, msg)
		return chatError(resp.StatusCode, "upstream_error", msg)
	}

	// 解析 Anthropic Messages 响应
	var anthroResp struct {
		ID         string `json:"id"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string      `json:"type"`
			Text     string      `json:"text"`
			Thinking string      `json:"thinking"`
			ID       string      `json:"id"`
			Name     string      `json:"name"`
			Input    interface{} `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &anthroResp); err != nil {
		return chatError(http.StatusBadGateway, "upstream_parse_failed", err.Error())
	}

	out := parsedChatResp{
		ID:    anthroResp.ID,
		Usage: &MimoUsage{PromptTokens: anthroResp.Usage.InputTokens, CompletionTokens: anthroResp.Usage.OutputTokens, TotalTokens: anthroResp.Usage.InputTokens + anthroResp.Usage.OutputTokens},
	}

	for _, block := range anthroResp.Content {
		switch block.Type {
		case "text":
			out.Content += block.Text
		case "thinking":
			out.ReasoningContent += block.Thinking
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			out.ToolCalls = append(out.ToolCalls, parsedToolCall{
				ID: block.ID, Name: block.Name, Arguments: string(args),
			})
		}
	}

	switch anthroResp.StopReason {
	case "tool_use":
		out.FinishReason = "tool_calls"
	case "end_turn", "stop_sequence":
		out.FinishReason = "stop"
	case "max_tokens":
		out.FinishReason = "length"
	default:
		out.FinishReason = anthroResp.StopReason
	}

	log.Printf("[Anthropic] response: stop=%s text=%d thinking=%d tools=%d input_tokens=%d",
		out.FinishReason, len(out.Content), len(out.ReasoningContent), len(out.ToolCalls), anthroResp.Usage.InputTokens)
	return out
}
