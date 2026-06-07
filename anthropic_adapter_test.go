package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// helper: 从 interface{} 断言出 map[string]interface{}
func mustMap(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	m, ok := v.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map[string]interface{}, got %T", v)
	}
	return m
}

// ═══════════════════════════════════════════════════════════════════════════
// convertOpenAIMessagesToAnthropic 测试
// ═══════════════════════════════════════════════════════════════════════════

// ── 基础场景 ──

func TestConvert_SimpleUserAssistant(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	result := convertOpenAIMessagesToAnthropic(msgs)

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0]["role"] != "user" {
		t.Errorf("msg[0] role = %v, want user", result[0]["role"])
	}
	if result[0]["content"] != "hello" {
		t.Errorf("msg[0] content = %v, want hello", result[0]["content"])
	}
	if result[1]["role"] != "assistant" {
		t.Errorf("msg[1] role = %v, want assistant", result[1]["role"])
	}
	if result[1]["content"] != "hi there" {
		t.Errorf("msg[1] content = %v, want 'hi there'", result[1]["content"])
	}
}

func TestConvert_SystemMessagesSkipped(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hello"},
	}
	result := convertOpenAIMessagesToAnthropic(msgs)

	if len(result) != 1 {
		t.Fatalf("expected 1 message (system skipped), got %d", len(result))
	}
	if result[0]["role"] != "user" {
		t.Errorf("msg[0] role = %v, want user", result[0]["role"])
	}
}

// ── Tool call 转换：这是"上下文串了"的核心 bug 所在 ──

func TestConvert_AssistantToolCalls(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run ls"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_001",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "exec_command",
					"arguments": `{"cmd":"ls -la"}`,
				},
			},
		}},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}

	assistant := result[1]
	if assistant["role"] != "assistant" {
		t.Fatalf("msg[1] role = %v, want assistant", assistant["role"])
	}

	content, ok := assistant["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("msg[1] content should be []map[string]interface{}, got %T", assistant["content"])
	}
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}

	block := content[0]
	if block["type"] != "tool_use" {
		t.Errorf("block type = %v, want tool_use", block["type"])
	}
	if block["id"] != "call_001" {
		t.Errorf("block id = %v, want call_001", block["id"])
	}
	if block["name"] != "exec_command" {
		t.Errorf("block name = %v, want exec_command", block["name"])
	}

	inputMap, ok := block["input"].(map[string]interface{})
	if !ok {
		t.Fatalf("block input should be map, got %T", block["input"])
	}
	if inputMap["cmd"] != "ls -la" {
		t.Errorf("block input.cmd = %v, want 'ls -la'", inputMap["cmd"])
	}
}

func TestConvert_ToolResult(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run ls"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_001",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "exec_command",
					"arguments": `{"cmd":"ls"}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_001", Content: "file1.txt\nfile2.txt"},
		{Role: "user", Content: "open file1"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// Phase-2 merge: tool_result and the following user text coalesce into ONE user
	// message → 3 messages: user("run ls"), assistant(tool_use), user([tool_result, text])
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(result), result)
	}

	// result[2] must be user with [tool_result, text] blocks
	userMsg := result[2]
	if userMsg["role"] != "user" {
		t.Fatalf("result[2] role = %v, want user", userMsg["role"])
	}

	blocks, ok := userMsg["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("result[2] content should be []map[string]interface{}, got %T", userMsg["content"])
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (tool_result + text), got %d", len(blocks))
	}

	// First block: tool_result
	tr := blocks[0]
	if tr["type"] != "tool_result" {
		t.Errorf("blocks[0] type = %v, want tool_result", tr["type"])
	}
	if tr["tool_use_id"] != "call_001" {
		t.Errorf("blocks[0] tool_use_id = %v, want call_001", tr["tool_use_id"])
	}
	if tr["content"] != "file1.txt\nfile2.txt" {
		t.Errorf("blocks[0] content = %v, want file output", tr["content"])
	}

	// Second block: text "open file1"
	txt := blocks[1]
	if txt["type"] != "text" {
		t.Errorf("blocks[1] type = %v, want text", txt["type"])
	}
	if txt["text"] != "open file1" {
		t.Errorf("blocks[1] text = %v, want 'open file1'", txt["text"])
	}
}

func TestConvert_MultipleToolResultsGrouped(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run both"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_1",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "exec",
					"arguments": `{"cmd":"a"}`,
				},
			},
			map[string]interface{}{
				"id":   "call_2",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "exec",
					"arguments": `{"cmd":"b"}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "output_a"},
		{Role: "tool", ToolCallID: "call_2", Content: "output_b"},
		{Role: "user", Content: "now what?"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// Phase-2 merge: the two tool_results and the following user text coalesce into ONE
	// user message → 3 messages: user("run both"), assistant(2 tool_use), user([tr1,tr2,text])
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}

	userMsg := result[2]
	if userMsg["role"] != "user" {
		t.Fatalf("result[2] role = %v, want user", userMsg["role"])
	}
	blocks, ok := userMsg["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("result[2] content type = %T", userMsg["content"])
	}
	// [tool_result call_1, tool_result call_2, text "now what?"]
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks (2 tool_results + text), got %d", len(blocks))
	}
	if blocks[0]["type"] != "tool_result" || blocks[0]["tool_use_id"] != "call_1" {
		t.Errorf("blocks[0] = %v, want tool_result call_1", blocks[0])
	}
	if blocks[1]["type"] != "tool_result" || blocks[1]["tool_use_id"] != "call_2" {
		t.Errorf("blocks[1] = %v, want tool_result call_2", blocks[1])
	}
	if blocks[2]["type"] != "text" || blocks[2]["text"] != "now what?" {
		t.Errorf("blocks[2] = %v, want text 'now what?'", blocks[2])
	}
}

func TestConvert_ToolResultAtEnd(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_x",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "run",
					"arguments": `{}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_x", Content: "done"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// 期望: user, assistant(tool_use), user(tool_result flush)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (flush at end), got %d", len(result))
	}

	lastMsg := result[2]
	if lastMsg["role"] != "user" {
		t.Errorf("last msg role = %v, want user (flushed tool_result)", lastMsg["role"])
	}
	blocks, ok := lastMsg["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("last msg content type = %T", lastMsg["content"])
	}
	if len(blocks) != 1 || blocks[0]["type"] != "tool_result" {
		t.Errorf("expected 1 tool_result block, got %+v", blocks)
	}
}

// ── 多轮完整场景：模拟 Codex 的典型对话 ──

func TestConvert_FullCodexMultiTurn(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "List files"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_1",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "exec_command",
					"arguments": `{"cmd":"ls"}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "file1.go\nfile2.go"},
		{Role: "assistant", Content: "I found 2 files."},
		{Role: "user", Content: "Show file1"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_2",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "exec_command",
					"arguments": `{"cmd":"cat file1.go"}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_2", Content: "package main\nfunc main() {}"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// 期望结构:
	// 0: user("List files")
	// 1: assistant(tool_use call_1)
	// 2: user(tool_result call_1)
	// 3: assistant("I found 2 files.")
	// 4: user("Show file1")
	// 5: assistant(tool_use call_2)
	// 6: user(tool_result call_2)
	if len(result) != 7 {
		t.Fatalf("expected 7 messages, got %d", len(result))
	}

	expectedRoles := []string{"user", "assistant", "user", "assistant", "user", "assistant", "user"}
	for i, m := range result {
		r, _ := m["role"].(string)
		if r != expectedRoles[i] {
			t.Errorf("msg[%d] role = %q, want %q", i, r, expectedRoles[i])
		}
	}

	// 验证 tool_result 在 user 消息里
	msg2, _ := result[2]["content"].([]map[string]interface{})
	if len(msg2) != 1 || msg2[0]["type"] != "tool_result" {
		t.Errorf("msg[2] should be tool_result, got %+v", msg2)
	}

	// 验证纯文本 assistant 消息
	if result[3]["content"] != "I found 2 files." {
		t.Errorf("msg[3] content = %v, want 'I found 2 files.'", result[3]["content"])
	}
}

// ── Assistant 带 text + tool_calls ──

func TestConvert_AssistantTextAndToolCalls(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run it"},
		{Role: "assistant", Content: "I'll run that for you.", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_t1",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "exec",
					"arguments": `{"cmd":"echo hi"}`,
				},
			},
		}},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}

	assistant := result[1]
	blocks, ok := assistant["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("assistant content type = %T", assistant["content"])
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks (text + tool_use), got %d", len(blocks))
	}

	if blocks[0]["type"] != "text" {
		t.Errorf("block[0] type = %v, want text", blocks[0]["type"])
	}
	if blocks[0]["text"] != "I'll run that for you." {
		t.Errorf("block[0] text = %v", blocks[0]["text"])
	}
	if blocks[1]["type"] != "tool_use" {
		t.Errorf("block[1] type = %v, want tool_use", blocks[1]["type"])
	}
}

// ── 空 tool arguments ──

func TestConvert_EmptyToolArguments(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_e1",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "noop",
					"arguments": "",
				},
			},
		}},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)
	assistant := result[1]
	blocks, _ := assistant["content"].([]map[string]interface{})

	input, ok := blocks[0]["input"].(map[string]interface{})
	if !ok {
		t.Fatalf("input type = %T", blocks[0]["input"])
	}
	if len(input) != 0 {
		t.Errorf("expected empty object for empty arguments, got %v", input)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// convertToolsToAnthropicFormat 测试
// ═══════════════════════════════════════════════════════════════════════════

func TestConvertTools_OpenAIFunctionFormat(t *testing.T) {
	tools := json.RawMessage(`[
		{
			"type": "function",
			"function": {
				"name": "exec_command",
				"description": "Run a shell command",
				"parameters": {
					"type": "object",
					"properties": {"cmd": {"type": "string"}},
					"required": ["cmd"]
				}
			}
		}
	]`)

	result := convertToolsToAnthropicFormat(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}

	tool := mustMap(t, result[0])
	if tool["name"] != "exec_command" {
		t.Errorf("name = %v, want exec_command", tool["name"])
	}
	if tool["description"] != "Run a shell command" {
		t.Errorf("description = %v", tool["description"])
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Error("missing input_schema field")
	}
	if _, ok := tool["parameters"]; ok {
		t.Error("should not have 'parameters' field — Anthropic uses 'input_schema'")
	}
}

func TestConvertTools_AlreadyAnthropicFormat(t *testing.T) {
	tools := json.RawMessage(`[
		{
			"name": "exec",
			"description": "Run command",
			"input_schema": {
				"type": "object",
				"properties": {"cmd": {"type": "string"}}
			}
		}
	]`)

	result := convertToolsToAnthropicFormat(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}

	tool := mustMap(t, result[0])
	if tool["name"] != "exec" {
		t.Errorf("name = %v, want exec", tool["name"])
	}
}

func TestConvertTools_MultipleTools(t *testing.T) {
	tools := json.RawMessage(`[
		{
			"type": "function",
			"function": {
				"name": "tool_a",
				"description": "Tool A",
				"parameters": {"type": "object", "properties": {}}
			}
		},
		{
			"type": "function",
			"function": {
				"name": "tool_b",
				"description": "Tool B",
				"parameters": {"type": "object", "properties": {"x": {"type": "integer"}}}
			}
		}
	]`)

	result := convertToolsToAnthropicFormat(tools)
	if len(result) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result))
	}

	ta := mustMap(t, result[0])
	tb := mustMap(t, result[1])
	if ta["name"] != "tool_a" || tb["name"] != "tool_b" {
		t.Errorf("names = %v, %v; want tool_a, tool_b", ta["name"], tb["name"])
	}
}

func TestConvertTools_EmptyInput(t *testing.T) {
	if result := convertToolsToAnthropicFormat(nil); result != nil {
		t.Errorf("expected nil for nil input, got %v", result)
	}
	if result := convertToolsToAnthropicFormat(json.RawMessage("")); result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestConvertTools_SkipsNameless(t *testing.T) {
	tools := json.RawMessage(`[
		{"type": "function", "function": {"name": "", "description": "no name"}},
		{"type": "function", "function": {"name": "real_tool", "description": "has name"}}
	]`)

	result := convertToolsToAnthropicFormat(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool (nameless skipped), got %d", len(result))
	}
	tool := mustMap(t, result[0])
	if tool["name"] != "real_tool" {
		t.Errorf("name = %v, want real_tool", tool["name"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// buildAnthropicRequest 测试 — max_tokens 和 thinking
// ═══════════════════════════════════════════════════════════════════════════

func TestBuildRequest_MaxTokensFloor(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 0,
	}
	req := buildAnthropicRequest(chatReq)

	mt, ok := req["max_tokens"].(int)
	if !ok {
		t.Fatalf("max_tokens type = %T", req["max_tokens"])
	}
	if mt != 65536 {
		t.Errorf("max_tokens = %d, want 65536 (default for Anthropic)", mt)
	}
}

func TestBuildRequest_MaxTokensBelowFloor(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 10000,
	}
	req := buildAnthropicRequest(chatReq)

	mt, ok := req["max_tokens"].(int)
	if !ok {
		t.Fatalf("max_tokens type = %T", req["max_tokens"])
	}
	if mt != 65536 {
		t.Errorf("max_tokens = %d, want 65536 (below floor)", mt)
	}
}

func TestBuildRequest_MaxTokensAboveFloor(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 20000,
	}
	req := buildAnthropicRequest(chatReq)

	mt, ok := req["max_tokens"].(int)
	if !ok {
		t.Fatalf("max_tokens type = %T", req["max_tokens"])
	}
	if mt != 20000 {
		t.Errorf("max_tokens = %d, want 20000 (above floor, kept)", mt)
	}
}

func TestBuildRequest_ThinkingPassed(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "think"}},
		MaxCompletionTokens: 20000,
		Thinking:            json.RawMessage(`{"type":"enabled","budget_tokens":10000}`),
	}
	req := buildAnthropicRequest(chatReq)

	thinking, exists := req["thinking"]
	if !exists {
		t.Fatal("thinking field not present in request")
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(thinking.(json.RawMessage), &parsed); err != nil {
		t.Fatalf("failed to parse thinking: %v", err)
	}
	if parsed["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", parsed["type"])
	}
	if budget, ok := parsed["budget_tokens"].(float64); !ok || int(budget) != 10000 {
		t.Errorf("thinking.budget_tokens = %v, want 10000", parsed["budget_tokens"])
	}
}

func TestBuildRequest_ThinkingNotPassed(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 20000,
	}
	req := buildAnthropicRequest(chatReq)

	if _, exists := req["thinking"]; exists {
		t.Error("thinking should not be present when not set")
	}
}

func TestBuildRequest_SystemExtracted(t *testing.T) {
	chatReq := ChatRequest{
		Model: "mimo-v2.5-pro",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are a coding assistant."},
			{Role: "user", Content: "write code"},
		},
		MaxCompletionTokens: 20000,
	}
	req := buildAnthropicRequest(chatReq)

	sys, ok := req["system"].(string)
	if !ok {
		t.Fatal("system field not present or not string")
	}
	if sys != "You are a coding assistant." {
		t.Errorf("system = %q, want 'You are a coding assistant.'", sys)
	}

	msgs, ok := req["messages"].([]map[string]interface{})
	if !ok {
		t.Fatalf("messages type = %T", req["messages"])
	}
	for _, m := range msgs {
		if m["role"] == "system" {
			t.Error("system message should be extracted out of messages array")
		}
	}
}

func TestBuildRequest_ToolsConverted(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "run"}},
		MaxCompletionTokens: 20000,
		Tools:               json.RawMessage(`[{"type":"function","function":{"name":"exec","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]`),
	}
	req := buildAnthropicRequest(chatReq)

	tools, exists := req["tools"]
	if !exists {
		t.Fatal("tools field missing")
	}
	toolSlice, ok := tools.([]interface{})
	if !ok {
		t.Fatalf("tools type = %T", tools)
	}
	if len(toolSlice) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(toolSlice))
	}

	tool := mustMap(t, toolSlice[0])
	if tool["name"] != "exec" {
		t.Errorf("tool name = %v, want exec", tool["name"])
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Error("tool missing input_schema (Anthropic format)")
	}
}

func TestBuildRequest_TemperatureAndTopP(t *testing.T) {
	temp := 0.7
	topP := 0.9
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 20000,
		Temperature:         &temp,
		TopP:                &topP,
	}
	req := buildAnthropicRequest(chatReq)

	if req["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want 0.7", req["temperature"])
	}
	if req["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want 0.9", req["top_p"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// 边界场景和回归测试
// ═══════════════════════════════════════════════════════════════════════════

func TestConvert_EmptyMessages(t *testing.T) {
	result := convertOpenAIMessagesToAnthropic(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 messages, got %d", len(result))
	}
}

func TestConvert_ToolResultWithNilContent(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_nil",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "exec",
					"arguments": `{}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_nil", Content: nil},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	lastMsg := result[len(result)-1]
	blocks, _ := lastMsg["content"].([]map[string]interface{})
	if blocks[0]["content"] != "" {
		t.Errorf("nil content should become empty string, got %v", blocks[0]["content"])
	}
}

func TestConvert_AssistantWithNoToolCallsField(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
	if result[1]["content"] != "hello" {
		t.Errorf("content = %v, want hello", result[1]["content"])
	}
}

func TestConvert_LargeMultiTurn72(t *testing.T) {
	var msgs []ChatMessage
	toolID := 0
	for i := 0; i < 72; i++ {
		switch {
		case i%4 == 0:
			msgs = append(msgs, ChatMessage{Role: "user", Content: "do something"})
		case i%4 == 1:
			toolID++
			msgs = append(msgs, ChatMessage{
				Role:    "assistant",
				Content: "",
				ToolCalls: []interface{}{
					map[string]interface{}{
						"id":   fmt.Sprintf("call_%d", toolID),
						"type": "function",
						"function": map[string]interface{}{
							"name":      "exec",
							"arguments": fmt.Sprintf(`{"cmd":"cmd_%d"}`, toolID),
						},
					},
				},
			})
		case i%4 == 2:
			msgs = append(msgs, ChatMessage{
				Role:       "tool",
				ToolCallID: fmt.Sprintf("call_%d", toolID),
				Content:    "ok",
			})
		case i%4 == 3:
			msgs = append(msgs, ChatMessage{
				Role:    "assistant",
				Content: "Done.",
			})
		}
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// 不应该有 role:"tool" 的消息
	for i, m := range result {
		if m["role"] == "tool" {
			t.Errorf("msg[%d] has role 'tool' — should have been converted to tool_result", i)
		}
	}

	// 不应该有残留的 OpenAI 格式字段
	for i, m := range result {
		if _, ok := m["tool_calls"]; ok {
			t.Errorf("msg[%d] still has tool_calls field — should be converted", i)
		}
		if _, ok := m["tool_call_id"]; ok {
			t.Errorf("msg[%d] still has tool_call_id field — should be converted", i)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Anthropic 响应解析验证
// ═══════════════════════════════════════════════════════════════════════════

func TestAnthropicResponseParsing(t *testing.T) {
	anthroResp := `{
		"id": "msg_test123",
		"stop_reason": "tool_use",
		"content": [
			{"type": "thinking", "thinking": "Let me analyze this..."},
			{"type": "text", "text": "I'll run that command."},
			{"type": "tool_use", "id": "toolu_abc", "name": "exec_command", "input": {"cmd": "ls -la"}}
		],
		"usage": {"input_tokens": 1500, "output_tokens": 200}
	}`

	var parsed struct {
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

	if err := json.Unmarshal([]byte(anthroResp), &parsed); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if parsed.ID != "msg_test123" {
		t.Errorf("id = %v", parsed.ID)
	}
	if parsed.StopReason != "tool_use" {
		t.Errorf("stop_reason = %v", parsed.StopReason)
	}
	if len(parsed.Content) != 3 {
		t.Fatalf("expected 3 content blocks, got %d", len(parsed.Content))
	}
	if parsed.Content[0].Thinking != "Let me analyze this..." {
		t.Errorf("thinking = %v", parsed.Content[0].Thinking)
	}
	if parsed.Content[1].Text != "I'll run that command." {
		t.Errorf("text = %v", parsed.Content[1].Text)
	}
	if parsed.Content[2].Name != "exec_command" {
		t.Errorf("tool name = %v", parsed.Content[2].Name)
	}
	if parsed.Usage.InputTokens != 1500 {
		t.Errorf("input_tokens = %d", parsed.Usage.InputTokens)
	}
}

func TestStopReasonMapping(t *testing.T) {
	tests := []struct {
		anthropic string
		openai    string
	}{
		{"tool_use", "tool_calls"},
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
	}

	for _, tc := range tests {
		var result string
		switch tc.anthropic {
		case "tool_use":
			result = "tool_calls"
		case "end_turn", "stop_sequence":
			result = "stop"
		case "max_tokens":
			result = "length"
		default:
			result = tc.anthropic
		}
		if result != tc.openai {
			t.Errorf("stop_reason %q → %q, want %q", tc.anthropic, result, tc.openai)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// 端到端序列化验证：buildAnthropicRequest 的完整 JSON 输出
// ═══════════════════════════════════════════════════════════════════════════

func TestBuildRequest_FullJSONOutput(t *testing.T) {
	temp := 0.5
	chatReq := ChatRequest{
		Model: "mimo-v2.5-pro",
		Messages: []ChatMessage{
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Content: "run ls"},
			{Role: "assistant", Content: "", ToolCalls: []interface{}{
				map[string]interface{}{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "exec",
						"arguments": `{"cmd":"ls"}`,
					},
				},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "a.go\nb.go"},
			{Role: "assistant", Content: "2 files found."},
			{Role: "user", Content: "show a.go"},
		},
		MaxCompletionTokens: 20000,
		Temperature:         &temp,
		Thinking:            json.RawMessage(`{"type":"enabled","budget_tokens":5000}`),
		Tools: json.RawMessage(`[
			{"type":"function","function":{"name":"exec","description":"Run command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}}
		]`),
	}

	req := buildAnthropicRequest(chatReq)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	// 验证 JSON 结构完整
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// model
	if parsed["model"] != "mimo-v2.5-pro" {
		t.Errorf("model = %v", parsed["model"])
	}

	// max_tokens
	if mt, _ := parsed["max_tokens"].(float64); int(mt) != 20000 {
		t.Errorf("max_tokens = %v", parsed["max_tokens"])
	}

	// system
	if parsed["system"] != "Be helpful." {
		t.Errorf("system = %v", parsed["system"])
	}

	// messages — 不应有 system，不应有 role:"tool"
	msgs, _ := parsed["messages"].([]interface{})
	for i, m := range msgs {
		msg, _ := m.(map[string]interface{})
		role, _ := msg["role"].(string)
		if role == "system" {
			t.Errorf("msgs[%d] is system — should be extracted", i)
		}
		if role == "tool" {
			t.Errorf("msgs[%d] is tool — should be converted to tool_result", i)
		}
	}

	// tools — 应该用 input_schema
	tools, _ := parsed["tools"].([]interface{})
	for i, tl := range tools {
		tool, _ := tl.(map[string]interface{})
		if _, ok := tool["input_schema"]; !ok {
			t.Errorf("tool[%d] missing input_schema", i)
		}
		if _, ok := tool["parameters"]; ok {
			t.Errorf("tool[%d] has parameters — should be input_schema", i)
		}
	}

	// thinking — messages contain a role:tool entry, so thinking must be OMITTED
	// (replaying thinking blocks without signatures hangs MiMo)
	if _, ok := parsed["thinking"]; ok {
		t.Error("thinking should be omitted when messages contain tool history (role:tool present)")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// Regression tests for the production-type tool call bug
// ═══════════════════════════════════════════════════════════════════════════

// TestConvert_ParallelToolCallsMerged replicates the exact /goal hang shape:
// two consecutive assistant messages each with a single tool_call (as produced by
// Codex's parallel exec batches) must merge into ONE assistant message with TWO
// tool_use blocks, and the following tool messages must merge into ONE user message.
// Uses production types: []map[string]interface{} with function: map[string]string.
func TestConvert_ParallelToolCallsMerged(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run both"},
		// Parallel batch: two consecutive assistant messages, each with one tool_call
		{Role: "assistant", Content: "", ToolCalls: []map[string]interface{}{
			{
				"id": "call_P1", "type": "function",
				"function": map[string]string{"name": "exec", "arguments": `{"cmd":"ls"}`},
			},
		}},
		{Role: "assistant", Content: "", ToolCalls: []map[string]interface{}{
			{
				"id": "call_P2", "type": "function",
				"function": map[string]string{"name": "exec", "arguments": `{"cmd":"pwd"}`},
			},
		}},
		// Results for both
		{Role: "tool", ToolCallID: "call_P1", Content: "file1.go"},
		{Role: "tool", ToolCallID: "call_P2", Content: "/home/user"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// Must produce exactly 3 messages: user, assistant(2 tool_use), user(2 tool_result)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (parallel calls merged), got %d: %+v", len(result), result)
	}

	// Strict role alternation: no two adjacent messages may share a role
	for i := 1; i < len(result); i++ {
		prevRole, _ := result[i-1]["role"].(string)
		curRole, _ := result[i]["role"].(string)
		if prevRole == curRole {
			t.Errorf("adjacent messages [%d] and [%d] both have role %q (not alternating)", i-1, i, curRole)
		}
	}

	// result[1] must be assistant with exactly 2 tool_use blocks
	asst := result[1]
	if asst["role"] != "assistant" {
		t.Fatalf("result[1] role = %v, want assistant", asst["role"])
	}
	aBlocks, ok := asst["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("result[1] content type = %T, want []map[string]interface{}", asst["content"])
	}
	if len(aBlocks) != 2 {
		t.Fatalf("expected 2 tool_use blocks in assistant message, got %d", len(aBlocks))
	}
	toolUseIDs := map[string]bool{}
	for _, b := range aBlocks {
		if b["type"] != "tool_use" {
			t.Errorf("assistant block type = %v, want tool_use", b["type"])
		}
		id, _ := b["id"].(string)
		toolUseIDs[id] = true
	}
	if !toolUseIDs["call_P1"] || !toolUseIDs["call_P2"] {
		t.Errorf("assistant tool_use ids = %v, want {call_P1, call_P2}", toolUseIDs)
	}

	// result[2] must be user with 2 tool_result blocks, each matching a tool_use id
	userMsg := result[2]
	if userMsg["role"] != "user" {
		t.Fatalf("result[2] role = %v, want user", userMsg["role"])
	}
	uBlocks, ok := userMsg["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("result[2] content type = %T, want []map[string]interface{}", userMsg["content"])
	}
	if len(uBlocks) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %d", len(uBlocks))
	}
	for _, b := range uBlocks {
		if b["type"] != "tool_result" {
			t.Errorf("user block type = %v, want tool_result", b["type"])
		}
		uid, _ := b["tool_use_id"].(string)
		if !toolUseIDs[uid] {
			t.Errorf("tool_result tool_use_id=%q has no matching tool_use in preceding assistant message", uid)
		}
	}
}

// TestConvert_ProductionToolCallType tests the PRODUCTION types used by parseInput
// and toolCallsFromResponseItems: []map[string]interface{} with function: map[string]string.
// This is the test that caught the original bug (type assertion to []interface{} failing,
// tool_use block dropped, orphan tool_result).
func TestConvert_ProductionToolCallType(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run ls"},
		{Role: "assistant", Content: "", ToolCalls: []map[string]interface{}{
			{
				"id":   "call_001",
				"type": "function",
				"function": map[string]string{
					"name":      "exec",
					"arguments": `{"cmd":"ls"}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_001", Content: "out"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(result), result)
	}

	// msg[1] must be assistant with tool_use block
	assistant := result[1]
	if assistant["role"] != "assistant" {
		t.Fatalf("msg[1] role = %v, want assistant", assistant["role"])
	}
	content, ok := assistant["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("msg[1] content should be []map[string]interface{}, got %T", assistant["content"])
	}
	if len(content) != 1 {
		t.Fatalf("expected 1 content block in msg[1], got %d", len(content))
	}
	block := content[0]
	if block["type"] != "tool_use" {
		t.Errorf("block type = %v, want tool_use", block["type"])
	}
	if block["id"] != "call_001" {
		t.Errorf("block id = %v, want call_001", block["id"])
	}
	if block["name"] != "exec" {
		t.Errorf("block name = %v, want exec", block["name"])
	}

	// msg[2] must be user with tool_result whose tool_use_id matches call_001
	toolResultMsg := result[2]
	if toolResultMsg["role"] != "user" {
		t.Fatalf("msg[2] role = %v, want user", toolResultMsg["role"])
	}
	blocks, ok := toolResultMsg["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("msg[2] content type = %T", toolResultMsg["content"])
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 tool_result block, got %d", len(blocks))
	}
	if blocks[0]["type"] != "tool_result" {
		t.Errorf("blocks[0] type = %v, want tool_result", blocks[0]["type"])
	}
	if blocks[0]["tool_use_id"] != "call_001" {
		t.Errorf("blocks[0] tool_use_id = %v, want call_001", blocks[0]["tool_use_id"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// FIX 2: tool msg 缺 ToolCallID 时被跳过
// ═══════════════════════════════════════════════════════════════════════════

func TestConvert_ToolMsgEmptyToolCallIDDropped(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "run"},
		{Role: "assistant", Content: "", ToolCalls: []interface{}{
			map[string]interface{}{
				"id":   "call_ok",
				"type": "function",
				"function": map[string]interface{}{"name": "exec", "arguments": `{}`},
			},
		}},
		// 没有 ToolCallID 的 tool 消息应被跳过
		{Role: "tool", ToolCallID: "", Content: "output"},
		{Role: "tool", ToolCallID: "call_ok", Content: "good output"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// 只有 call_ok 那条 tool_result 进入结果，空 ID 那条被丢弃
	// 期望: user, assistant(tool_use), user(tool_result call_ok)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(result), result)
	}

	userMsg := result[2]
	blocks, ok := userMsg["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("result[2] content type = %T", userMsg["content"])
	}
	for _, b := range blocks {
		if uid, _ := b["tool_use_id"].(string); uid == "" {
			t.Errorf("tool_result with empty tool_use_id slipped through: %+v", b)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// FIX 5: tool_choice 传递（OpenAI→Anthropic 格式映射）
// ═══════════════════════════════════════════════════════════════════════════

func TestBuildRequest_ToolChoiceAuto(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "go"}},
		MaxCompletionTokens: 20000,
		Tools:               json.RawMessage(`[{"type":"function","function":{"name":"exec","description":"run","parameters":{"type":"object","properties":{}}}}]`),
		ToolChoice:          json.RawMessage(`"auto"`),
	}
	req := buildAnthropicRequest(chatReq)

	tc, ok := req["tool_choice"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool_choice type = %T, want map", req["tool_choice"])
	}
	if tc["type"] != "auto" {
		t.Errorf("tool_choice.type = %v, want auto", tc["type"])
	}
}

func TestBuildRequest_ToolChoiceRequired(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "go"}},
		MaxCompletionTokens: 20000,
		Tools:               json.RawMessage(`[{"type":"function","function":{"name":"exec","description":"run","parameters":{"type":"object","properties":{}}}}]`),
		ToolChoice:          json.RawMessage(`"required"`),
	}
	req := buildAnthropicRequest(chatReq)

	tc, ok := req["tool_choice"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool_choice type = %T, want map", req["tool_choice"])
	}
	if tc["type"] != "any" {
		t.Errorf("tool_choice.type = %v, want any (Anthropic 'required')", tc["type"])
	}
}

func TestBuildRequest_ToolChoiceNoneOmitted(t *testing.T) {
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "go"}},
		MaxCompletionTokens: 20000,
		Tools:               json.RawMessage(`[{"type":"function","function":{"name":"exec","description":"run","parameters":{"type":"object","properties":{}}}}]`),
		ToolChoice:          json.RawMessage(`"none"`),
	}
	req := buildAnthropicRequest(chatReq)

	if _, ok := req["tool_choice"]; ok {
		t.Errorf("tool_choice should be absent for 'none', got %v", req["tool_choice"])
	}
}

func TestBuildRequest_ToolChoiceFunction(t *testing.T) {
	// {"type":"function","function":{"name":"exec"}} 形态
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "go"}},
		MaxCompletionTokens: 20000,
		Tools:               json.RawMessage(`[{"type":"function","function":{"name":"exec","description":"run","parameters":{"type":"object","properties":{}}}}]`),
		ToolChoice:          json.RawMessage(`{"type":"function","function":{"name":"exec"}}`),
	}
	req := buildAnthropicRequest(chatReq)

	tc, ok := req["tool_choice"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool_choice type = %T, want map", req["tool_choice"])
	}
	if tc["type"] != "tool" {
		t.Errorf("tool_choice.type = %v, want tool", tc["type"])
	}
	if tc["name"] != "exec" {
		t.Errorf("tool_choice.name = %v, want exec", tc["name"])
	}
}

func TestBuildRequest_ToolChoiceAbsentWhenNoTools(t *testing.T) {
	// 没有工具时，不应传 tool_choice（无论 ToolChoice 是什么）
	chatReq := ChatRequest{
		Model:               "mimo-v2.5-pro",
		Messages:            []ChatMessage{{Role: "user", Content: "go"}},
		MaxCompletionTokens: 20000,
		ToolChoice:          json.RawMessage(`"auto"`),
	}
	req := buildAnthropicRequest(chatReq)

	if _, ok := req["tool_choice"]; ok {
		t.Errorf("tool_choice should be absent when no tools, got %v", req["tool_choice"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// FIX 6: 多条 system 消息合并
// ═══════════════════════════════════════════════════════════════════════════

func TestBuildRequest_MultipleSystemMessages(t *testing.T) {
	chatReq := ChatRequest{
		Model: "mimo-v2.5-pro",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are helpful."},
			{Role: "system", Content: "Always be concise."},
			{Role: "user", Content: "hi"},
		},
		MaxCompletionTokens: 20000,
	}
	req := buildAnthropicRequest(chatReq)

	sys, ok := req["system"].(string)
	if !ok {
		t.Fatal("system field not present or not string")
	}
	expected := "You are helpful.\n\nAlways be concise."
	if sys != expected {
		t.Errorf("system = %q, want %q", sys, expected)
	}
}

func TestBuildRequest_SingleSystemMessageUnchanged(t *testing.T) {
	chatReq := ChatRequest{
		Model: "mimo-v2.5-pro",
		Messages: []ChatMessage{
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Content: "hi"},
		},
		MaxCompletionTokens: 20000,
	}
	req := buildAnthropicRequest(chatReq)

	sys, ok := req["system"].(string)
	if !ok {
		t.Fatal("system field not present or not string")
	}
	if sys != "Be helpful." {
		t.Errorf("system = %q, want 'Be helpful.'", sys)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// TestConvert_ReasoningOnlyDropped verifies that a reasoning-only assistant message
// (no text content, no tool calls) is dropped entirely — not emitted as an
// empty-content message that Anthropic would reject.
func TestConvert_ReasoningOnlyDropped(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "think"},
		{Role: "assistant", Content: "", ReasoningContent: "some internal reasoning"},
		{Role: "assistant", Content: "Here is the answer."},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// The reasoning-only assistant message must be dropped.
	// We expect: user("think"), assistant("Here is the answer.")
	if len(result) != 2 {
		t.Fatalf("expected 2 messages (reasoning-only dropped), got %d: %+v", len(result), result)
	}
	if result[1]["content"] != "Here is the answer." {
		t.Errorf("msg[1] content = %v, want 'Here is the answer.'", result[1]["content"])
	}
	// No message may have empty string content
	for i, m := range result {
		if c, ok := m["content"].(string); ok && c == "" {
			t.Errorf("msg[%d] has empty string content", i)
		}
	}
}

// TestConvert_NoEmptyContent is an invariant check over a mixed multi-turn transcript:
// no produced message may have empty-string content, and every tool_result.tool_use_id
// must have a matching preceding tool_use with that id.
func TestConvert_NoEmptyContent(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "run ls"},
		// production types: []map[string]interface{} + map[string]string
		{Role: "assistant", Content: "", ToolCalls: []map[string]interface{}{
			{"id": "call_A", "type": "function", "function": map[string]string{"name": "exec", "arguments": `{"cmd":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "call_A", Content: "a.go"},
		// reasoning-only — must be dropped
		{Role: "assistant", Content: "", ReasoningContent: "reasoning"},
		{Role: "assistant", Content: "Done."},
		{Role: "user", Content: "now show"},
		// old test types: []interface{} + map[string]interface{}
		{Role: "assistant", Content: "sure", ToolCalls: []interface{}{
			map[string]interface{}{"id": "call_B", "type": "function", "function": map[string]interface{}{"name": "cat", "arguments": `{"f":"a.go"}`}},
		}},
		{Role: "tool", ToolCallID: "call_B", Content: "package main"},
	}

	result := convertOpenAIMessagesToAnthropic(msgs)

	// Collect all tool_use ids seen so far (in order)
	seenToolUseIDs := map[string]bool{}

	for i, m := range result {
		role, _ := m["role"].(string)

		// Invariant 1: no empty-string content on simple string content
		if c, ok := m["content"].(string); ok && c == "" {
			t.Errorf("msg[%d] (role=%s) has empty string content", i, role)
		}

		switch c := m["content"].(type) {
		case []map[string]interface{}:
			if len(c) == 0 {
				t.Errorf("msg[%d] (role=%s) has empty content slice", i, role)
			}
			for _, block := range c {
				btype, _ := block["type"].(string)
				switch btype {
				case "tool_use":
					id, _ := block["id"].(string)
					if id == "" {
						t.Errorf("msg[%d] tool_use block has empty id", i)
					}
					seenToolUseIDs[id] = true
				case "tool_result":
					uid, _ := block["tool_use_id"].(string)
					if uid == "" {
						t.Errorf("msg[%d] tool_result block has empty tool_use_id", i)
					}
					// Invariant 2: every tool_result's tool_use_id must match a preceding tool_use
					if !seenToolUseIDs[uid] {
						t.Errorf("msg[%d] tool_result tool_use_id=%q has no preceding tool_use with that id", i, uid)
					}
				}
			}
		}
	}
}
