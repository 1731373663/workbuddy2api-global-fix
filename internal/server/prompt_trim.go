package server

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
)

const (
	promptTrimMaxRetries  = 2
	promptTrimSafetyToken = 4096
	promptTrimDefaultSize = 65536
)

var promptTooLongTokenRE = regexp.MustCompile("(?i)prompt is too long:\\s*([0-9]+)\\s*tokens?\\s*>\\s*([0-9]+)\\s*maximum")

func trimChatPrompt(body []byte, upstreamError string) ([]byte, bool) {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, false
	}
	rawMessages, ok := obj["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, false
	}
	systemEnd := 0
	for systemEnd < len(rawMessages) {
		role := messageRole(rawMessages[systemEnd])
		if role != "system" && role != "developer" {
			break
		}
		systemEnd++
	}
	userIndexes := make([]int, 0, len(rawMessages))
	for i := systemEnd; i < len(rawMessages); i++ {
		if messageRole(rawMessages[i]) == "user" {
			userIndexes = append(userIndexes, i)
		}
	}
	if len(userIndexes) < 2 {
		return nil, false
	}
	required := promptTrimRequirement(upstreamError)
	var removedTokens int64
	for keepFromUser := 1; keepFromUser < len(userIndexes); keepFromUser++ {
		keepFrom := userIndexes[keepFromUser]
		removedTokens += estimateMessagesTokens(rawMessages[systemEnd:keepFrom])
		if removedTokens < required {
			continue
		}
		trimmed := make([]any, 0, systemEnd+len(rawMessages)-keepFrom)
		trimmed = append(trimmed, rawMessages[:systemEnd]...)
		trimmed = append(trimmed, rawMessages[keepFrom:]...)
		obj["messages"] = trimmed
		out, err := json.Marshal(obj)
		if err != nil {
			return nil, false
		}
		return out, true
	}
	return nil, false
}

func promptTrimRequirement(upstreamError string) int64 {
	m := promptTooLongTokenRE.FindStringSubmatch(upstreamError)
	if len(m) != 3 {
		return promptTrimDefaultSize
	}
	actual, errActual := strconv.ParseInt(m[1], 10, 64)
	maximum, errMaximum := strconv.ParseInt(m[2], 10, 64)
	if errActual != nil || errMaximum != nil || actual <= maximum {
		return promptTrimDefaultSize
	}
	return actual - maximum + promptTrimSafetyToken
}

func messageRole(v any) string {
	msg, _ := v.(map[string]any)
	if msg == nil {
		return ""
	}
	role, _ := msg["role"].(string)
	return role
}

func estimateMessagesTokens(messages []any) int64 {
	var total int64
	for _, msg := range messages {
		raw, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		total += int64(len(raw)/4 + 8)
	}
	return total
}
