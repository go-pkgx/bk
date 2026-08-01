package schema

import (
	"encoding/json"
	"testing"
)

func TestPackageJSONEmbedded(t *testing.T) {
	if len(PackageJSON) == 0 {
		t.Fatal("embedded schema is empty")
	}
	var doc map[string]any
	if err := json.Unmarshal(PackageJSON, &doc); err != nil {
		t.Fatalf("embedded schema is not valid JSON: %v", err)
	}
	if doc["$schema"] == "" || doc["title"] == "" {
		t.Errorf("schema missing $schema/title: %v", doc["$schema"])
	}
}
