package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Invalid replayed IDs are removed rather than rewritten because a fabricated
// msg/fc/rs ID may point at a different upstream object.
//
// OpenAI Responses validates input item id prefixes by type:
//   - message        -> must begin with "msg"
//   - function_call* -> must begin with "fc"
//   - reasoning      -> must begin with "rs"
//
// Codex / multi-turn clients (and some chat→responses bridges) often replay
// history with a generic "item_*" id on every output item. Upstream then
// rejects with 400:
//
//	Invalid 'input[N].id': 'item_...'. Expected an ID that begins with 'rs'.
//
// Stripping the bad id keeps the item content (encrypted_content / summary /
// arguments) so multi-turn reasoning still works without the illegal lookup.
func shouldStripOpenAIResponsesInputItemID(itemType, id string) bool {
	if id == "" {
		return false
	}
	switch itemType {
	case "message":
		return !strings.HasPrefix(id, "msg")
	case "reasoning":
		return !strings.HasPrefix(id, "rs")
	default:
		if isCodexToolCallInputType(itemType) {
			return !strings.HasPrefix(id, "fc")
		}
		return false
	}
}

func sanitizeOpenAIResponsesInputItemIDs(body []byte) ([]byte, bool, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}

	items := make([][]byte, 0)
	changed := false
	var sanitizeErr error
	index := 0
	input.ForEach(func(_, item gjson.Result) bool {
		currentIndex := index
		index++
		itemBody := []byte(item.Raw)
		if item.IsObject() {
			itemType := item.Get("type")
			id := item.Get("id")
			if itemType.Type == gjson.String && id.Type == gjson.String &&
				shouldStripOpenAIResponsesInputItemID(itemType.String(), id.String()) {
				itemBody, sanitizeErr = sjson.DeleteBytes(itemBody, "id")
				if sanitizeErr != nil {
					sanitizeErr = fmt.Errorf("delete input.%d.id: %w", currentIndex, sanitizeErr)
					return false
				}
				changed = true
			}
		}
		items = append(items, itemBody)
		return true
	})
	if sanitizeErr != nil {
		return nil, false, sanitizeErr
	}
	if !changed {
		return body, false, nil
	}

	rebuiltInput := make([]byte, 0, len(input.Raw))
	rebuiltInput = append(rebuiltInput, '[')
	for i, item := range items {
		if i > 0 {
			rebuiltInput = append(rebuiltInput, ',')
		}
		rebuiltInput = append(rebuiltInput, item...)
	}
	rebuiltInput = append(rebuiltInput, ']')

	sanitized, err := sjson.SetRawBytes(body, "input", rebuiltInput)
	if err != nil {
		return nil, false, fmt.Errorf("replace sanitized input: %w", err)
	}
	return sanitized, true, nil
}
