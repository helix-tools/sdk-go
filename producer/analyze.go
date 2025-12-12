package producer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// AnalysisResult contains dataset analysis results.
type AnalysisResult struct {
	Schema         map[string]any     `json:"schema"`
	FieldEmptiness map[string]float64 `json:"field_emptiness"`
	RecordCount    int                `json:"record_count"`
	AnalysisErrors int                `json:"analysis_errors"`
}

// AnalysisOptions configures the analysis behavior.
type AnalysisOptions struct {
	SchemaSampleLimit int // Default: 1000, 0 = all records
}

// DefaultAnalysisOptions returns default analysis options.
func DefaultAnalysisOptions() AnalysisOptions {
	return AnalysisOptions{
		SchemaSampleLimit: 1000,
	}
}

// analyzeData analyzes an NDJSON file for schema and field emptiness.
//
// This method efficiently streams through the file to:
// 1. Infer JSON schema by sampling multiple records (default: first 1000)
// 2. Calculate the percentage of records where each field is missing or empty
//
// Memory efficiency is achieved by processing line-by-line rather than
// loading the entire file into memory.
func (p *Producer) analyzeData(filePath string, opts AnalysisOptions) (*AnalysisResult, error) {
	if opts.SchemaSampleLimit == 0 {
		opts.SchemaSampleLimit = 0 // 0 means all records
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var (
		allFields         = make(map[string]bool)
		fieldPresentCount = make(map[string]int)
		schemaBuilder     = newSchemaBuilder()
		recordCount       = 0
		analysisErrors    = 0
	)

	fmt.Println("📊 Analyzing dataset for schema and field statistics...")

	scanner := bufio.NewScanner(file)
	// Increase buffer size for large lines (default is 64KB)
	buf := make([]byte, 0, 1024*1024) // 1MB buffer
	scanner.Buffer(buf, 10*1024*1024) // 10MB max line size

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			analysisErrors++
			if analysisErrors <= 5 {
				fmt.Printf("  Warning: Failed to parse line %d: %v\n", lineNum, err)
			}
			continue
		}

		recordCount++

		// Infer schema from first N records for complete type coverage
		if opts.SchemaSampleLimit == 0 || recordCount <= opts.SchemaSampleLimit {
			schemaBuilder.addObject(record)
		}

		// Collect all fields and which are present/non-empty in this record
		discovered, present := getFieldStatus(record, "")

		// Track all discovered fields across all records
		for field := range discovered {
			allFields[field] = true
		}

		// Count records where each field is present and non-empty
		for field := range present {
			fieldPresentCount[field]++
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	// Calculate emptiness: % of records where field is missing OR empty
	fieldEmptiness := make(map[string]float64)
	for field := range allFields {
		present := fieldPresentCount[field]
		missingOrEmpty := recordCount - present
		var percentage float64
		if recordCount > 0 {
			percentage = float64(missingOrEmpty) / float64(recordCount) * 100
		}
		fieldEmptiness[field] = roundTo2Decimals(percentage)
	}

	// Sort by emptiness percentage (highest first)
	fieldEmptiness = sortByValueDesc(fieldEmptiness)

	// Build the final schema
	var schema map[string]any
	if recordCount > 0 {
		schema = schemaBuilder.toSchema()
	} else {
		schema = make(map[string]any)
	}

	// Print summary
	nonEmptyFields := 0
	partiallyEmpty := 0
	fullyEmpty := 0
	for _, v := range fieldEmptiness {
		if v == 0.0 {
			nonEmptyFields++
		} else if v == 100.0 {
			fullyEmpty++
		} else {
			partiallyEmpty++
		}
	}

	schemaCount := recordCount
	if opts.SchemaSampleLimit > 0 && recordCount > opts.SchemaSampleLimit {
		schemaCount = opts.SchemaSampleLimit
	}

	fmt.Printf("  Records analyzed: %d\n", recordCount)
	fmt.Printf("  Schema sampled from: %d records\n", schemaCount)
	fmt.Printf("  Fields discovered: %d\n", len(fieldEmptiness))
	fmt.Printf("    - Complete (0%% empty): %d\n", nonEmptyFields)
	fmt.Printf("    - Partial (1-99%% empty): %d\n", partiallyEmpty)
	fmt.Printf("    - Empty (100%% empty): %d\n", fullyEmpty)
	if analysisErrors > 0 {
		fmt.Printf("  Parse errors: %d\n", analysisErrors)
	}

	return &AnalysisResult{
		Schema:         schema,
		FieldEmptiness: fieldEmptiness,
		RecordCount:    recordCount,
		AnalysisErrors: analysisErrors,
	}, nil
}

// isEmptyValue checks if a value is considered empty.
// Empty values include: nil, empty string, empty slice, empty map,
// and strings containing only whitespace.
func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}

	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val) == ""
	case []any:
		return len(val) == 0
	case map[string]any:
		return len(val) == 0
	}

	return false
}

// getFieldStatus recursively collects field paths and their presence status.
// Handles nested objects using dot notation (e.g., "address.city").
//
// Returns:
//   - allFields: map of all discovered field paths
//   - presentFields: map of fields that have non-empty values
func getFieldStatus(obj map[string]any, prefix string) (allFields, presentFields map[string]bool) {
	allFields = make(map[string]bool)
	presentFields = make(map[string]bool)

	for key, value := range obj {
		var fieldPath string
		if prefix != "" {
			fieldPath = prefix + "." + key
		} else {
			fieldPath = key
		}

		// Always track discovered fields
		allFields[fieldPath] = true

		// Only count as present if value is non-empty
		if !isEmptyValue(value) {
			presentFields[fieldPath] = true

			// Recurse into nested objects
			if nestedMap, ok := value.(map[string]any); ok {
				nestedAll, nestedPresent := getFieldStatus(nestedMap, fieldPath)
				for f := range nestedAll {
					allFields[f] = true
				}
				for f := range nestedPresent {
					presentFields[f] = true
				}
			} else if arr, ok := value.([]any); ok && len(arr) > 0 {
				// For arrays of objects, analyze each item
				if firstItem, ok := arr[0].(map[string]any); ok {
					_ = firstItem // Type check passed
					for _, item := range arr {
						if itemMap, ok := item.(map[string]any); ok {
							nestedAll, nestedPresent := getFieldStatus(itemMap, fieldPath+"[]")
							for f := range nestedAll {
								allFields[f] = true
							}
							for f := range nestedPresent {
								presentFields[f] = true
							}
						}
					}
				}
			}
		}
	}

	return allFields, presentFields
}

// schemaBuilder builds a JSON schema from sample records.
type schemaBuilder struct {
	properties map[string]*propertySchema
}

type propertySchema struct {
	types      map[string]bool
	properties map[string]*propertySchema // For nested objects
	items      *propertySchema            // For arrays
}

func newSchemaBuilder() *schemaBuilder {
	return &schemaBuilder{
		properties: make(map[string]*propertySchema),
	}
}

func (sb *schemaBuilder) addObject(obj map[string]any) {
	sb.addProperties(obj, sb.properties)
}

func (sb *schemaBuilder) addProperties(obj map[string]any, props map[string]*propertySchema) {
	for key, value := range obj {
		if _, exists := props[key]; !exists {
			props[key] = &propertySchema{
				types:      make(map[string]bool),
				properties: make(map[string]*propertySchema),
			}
		}

		prop := props[key]
		prop.types[inferType(value)] = true

		// Handle nested objects
		if nestedMap, ok := value.(map[string]any); ok {
			sb.addProperties(nestedMap, prop.properties)
		}

		// Handle arrays
		if arr, ok := value.([]any); ok && len(arr) > 0 {
			if prop.items == nil {
				prop.items = &propertySchema{
					types:      make(map[string]bool),
					properties: make(map[string]*propertySchema),
				}
			}
			for _, item := range arr {
				prop.items.types[inferType(item)] = true
				if itemMap, ok := item.(map[string]any); ok {
					sb.addProperties(itemMap, prop.items.properties)
				}
			}
		}
	}
}

func (sb *schemaBuilder) toSchema() map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": sb.propertiesToSchema(sb.properties),
	}
	return schema
}

func (sb *schemaBuilder) propertiesToSchema(props map[string]*propertySchema) map[string]any {
	result := make(map[string]any)

	for name, prop := range props {
		propSchema := make(map[string]any)

		// Get types
		types := make([]string, 0, len(prop.types))
		for t := range prop.types {
			types = append(types, t)
		}
		sort.Strings(types)

		if len(types) == 1 {
			propSchema["type"] = types[0]
		} else if len(types) > 1 {
			propSchema["type"] = types
		}

		// Handle nested object properties
		if len(prop.properties) > 0 {
			propSchema["properties"] = sb.propertiesToSchema(prop.properties)
		}

		// Handle array items
		if prop.items != nil {
			itemSchema := make(map[string]any)
			itemTypes := make([]string, 0, len(prop.items.types))
			for t := range prop.items.types {
				itemTypes = append(itemTypes, t)
			}
			sort.Strings(itemTypes)

			if len(itemTypes) == 1 {
				itemSchema["type"] = itemTypes[0]
			} else if len(itemTypes) > 1 {
				itemSchema["type"] = itemTypes
			}

			if len(prop.items.properties) > 0 {
				itemSchema["properties"] = sb.propertiesToSchema(prop.items.properties)
			}

			propSchema["items"] = itemSchema
		}

		result[name] = propSchema
	}

	return result
}

// inferType returns the JSON schema type for a value.
func inferType(v any) string {
	if v == nil {
		return "null"
	}

	switch v.(type) {
	case bool:
		return "boolean"
	case float64:
		return "number"
	case int, int64:
		return "integer"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// roundTo2Decimals rounds a float64 to 2 decimal places.
func roundTo2Decimals(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

// sortByValueDesc returns a new map sorted by value in descending order.
// Note: Go maps are unordered, so this returns a new map for consistency.
func sortByValueDesc(m map[string]float64) map[string]float64 {
	// Create a sorted slice of keys
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Slice(keys, func(i, j int) bool {
		// Sort by value descending, then by key ascending for stability
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})

	// Create a new map (note: iteration order is still undefined in Go)
	result := make(map[string]float64, len(m))
	for _, k := range keys {
		result[k] = m[k]
	}

	return result
}
