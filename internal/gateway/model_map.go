package gateway

import (
	"encoding/json"
	"os"
)

type ModelMapping struct {
	DisplayID string `json:"display_id"`
	CopilotID string `json:"copilot_id"`
}

type modelMapFile struct {
	Mappings []ModelMapping    `json:"mappings,omitempty"`
	Aliases  map[string]string `json:"aliases,omitempty"`
}

func LoadModelAliases(path string) (map[string]string, error) {
	if path == "" || path == ":memory:" {
		return map[string]string{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return map[string]string{}, nil
	}
	var asMap map[string]string
	if err := json.Unmarshal(data, &asMap); err == nil && asMap != nil {
		return sanitizeAliases(asMap), nil
	}
	var file modelMapFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	aliases := sanitizeAliases(file.Aliases)
	for _, mapping := range file.Mappings {
		if mapping.DisplayID != "" && mapping.CopilotID != "" {
			aliases[mapping.DisplayID] = mapping.CopilotID
		}
	}
	return aliases, nil
}

func SaveModelAliases(path string, aliases map[string]string) error {
	if path == "" || path == ":memory:" {
		return nil
	}
	clean := sanitizeAliases(aliases)
	mappings := make([]ModelMapping, 0, len(clean))
	for _, alias := range sortedKeys(clean) {
		mappings = append(mappings, ModelMapping{DisplayID: alias, CopilotID: clean[alias]})
	}
	data, err := json.MarshalIndent(modelMapFile{Mappings: mappings}, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func sanitizeAliases(aliases map[string]string) map[string]string {
	clean := map[string]string{}
	for alias, target := range aliases {
		alias = stringsTrim(alias)
		target = stringsTrim(target)
		if alias != "" && target != "" {
			clean[alias] = target
		}
	}
	return clean
}
