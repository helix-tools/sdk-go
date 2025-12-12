package producer

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

const testdataDir = "testdata"

// Helper to get testdata file path
func testdataPath(filename string) string {
	return filepath.Join(testdataDir, filename)
}

// Helper to check approximate equality for floats
func approxEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

// TestIsEmptyValue tests the isEmptyValue function
func TestIsEmptyValue(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		expected bool
	}{
		{"nil is empty", nil, true},
		{"empty string is empty", "", true},
		{"whitespace string is empty", "   ", true},
		{"tab newline is empty", "\t\n", true},
		{"empty slice is empty", []any{}, true},
		{"empty map is empty", map[string]any{}, true},
		{"non-empty string not empty", "hello", false},
		{"non-empty slice not empty", []any{1, 2, 3}, false},
		{"non-empty map not empty", map[string]any{"key": "value"}, false},
		{"zero not empty", float64(0), false},
		{"false not empty", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isEmptyValue(tt.value)
			if result != tt.expected {
				t.Errorf("isEmptyValue(%v) = %v, want %v", tt.value, result, tt.expected)
			}
		})
	}
}

// TestGetFieldStatus tests the getFieldStatus function
func TestGetFieldStatus(t *testing.T) {
	t.Run("flat object", func(t *testing.T) {
		obj := map[string]any{"name": "Alice", "age": float64(30)}
		allFields, present := getFieldStatus(obj, "")

		if !allFields["name"] || !allFields["age"] {
			t.Error("allFields should contain name and age")
		}
		if !present["name"] || !present["age"] {
			t.Error("present should contain name and age")
		}
	})

	t.Run("flat object with empty values", func(t *testing.T) {
		obj := map[string]any{"name": "Alice", "email": "", "phone": nil}
		allFields, present := getFieldStatus(obj, "")

		if !allFields["name"] || !allFields["email"] || !allFields["phone"] {
			t.Error("allFields should contain all fields")
		}
		if !present["name"] {
			t.Error("present should contain name")
		}
		if present["email"] || present["phone"] {
			t.Error("present should not contain email or phone (they are empty)")
		}
	})

	t.Run("nested object", func(t *testing.T) {
		obj := map[string]any{
			"user": map[string]any{
				"name": "Alice",
				"address": map[string]any{
					"city": "NYC",
				},
			},
		}
		allFields, _ := getFieldStatus(obj, "")

		expectedFields := []string{"user", "user.name", "user.address", "user.address.city"}
		for _, f := range expectedFields {
			if !allFields[f] {
				t.Errorf("allFields should contain %s", f)
			}
		}
	})

	t.Run("array of objects", func(t *testing.T) {
		obj := map[string]any{
			"items": []any{
				map[string]any{"id": float64(1), "name": "Item1"},
				map[string]any{"id": float64(2), "name": "Item2"},
			},
		}
		allFields, _ := getFieldStatus(obj, "")

		if !allFields["items"] {
			t.Error("allFields should contain items")
		}
		if !allFields["items[].id"] {
			t.Error("allFields should contain items[].id")
		}
		if !allFields["items[].name"] {
			t.Error("allFields should contain items[].name")
		}
	})

	t.Run("empty array", func(t *testing.T) {
		obj := map[string]any{"items": []any{}}
		allFields, present := getFieldStatus(obj, "")

		if !allFields["items"] {
			t.Error("allFields should contain items")
		}
		if present["items"] {
			t.Error("present should not contain items (empty array)")
		}
	})
}

// TestAnalyzeDataEmptyFile tests empty file handling
func TestAnalyzeDataEmptyFile(t *testing.T) {
	p := &Producer{}
	result, err := p.analyzeData(testdataPath("empty.ndjson"), DefaultAnalysisOptions())
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	if result.RecordCount != 0 {
		t.Errorf("RecordCount = %d, want 0", result.RecordCount)
	}
	if len(result.FieldEmptiness) != 0 {
		t.Errorf("FieldEmptiness should be empty, got %v", result.FieldEmptiness)
	}
	if len(result.Schema) != 0 {
		t.Errorf("Schema should be empty, got %v", result.Schema)
	}
	if result.AnalysisErrors != 0 {
		t.Errorf("AnalysisErrors = %d, want 0", result.AnalysisErrors)
	}
}

// TestAnalyzeDataSingleRecord tests single record analysis
func TestAnalyzeDataSingleRecord(t *testing.T) {
	p := &Producer{}
	result, err := p.analyzeData(testdataPath("single_record.ndjson"), DefaultAnalysisOptions())
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	if result.RecordCount != 1 {
		t.Errorf("RecordCount = %d, want 1", result.RecordCount)
	}
	if result.AnalysisErrors != 0 {
		t.Errorf("AnalysisErrors = %d, want 0", result.AnalysisErrors)
	}

	// All fields should be 0% empty
	for field, emptiness := range result.FieldEmptiness {
		if emptiness != 0.0 {
			t.Errorf("Field %s should be 0%% empty, got %.2f%%", field, emptiness)
		}
	}

	// Schema should have all fields
	schema := result.Schema
	if schema["type"] != "object" {
		t.Errorf("Schema type should be object, got %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("Schema should have properties")
	}
	for _, field := range []string{"id", "name", "active", "score"} {
		if _, exists := props[field]; !exists {
			t.Errorf("Schema should have field %s", field)
		}
	}
}

// TestAnalyzeDataMissingFields tests missing field handling
func TestAnalyzeDataMissingFields(t *testing.T) {
	p := &Producer{}
	result, err := p.analyzeData(testdataPath("missing_fields.ndjson"), DefaultAnalysisOptions())
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	if result.RecordCount != 3 {
		t.Errorf("RecordCount = %d, want 3", result.RecordCount)
	}

	emptiness := result.FieldEmptiness

	// name: present in all 3 records
	if emptiness["name"] != 0.0 {
		t.Errorf("name should be 0%% empty, got %.2f%%", emptiness["name"])
	}

	// email: present in 2/3 records, missing in 1
	if !approxEqual(emptiness["email"], 33.33, 0.01) {
		t.Errorf("email should be ~33.33%% empty, got %.2f%%", emptiness["email"])
	}

	// phone: present in 1/3 records, missing in 2
	if !approxEqual(emptiness["phone"], 66.67, 0.01) {
		t.Errorf("phone should be ~66.67%% empty, got %.2f%%", emptiness["phone"])
	}
}

// TestAnalyzeDataEmptyValues tests empty value detection
func TestAnalyzeDataEmptyValues(t *testing.T) {
	p := &Producer{}
	result, err := p.analyzeData(testdataPath("simple.ndjson"), DefaultAnalysisOptions())
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	if result.RecordCount != 3 {
		t.Errorf("RecordCount = %d, want 3", result.RecordCount)
	}

	emptiness := result.FieldEmptiness

	// name: all 3 have names
	if emptiness["name"] != 0.0 {
		t.Errorf("name should be 0%% empty, got %.2f%%", emptiness["name"])
	}

	// email: 1 empty (""), 2 have values
	if !approxEqual(emptiness["email"], 33.33, 0.01) {
		t.Errorf("email should be ~33.33%% empty, got %.2f%%", emptiness["email"])
	}

	// phone: 1 empty (""), 1 null, 1 has value = 66.67% empty
	if !approxEqual(emptiness["phone"], 66.67, 0.01) {
		t.Errorf("phone should be ~66.67%% empty, got %.2f%%", emptiness["phone"])
	}
}

// TestAnalyzeDataNestedObjects tests nested object analysis
func TestAnalyzeDataNestedObjects(t *testing.T) {
	p := &Producer{}
	result, err := p.analyzeData(testdataPath("nested.ndjson"), DefaultAnalysisOptions())
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	if result.RecordCount != 3 {
		t.Errorf("RecordCount = %d, want 3", result.RecordCount)
	}

	emptiness := result.FieldEmptiness

	// Check nested field paths exist
	expectedFields := []string{"user", "user.name", "user.address", "user.address.city", "user.address.zip"}
	for _, field := range expectedFields {
		if _, exists := emptiness[field]; !exists {
			t.Errorf("FieldEmptiness should contain %s", field)
		}
	}

	// user.name: all 3 have names
	if emptiness["user.name"] != 0.0 {
		t.Errorf("user.name should be 0%% empty, got %.2f%%", emptiness["user.name"])
	}

	// user.address.city: 1 empty (""), 2 have values
	if !approxEqual(emptiness["user.address.city"], 33.33, 0.01) {
		t.Errorf("user.address.city should be ~33.33%% empty, got %.2f%%", emptiness["user.address.city"])
	}

	// user.address.zip: 1 empty (""), 1 null, 1 has value
	if !approxEqual(emptiness["user.address.zip"], 66.67, 0.01) {
		t.Errorf("user.address.zip should be ~66.67%% empty, got %.2f%%", emptiness["user.address.zip"])
	}
}

// TestAnalyzeDataArrays tests arrays of objects
func TestAnalyzeDataArrays(t *testing.T) {
	p := &Producer{}
	result, err := p.analyzeData(testdataPath("arrays.ndjson"), DefaultAnalysisOptions())
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	if result.RecordCount != 3 {
		t.Errorf("RecordCount = %d, want 3", result.RecordCount)
	}

	emptiness := result.FieldEmptiness

	// items: 1 record has empty array = 33.33% empty
	if !approxEqual(emptiness["items"], 33.33, 0.01) {
		t.Errorf("items should be ~33.33%% empty, got %.2f%%", emptiness["items"])
	}
}

// TestAnalyzeDataSchemaSampling tests schema sampling behavior
func TestAnalyzeDataSchemaSampling(t *testing.T) {
	// Create a temp file with many records
	tmpFile, err := os.CreateTemp("", "test_*.ndjson")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// First record has field "a"
	tmpFile.WriteString(`{"a": 1}` + "\n")
	// Records 2-10 have field "b"
	for i := 0; i < 9; i++ {
		tmpFile.WriteString(`{"b": 2}` + "\n")
	}
	// Record 11+ has field "c" (beyond sample limit of 5)
	tmpFile.WriteString(`{"c": 3}` + "\n")
	tmpFile.Close()

	p := &Producer{}

	// With sample limit of 5, schema should include "a" and "b" but not "c"
	opts := AnalysisOptions{SchemaSampleLimit: 5}
	result, err := p.analyzeData(tmpFile.Name(), opts)
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	schema := result.Schema
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("Schema should have properties")
	}

	if _, exists := props["a"]; !exists {
		t.Error("Schema should have field 'a'")
	}
	if _, exists := props["b"]; !exists {
		t.Error("Schema should have field 'b'")
	}
	if _, exists := props["c"]; exists {
		t.Error("Schema should NOT have field 'c' (beyond sample limit)")
	}

	// But field_emptiness should have all fields (full scan)
	if _, exists := result.FieldEmptiness["a"]; !exists {
		t.Error("FieldEmptiness should have field 'a'")
	}
	if _, exists := result.FieldEmptiness["b"]; !exists {
		t.Error("FieldEmptiness should have field 'b'")
	}
	if _, exists := result.FieldEmptiness["c"]; !exists {
		t.Error("FieldEmptiness should have field 'c'")
	}
}

// TestAnalyzeDataMalformedJSON tests malformed JSON handling
func TestAnalyzeDataMalformedJSON(t *testing.T) {
	p := &Producer{}
	result, err := p.analyzeData(testdataPath("malformed.ndjson"), DefaultAnalysisOptions())
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	// 3 valid records, 2 invalid
	if result.RecordCount != 3 {
		t.Errorf("RecordCount = %d, want 3", result.RecordCount)
	}
	if result.AnalysisErrors != 2 {
		t.Errorf("AnalysisErrors = %d, want 2", result.AnalysisErrors)
	}

	// Valid records should still be analyzed
	emptiness := result.FieldEmptiness
	if _, exists := emptiness["valid"]; !exists {
		t.Error("FieldEmptiness should have field 'valid'")
	}
	if _, exists := emptiness["count"]; !exists {
		t.Error("FieldEmptiness should have field 'count'")
	}
}

// TestAnalyzeDataLargeFile tests memory efficiency with large files
func TestAnalyzeDataLargeFile(t *testing.T) {
	// Create a file with 10,000 records
	recordCount := 10000
	tmpFile, err := os.CreateTemp("", "test_large_*.ndjson")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	for i := 0; i < recordCount; i++ {
		email := ""
		if i%2 == 0 {
			email = "user" + string(rune('0'+i%10)) + "@example.com"
		}
		record := map[string]any{
			"id":    i,
			"name":  "User" + string(rune('0'+i%10)),
			"email": email,
			"score": float64(i) * 1.5,
		}
		data, _ := json.Marshal(record)
		tmpFile.Write(data)
		tmpFile.WriteString("\n")
	}
	tmpFile.Close()

	p := &Producer{}
	result, err := p.analyzeData(tmpFile.Name(), DefaultAnalysisOptions())
	if err != nil {
		t.Fatalf("analyzeData failed: %v", err)
	}

	if result.RecordCount != recordCount {
		t.Errorf("RecordCount = %d, want %d", result.RecordCount, recordCount)
	}
	if result.AnalysisErrors != 0 {
		t.Errorf("AnalysisErrors = %d, want 0", result.AnalysisErrors)
	}

	// email: 50% empty (every other record)
	if result.FieldEmptiness["email"] != 50.0 {
		t.Errorf("email should be 50%% empty, got %.2f%%", result.FieldEmptiness["email"])
	}
}

// TestInferType tests the inferType function
func TestInferType(t *testing.T) {
	tests := []struct {
		value    any
		expected string
	}{
		{nil, "null"},
		{true, "boolean"},
		{false, "boolean"},
		{float64(123), "number"},
		{"hello", "string"},
		{[]any{1, 2, 3}, "array"},
		{map[string]any{"key": "value"}, "object"},
	}

	for _, tt := range tests {
		result := inferType(tt.value)
		if result != tt.expected {
			t.Errorf("inferType(%v) = %s, want %s", tt.value, result, tt.expected)
		}
	}
}
