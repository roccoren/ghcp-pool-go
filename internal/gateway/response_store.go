package gateway

import (
	"encoding/json"
	"sync"
)

type StoredResponse struct {
	Response   map[string]any
	InputItems []any
}

type ResponseStore struct {
	mu    sync.Mutex
	items map[string]StoredResponse
}

func NewResponseStore() *ResponseStore {
	return &ResponseStore{items: map[string]StoredResponse{}}
}

func (s *ResponseStore) Store(response map[string]any, inputItems []any) {
	id := stringFromAny(response["id"])
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = StoredResponse{
		Response:   cloneJSONMap(response),
		InputItems: cloneJSONSlice(inputItems),
	}
}

func (s *ResponseStore) Get(id string) (StoredResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.items[id]
	if !ok {
		return StoredResponse{}, false
	}
	return StoredResponse{Response: cloneJSONMap(rec.Response), InputItems: cloneJSONSlice(rec.InputItems)}, true
}

func (s *ResponseStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return false
	}
	delete(s.items, id)
	return true
}

func (s *ResponseStore) Update(id string, mutate func(map[string]any) map[string]any) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.items[id]
	if !ok {
		return nil, false
	}
	updated := mutate(cloneJSONMap(rec.Response))
	rec.Response = cloneJSONMap(updated)
	s.items[id] = rec
	return cloneJSONMap(updated), true
}

func cloneJSONMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	var out map[string]any
	data, err := json.Marshal(m)
	if err != nil || json.Unmarshal(data, &out) != nil {
		return cloneMap(m)
	}
	return out
}

func cloneJSONSlice(values []any) []any {
	if values == nil {
		return nil
	}
	var out []any
	data, err := json.Marshal(values)
	if err != nil || json.Unmarshal(data, &out) != nil {
		out = make([]any, len(values))
		copy(out, values)
		return out
	}
	return out
}

func responseInputItemsFromRaw(raw map[string]any) []any {
	input := raw["input"]
	switch v := input.(type) {
	case string:
		return []any{map[string]any{
			"id":      newID("msg"),
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": v}},
		}}
	case []any:
		out := cloneJSONSlice(v)
		for i, item := range out {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if stringFromAny(obj["id"]) == "" {
				obj["id"] = newID("item")
				out[i] = obj
			}
		}
		return out
	default:
		return nil
	}
}
